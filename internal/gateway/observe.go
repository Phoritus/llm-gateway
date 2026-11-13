package gateway

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"llm-gateway/internal/store"
	"llm-gateway/internal/usage"
)

// statusClientClosed is what a request is recorded as when the client left
// before any status was sent. It is nginx's code for the same thing, and never
// reaches a client.
const statusClientClosed = 499

// The labels below come from fixed sets only: a vendor is one of
// provider.Vendor, an outcome is a store.Outcome constant. Never the route
// alias, the workspace or the key: each of those is a new metric series per
// tenant.
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
)

// UsageRecorder takes an event without waiting. *usage.Recorder is the
// production one.
type UsageRecorder interface {
	Record(usage.Event)
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
	answered   bool
	target     store.ResolvedTarget
	firstChunk time.Time
	// status is what the client was told, and outcome the fixed name of how
	// the request ended.
	status  int
	outcome store.Outcome
}

// finish records the request. It runs last, after the response is written,
// and nothing in it waits on anything.
func (d Deps) finish(o *observation, key store.ActiveGatewayKey, cost *spent) {
	vendor, outcome := noVendor, o.outcome
	if o.answered {
		vendor = string(o.target.Vendor)
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
		WorkspaceID: key.WorkspaceID, KeyID: key.ID, RouteAlias: o.alias,
		Status: o.status, Outcome: outcome, Attempts: o.attempts,
		LatencyMs: int(elapsed.Milliseconds()), TTFTMs: int(ttft.Milliseconds()),
		CreatedAt: o.start,
	}
	if o.answered {
		e.Vendor, e.Model = string(o.target.Vendor), o.target.Model
	}
	if cost.usage != nil {
		e.PromptTokens, e.CompletionTokens = cost.usage.PromptTokens, cost.usage.CompletionTokens
	}
	d.Usage.Record(e)
}
