package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"llm-gateway/internal/budget"
	"llm-gateway/internal/promptlog"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/store"
	"llm-gateway/internal/usage"
)

// statusClientClosed is what a request is recorded as when the client left
// before any status was sent. It is nginx's code for the same thing, and never
// reaches a client.
const statusClientClosed = 499

// The labels below come from fixed sets only: a vendor is one of
// provider.Vendor, noVendor or vendorCache, and an outcome is a store.Outcome
// constant. Never the route alias, the workspace or the key: each of those is
// a new metric series per tenant.
const (
	noVendor                     = "none"
	outcomeRefused store.Outcome = "refused"
)

var (
	meter = otel.Meter("llm-gateway/gateway")

	requestsTotal, _ = meter.Int64Counter("gateway.requests",
		metric.WithDescription("Requests to /v1/chat/completions, by answering vendor and outcome."))
	requestDuration, _ = meter.Float64Histogram("gateway.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Time from request to the end of its response."))
	timeToFirstToken, _ = meter.Float64Histogram("gateway.time_to_first_token",
		metric.WithUnit("s"), metric.WithDescription("Time from request to the first chunk from the upstream."))
	fallbackAttempts, _ = meter.Int64Histogram("gateway.fallback.attempts",
		metric.WithDescription("Targets called for one request."))
	contextHandoff, _ = meter.Int64Counter("gateway.context_handoff",
		metric.WithDescription("Requests context handoff changed, by action: reordered, trimmed or unfittable."))
)

const handoffUnfittable = "unfittable"

func countHandoff(ctx context.Context, h router.Handoff) {
	var actions []string
	if a := h.Action(); a != "" {
		actions = append(actions, string(a))
	}
	if h.Unfittable {
		actions = append(actions, handoffUnfittable)
	}
	for _, a := range actions {
		contextHandoff.Add(ctx, 1, metric.WithAttributes(attribute.String("action", a)))
	}
}

// UsageRecorder takes an event without waiting. *usage.Recorder is the
// production one.
type UsageRecorder interface {
	Record(usage.Event)
}

// PromptLogger takes a prompt log without waiting.
type PromptLogger interface {
	Record(store.RequestLog)
}

// observation is what one request amounts to for usage history and metrics,
// filled in by the handler as it goes and read once, when it ends.
type observation struct {
	start time.Time
	// routed is set once the model resolved to a route. A request refused
	// before that counts in the metrics but leaves no usage event.
	routed   bool
	alias    string
	attempts int
	// answered is set when a target produced a first chunk, and target and
	// firstChunk are then valid.
	answered bool
	// cached is set when the exact cache answered. target is then the target
	// that produced the stored answer, and no target was called.
	cached     bool
	handoff    router.Handoff
	target     store.ResolvedTarget
	firstChunk time.Time
	// status is what the client was told, and outcome the fixed name of how
	// the request ended.
	status  int
	outcome store.Outcome
	prompt  []byte
	answer  *assembled
	failure error
	// budgets are the accounts the request is charged to, and estimate its
	// prompt's estimated tokens, priced when the upstream reports no usage.
	budgets  []budget.Account
	estimate float64
}

// response is what the client was sent, with an answer's text cut to what a
// log keeps before it is encoded, and whether it was cut.
func (o *observation) response() ([]byte, bool) {
	var (
		body any
		cut  bool
	)
	switch {
	case o.answer != nil && o.status == http.StatusOK:
		body, cut = capAnswer(o.answer.answer.Response(), promptlog.MaxBodyBytes)
	case o.failure != nil && o.status != statusClientClosed:
		_, detail, _ := upstreamError(o.failure)
		body = errorBody{Error: detail}
	default:
		return nil, false
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), cut
}

// capAnswer cuts the answer's text so that, all together, it holds at most
// budget bytes.
func capAnswer(r *provider.ChatResponse, budget int) (*provider.ChatResponse, bool) {
	out := *r
	out.Choices = slices.Clone(r.Choices)
	cut := false
	take := func(s string) string {
		if len(s) > budget {
			s, cut = s[:runeStart(s, budget)], true
		}
		budget -= len(s)
		return s
	}
	for i := range out.Choices {
		m := &out.Choices[i].Message
		m.Content = take(m.Content)
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for j := range m.ToolCalls {
			m.ToolCalls[j].Function.Arguments = take(m.ToolCalls[j].Function.Arguments)
		}
	}
	return &out, cut
}

func runeStart(s string, n int) int {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// finish records the request. It runs last, after the response is written,
// and nothing in it waits on anything. requestCtx carries the request id to
// the work it detaches.
func (d Deps) finish(requestCtx context.Context, o *observation, key store.ActiveGatewayKey, cost *spent) {
	vendor, outcome := noVendor, o.outcome
	if o.answered {
		vendor = string(o.target.Vendor)
	}
	if o.cached {
		vendor = vendorCache
	}
	if !o.routed {
		outcome = outcomeRefused
	}
	elapsed := time.Since(o.start)
	ctx := context.Background()
	both := metric.WithAttributes(attribute.String("vendor", vendor), attribute.String("outcome", string(outcome)))

	requestsTotal.Add(ctx, 1, both)
	requestDuration.Record(ctx, elapsed.Seconds(), both)
	if !o.routed {
		return
	}
	fallbackAttempts.Record(ctx, int64(o.attempts))
	var ttft time.Duration
	if o.answered {
		ttft = o.firstChunk.Sub(o.start)
		timeToFirstToken.Record(ctx, ttft.Seconds(), metric.WithAttributes(attribute.String("vendor", vendor)))
	}

	e := usage.Event{
		ID: uuid.NewString(), WorkspaceID: key.WorkspaceID, KeyID: key.ID, RouteAlias: o.alias,
		Status: o.status, Outcome: outcome, Attempts: o.attempts,
		LatencyMs: int(elapsed.Milliseconds()), TTFTMs: int(ttft.Milliseconds()),
		Cached: o.cached, ContextHandoff: o.handoff.Action(), CreatedAt: o.start,
	}
	if o.answered {
		e.Vendor, e.Model = string(o.target.Vendor), o.target.Model
	}
	if cost.usage != nil {
		e.PromptTokens, e.CompletionTokens = cost.usage.PromptTokens, cost.usage.CompletionTokens
	}
	e.CostMicroUSD = d.chargeBudgets(requestCtx, o, key, cost)
	d.Usage.Record(e)
	if o.prompt != nil && d.PromptLog != nil {
		// Off the handler: net/http finishes the response only when it
		// returns, so a non-streaming body's tail and a stream's end would
		// otherwise wait for the redaction.
		go func() { d.PromptLog.Record(promptLog(e, o)) }()
	}
}

func promptLog(e usage.Event, o *observation) store.RequestLog {
	l := store.RequestLog{
		ID: e.ID, WorkspaceID: e.WorkspaceID, KeyID: e.KeyID, RouteAlias: e.RouteAlias,
		Vendor: e.Vendor, Model: e.Model, Status: e.Status, Outcome: e.Outcome, CreatedAt: e.CreatedAt,
	}
	l.Request, l.RequestTruncated = promptlog.Prepare(o.prompt)
	answer, cut := o.response()
	l.Response, l.ResponseTruncated = promptlog.Prepare(answer)
	l.ResponseTruncated = l.ResponseTruncated || cut
	return l
}
