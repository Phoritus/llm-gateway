package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

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
	deadline := time.Now().Add(d.Timeout)
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
	req, bodyBytes, ok := decodeChatRequest(w, r)
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
	reserved, ok := d.reserve(w, r, key, estimateTokens(bodyBytes))
	if !ok {
		return
	}
	var cost spent
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

	res, err := d.Router.Run(ctx, route, d.attempt(req))
	if err != nil && clientGone(ctx) {
		return
	}
	d.logAttempts(r, res.Steps)
	setFallbackHeaders(w.Header(), res)
	if err != nil {
		d.writeUpstreamError(w, r, withDeadline(ctx, err))
		return
	}
	stream, first := res.Stream, res.First
	defer stream.Close()
	cost.observe(first)

	if req.Stream {
		wantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		d.streamResponse(w, r, stream, first, &cost, wantsUsage)
		return
	}

	var a provider.Assembler
	a.Add(first)
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Nothing has been written yet, so this is still a status.
			d.failBeforeFirstByte(w, r, err)
			return
		}
		cost.observe(chunk)
		a.Add(chunk)
	}
	writeJSON(w, http.StatusOK, a.Response())
}

// attempt is one call to one target: the stream opened and its first chunk
// read, which is as far as failover can still happen.
func (d Deps) attempt(req *provider.ChatRequest) router.Attempt {
	return func(ctx context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
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

		stream, err := adapter.ChatStream(ctx, &upstream, provider.Credential{APIKey: t.Secret})
		if err != nil {
			return nil, nil, err
		}
		first, err := stream.Next()
		if errors.Is(err, io.EOF) {
			err = &provider.Error{Kind: provider.ErrUpstream, Detail: "stream ended before its first chunk"}
		}
		return stream, first, err
	}
}

// setFallbackHeaders tells the client which targets its request went
// through, as vendor/model=outcome in the order they were considered.
func setFallbackHeaders(h http.Header, res router.Result) {
	trail := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		trail[i] = string(s.Vendor) + "/" + headerSafe(s.Model) + "=" + string(s.Outcome)
	}
	h.Set("X-Fallback-Attempts", strconv.Itoa(res.Attempts()))
	h.Set("X-Fallback-Trail", strings.Join(trail, ", "))
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
func (d Deps) streamResponse(w http.ResponseWriter, r *http.Request, stream provider.Stream, first *provider.ChatChunk, cost *spent, wantsUsage bool) {
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
		return
	}
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			send([]byte("[DONE]"))
			return
		}
		if err != nil {
			if clientGone(ctx) {
				return
			}
			err = withDeadline(ctx, err)
			d.logUpstream(r, err)
			_, detail := upstreamError(err)
			b, _ := json.Marshal(errorBody{Error: detail})
			send(b)
			return
		}
		cost.observe(chunk)
		if !sendChunk(chunk) {
			return
		}
	}
}

// failBeforeFirstByte answers an upstream failure while a status line can
// still be sent. A client that has gone away gets nothing: there is no one to
// read it.
func (d Deps) failBeforeFirstByte(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(r.Context()) {
		return
	}
	d.writeUpstreamError(w, r, withDeadline(r.Context(), err))
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
// refusing them would break every SDK. The body's length is returned with
// the request, as the basis of the token estimate.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*provider.ChatRequest, int, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, "body_too_large",
				"The request body is too large.")
			return nil, 0, false
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// Without this, net/http reads the rest of the promised body before
			// sending the answer, which is the wait the deadline just ended.
			w.Header().Set("Connection", "close")
			writeError(w, http.StatusRequestTimeout, typeInvalidRequest, "request_timeout",
				"The request body was not received in time.")
			return nil, 0, false
		}
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_body",
			"The request body could not be read.")
		return nil, 0, false
	}

	var req provider.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_json",
			"The request body must be a JSON object in the chat completions format.")
		return nil, 0, false
	}
	if strings.TrimSpace(req.Model) == "" {
		invalidField(w, "model", "model is required.")
		return nil, 0, false
	}
	if len(req.Messages) == 0 {
		invalidField(w, "messages", "messages must contain at least one message.")
		return nil, 0, false
	}
	for _, m := range req.Messages {
		if !knownRoles[m.Role] {
			invalidField(w, "messages",
				"Every message needs a role of system, developer, user, assistant or tool.")
			return nil, 0, false
		}
		if !contentShapeOK(m.Content) {
			invalidField(w, "messages", "A message's content must be a string, an array of parts, or null.")
			return nil, 0, false
		}
	}
	return &req, len(body), true
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
