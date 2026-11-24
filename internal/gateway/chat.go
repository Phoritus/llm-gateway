package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/budget"
	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/store"
)

// errDeadline marks the request's own deadline passing, as opposed to the
// client going away. The first is an upstream that took too long and is
// answered; the second has nobody left to answer.
var errDeadline = errors.New("request deadline exceeded")

// writeGrace is how long past the deadline a write may still take, which is
// long enough to send the timeout error and short enough that a client that
// stopped reading lets go of the request soon after.
const writeGrace = 2 * time.Second

// handleChatCompletions is POST /v1/chat/completions.
//
// The point of no return is the first chunk, not the call. The upstream's
// first chunk is read before any header is written, so an upstream that
// accepts the request and then fails at once can still be failed over from,
// and when every target fails is still answered with a status. After that
// byte, a failure can only end the stream with an error event.
//
// The deadline is also put on the socket. A context only stops code that looks
// at it, and a handler blocked reading a body the client is dripping, or
// writing to a client that stopped reading, looks at nothing.
func (d Deps) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	obs := observation{start: time.Now()}
	deadline := obs.start.Add(d.Timeout)
	var (
		key  store.ActiveGatewayKey
		cost spent
	)
	defer func() { d.finish(r.Context(), &obs, key, &cost) }()

	rc := http.NewResponseController(w)
	ctx, cancel := context.WithDeadlineCause(r.Context(), deadline, errDeadline)
	defer cancel()
	r = r.WithContext(ctx)

	// The read deadline covers the body only. Left in place, it would fire in
	// net/http's background read that watches for a disconnect, and the
	// request would look abandoned instead of timed out. It is set before
	// authenticating because refusing a request without reading its body
	// makes net/http drain the body before it writes the refusal.
	_ = rc.SetReadDeadline(deadline)
	key, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	req, body, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	// Writes get a grace period past the deadline so the timeout itself can
	// still be told to the client. net/http clears the deadline after the
	// request, so it does not carry over to the next one on the connection.
	_ = rc.SetWriteDeadline(deadline.Add(writeGrace))

	// The slot is taken before the buckets so that a full workspace is turned
	// away with nothing reserved, and so nothing to give back.
	release, ok := d.inflight.acquire(key.WorkspaceID, d.Limits.WorkspaceConcurrency)
	if !ok {
		tooManyConcurrentRequests(w)
		return
	}
	defer release()

	// The limits come before the route is resolved: resolving it is a
	// Postgres query, and a key past its limit should not get to make one.
	estimate := estimateTokens(len(body))
	reserved, ok := d.reserve(w, r, key, estimate)
	if !ok {
		return
	}
	defer func() { d.settle(ctx, reserved, &cost) }()

	route, err := d.Routes.ResolveRoute(ctx, key.WorkspaceID, req.Model)
	if errors.Is(err, store.ErrNotFound) {
		param := "model"
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{
			Message: "The model `" + req.Model + "` does not exist. Create a route with this alias first.",
			Type:    typeInvalidRequest, Param: &param, Code: "model_not_found",
		}})
		return
	}
	if err != nil {
		d.serverError(w, r, err, "resolve route")
		return
	}
	obs.routed, obs.alias = true, req.Model
	obs.budgets, obs.estimate = budgetAccounts(key, route), estimate
	if route.Settings.PromptLogging {
		obs.prompt = body
	}

	var (
		stream provider.Stream
		first  *provider.ChatChunk
		// charged is where the chunks' usage is counted as this request's cost.
		charged = &cost
	)
	cached := d.lookupCache(r, key.WorkspaceID, route, req)
	if cached.hit == nil {
		if cached.key != "" {
			w.Header().Set("X-Cache", "MISS")
		}
		targets, ok := d.budgetTargets(w, r, obs.budgets, route.Targets)
		if !ok {
			if cached.missed {
				countCache(cacheMiss)
			}
			obs.status, obs.outcome = http.StatusTooManyRequests, store.OutcomeBudgetExceeded
			return
		}
		route.Targets = targets
		d.lookupSemantic(ctx, key.WorkspaceID, route, req, &cached)
	}
	if cached.hit != nil {
		// The stored answer is sent as a stream, by the code that sends a live
		// one. The usage it carries was spent by the request that stored it,
		// so it is counted nowhere: cost stays empty, which refunds the whole
		// token reservation and records no tokens.
		stream, charged = cache.Replay(&cached.hit.Response), &spent{}
		first, _ = stream.Next()
		obs.cached = true
		obs.target = store.ResolvedTarget{Vendor: cached.hit.Vendor, Model: cached.hit.Model}
		w.Header().Set("X-Cache", "HIT")
		setFallbackHeaders(w.Header(), router.Result{})
		if d.Budget != nil && anyBudget(obs.budgets) {
			w.Header().Set(headerCostBasis, string(budget.Cached))
		}
	} else {
		if route.FreeFirst && d.Quota != nil {
			route.Targets = d.Quota.Prioritize(ctx, route.Targets, estimate)
		}
		if !d.ContextHandoff {
			route.ContextHandoff = store.HandoffOff
		}
		res, err := d.Router.Run(ctx, route, req, d.attempt)
		obs.attempts, obs.handoff = res.Attempts(), res.Handoff
		countHandoff(ctx, res.Handoff)
		if err != nil && clientGone(ctx) {
			obs.status, obs.outcome = statusClientClosed, store.OutcomeClientGone
			return
		}
		d.logAttempts(r, res.Steps)
		setFallbackHeaders(w.Header(), res)
		if err != nil {
			obs.failure = withDeadline(ctx, err)
			obs.status, obs.outcome = d.writeUpstreamError(w, r, obs.failure)
			return
		}
		stream, first, obs.target = res.Stream, res.First, res.Target
		if d.Budget != nil && anyBudget(obs.budgets) {
			w.Header().Set(headerCostBasis, string(d.Budget.Charge(res.Target, 0, 0).Basis))
		}
		defer func() { d.spendFree(ctx, res.Target, &cost, estimate) }()
	}
	defer stream.Close()
	charged.observe(first)
	obs.answered, obs.firstChunk = true, time.Now()
	obs.status, obs.outcome = http.StatusOK, store.OutcomeOK

	whole := &assembled{Stream: stream}
	whole.answer.Add(first)
	obs.answer = whole
	if req.Stream {
		wantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		obs.outcome = d.streamResponse(w, r, whole, first, charged, wantsUsage)
	} else {
		for {
			chunk, err := whole.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				// Nothing has been written yet, so this is still a status.
				obs.failure = withDeadline(ctx, err)
				obs.status, obs.outcome = d.failBeforeFirstByte(w, r, err)
				return
			}
			charged.observe(chunk)
		}
		writeJSON(w, http.StatusOK, whole.answer.Response())
	}
	if !obs.cached && whole.complete && !obs.handoff.Fitted.Changed() {
		d.storeAnswer(ctx, key.WorkspaceID, cached, obs.target, whole.answer.Response())
	}
}

// attempt is one call to one target: the stream opened and its first chunk
// read, which is as far as failover can still happen.
func (d Deps) attempt(parent context.Context, t store.ResolvedTarget, req *provider.ChatRequest) (provider.Stream, *provider.ChatChunk, error) {
	adapter, ok := d.Providers[t.Vendor]
	if !ok {
		return nil, nil, router.ErrNoAdapter
	}
	upstream := *req
	upstream.Model = t.Model
	// Usage is always asked for, whatever the client asked: it is what the
	// token reservation is reconciled against, and what a non-streaming
	// answer reports.
	upstream.StreamOptions = &provider.StreamOptions{IncludeUsage: true}

	wait := router.FirstByteTimeout(t)
	ctx, cancel, stopWaiting := cancelAfterUnlessStopped(parent, wait)
	stream, first, err := firstChunk(ctx, adapter, &upstream, t)
	if timedOut := !stopWaiting(); timedOut && parent.Err() == nil {
		err = &provider.Error{Kind: provider.ErrTimeout, Detail: "no first chunk within " + wait.String()}
	}
	if err != nil {
		cancel()
		return stream, nil, err
	}
	return cancelOnClose{Stream: stream, cancel: cancel}, first, nil
}

// cancelAfterUnlessStopped is a context deadline that can be called off:
// stop reports whether it was stopped before it fired.
func cancelAfterUnlessStopped(parent context.Context, d time.Duration) (ctx context.Context, cancel context.CancelFunc, stop func() bool) {
	ctx, cancel = context.WithCancel(parent)
	return ctx, cancel, time.AfterFunc(d, cancel).Stop
}

func firstChunk(ctx context.Context, adapter provider.Provider, req *provider.ChatRequest, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
	stream, err := adapter.ChatStream(ctx, req, provider.Credential{APIKey: t.Secret, BaseURL: t.BaseURL})
	if err != nil {
		return nil, nil, err
	}
	first, err := stream.Next()
	if errors.Is(err, io.EOF) {
		err = &provider.Error{Kind: provider.ErrUpstream, Detail: "stream ended before its first chunk"}
	}
	return stream, first, err
}

type cancelOnClose struct {
	provider.Stream
	cancel context.CancelFunc
}

func (s cancelOnClose) Close() error {
	err := s.Stream.Close()
	s.cancel()
	return err
}

// setFallbackHeaders tells the client which targets its request went
// through, as vendor/model=outcome in the order they were considered, with
// +trimmed on a target sent a fitted conversation, and what context handoff
// did to the request.
func setFallbackHeaders(h http.Header, res router.Result) {
	trail := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		trail[i] = string(s.Vendor) + "/" + headerSafe(s.Model) + "=" + string(s.Outcome)
		if s.Trimmed {
			trail[i] += "+trimmed"
		}
	}
	h.Set("X-Fallback-Attempts", strconv.Itoa(res.Attempts()))
	h.Set("X-Fallback-Trail", strings.Join(trail, ", "))
	if v := handoffHeader(res.Handoff); v != "" {
		h.Set("X-Context-Handoff", v)
	}
}

// handoffHeader is X-Context-Handoff: the action, then for a trimmed request
// how much was cut, in counts only.
func handoffHeader(h router.Handoff) string {
	switch a := h.Action(); {
	case a == store.HandoffTrimmed:
		return fmt.Sprintf("%s; omitted=%d; cleared_tool_results=%d", a, h.Fitted.OmittedMessages, h.Fitted.ClearedToolResults)
	case a != "":
		return string(a)
	case h.Unfittable:
		return handoffUnfittable
	}
	return ""
}

// headerSafe drops what a header value cannot carry. A model name is
// whatever the workspace typed into its route.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return -1
		}
		return r
	}, s)
}

// streamResponse sends the chunks as server-sent events, flushing each one.
//
// The upstream is always asked for usage. A client that did not ask is shown
// the stream it would have had without it: no usage-only chunk, and no usage
// field on any other.
//
// It returns how the stream ended. The status was sent with the first chunk,
// so a stream that fails afterwards is still a 200 and only the outcome says so.
func (d Deps) streamResponse(w http.ResponseWriter, r *http.Request, stream provider.Stream, first *provider.ChatChunk, cost *spent, wantsUsage bool) store.Outcome {
	ctx := r.Context()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Stops a buffering reverse proxy from holding chunks back.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	send := func(payload []byte) bool {
		if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	sendChunk := func(c *provider.ChatChunk) bool {
		if c.Usage != nil && !wantsUsage {
			if len(c.Choices) == 0 {
				return true
			}
			withoutUsage := *c
			withoutUsage.Usage = nil
			c = &withoutUsage
		}
		b, err := json.Marshal(c)
		return err == nil && send(b)
	}

	if !sendChunk(first) {
		return store.OutcomeClientGone
	}
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			send([]byte("[DONE]"))
			return store.OutcomeOK
		}
		if err != nil {
			if clientGone(ctx) {
				return store.OutcomeClientGone
			}
			err = withDeadline(ctx, err)
			d.logUpstream(r, err)
			_, detail, _ := upstreamError(err)
			b, _ := json.Marshal(errorBody{Error: detail})
			send(b)
			return store.OutcomeStreamError
		}
		cost.observe(chunk)
		if !sendChunk(chunk) {
			return store.OutcomeClientGone
		}
	}
}

// failBeforeFirstByte answers an upstream failure while a status line can
// still be sent. A client that has gone away gets nothing: there is no one to
// read it.
func (d Deps) failBeforeFirstByte(w http.ResponseWriter, r *http.Request, err error) (int, store.Outcome) {
	if clientGone(r.Context()) {
		return statusClientClosed, store.OutcomeClientGone
	}
	return d.writeUpstreamError(w, r, withDeadline(r.Context(), err))
}

// clientGone reports that the request was cancelled for a reason other than
// its own deadline -- the client disconnected.
func clientGone(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(context.Cause(ctx), errDeadline)
}

// withDeadline replaces a bare context error with errDeadline when the
// request's own deadline is what ended it, so it is answered as a timeout.
func withDeadline(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errDeadline) && errors.Is(err, context.DeadlineExceeded) {
		return errDeadline
	}
	return err
}

// decodeChatRequest reads and validates the body. Unknown fields are
// accepted: OpenAI clients send many this gateway has no use for, and
// refusing them would break every SDK.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*provider.ChatRequest, []byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, "body_too_large",
				"The request body is too large.")
			return nil, nil, false
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// Without this, net/http reads the rest of the promised body before
			// sending the answer, which is the wait the deadline just ended.
			w.Header().Set("Connection", "close")
			writeError(w, http.StatusRequestTimeout, typeInvalidRequest, "request_timeout",
				"The request body was not received in time.")
			return nil, nil, false
		}
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_body",
			"The request body could not be read.")
		return nil, nil, false
	}

	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_json",
			"The request body must be a JSON object in the chat completions format.")
		return nil, nil, false
	}
	if strings.TrimSpace(req.Model) == "" {
		invalidField(w, "model", "model is required.")
		return nil, nil, false
	}
	if len(req.Messages) == 0 {
		invalidField(w, "messages", "messages must contain at least one message.")
		return nil, nil, false
	}
	for _, m := range req.Messages {
		if !knownRoles[m.Role] {
			invalidField(w, "messages",
				"Every message needs a role of system, developer, user, assistant or tool.")
			return nil, nil, false
		}
		if !contentShapeOK(m.Content) {
			invalidField(w, "messages", "A message's content must be a string, an array of parts, or null.")
			return nil, nil, false
		}
	}
	return &req, body, true
}

// knownRoles are the roles OpenAI's chat format defines. "function" is the
// deprecated predecessor of "tool" and still sent by older clients.
var knownRoles = map[string]bool{
	"system": true, "developer": true, "user": true, "assistant": true, "tool": true, "function": true,
}

// contentShapeOK checks content's JSON type only. What is inside the parts is
// the upstream's to judge: the part types it accepts differ by provider.
func contentShapeOK(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return true
	}
	switch b[0] {
	case '"', '[', 'n':
		return true
	}
	return false
}
