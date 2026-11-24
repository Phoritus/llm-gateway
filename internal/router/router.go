package router

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"llm-gateway/internal/handoff"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

const (
	// budget is how long a request may keep starting attempts, counted from
	// its first. The first retry is exempt, so one slow target never leaves
	// a route without its fallback.
	budget = 45 * time.Second
	// DefaultFirstByteTimeout is how long a target with no wait of its own
	// may take to its first chunk.
	DefaultFirstByteTimeout = 30 * time.Second
	// copyTTL is how long a value read from the shared store is reused in
	// process, so that a busy route costs about one read a second.
	copyTTL = time.Second
	// writeTimeout bounds the writes after a failure, which nobody waits on.
	writeTimeout = 2 * time.Second
)

// The labels are a vendor and an outcome, both from fixed sets.
var (
	upstreamErrors, _ = otel.Meter("llm-gateway/router").Int64Counter("router.upstream.errors",
		metric.WithDescription("Failed calls to a target, by vendor and outcome."))
	// A count of openings stands in for the breaker's state: a gauge of it
	// would have to be read from each instance's own copy of the shared state.
	breakerOpened, _ = otel.Meter("llm-gateway/router").Int64Counter("router.breaker.opened",
		metric.WithDescription("Times a vendor's circuit breaker opened."))
)

// FirstByteTimeout is how long t may take to its first chunk.
func FirstByteTimeout(t store.ResolvedTarget) time.Duration {
	if t.FirstByteTimeout > 0 {
		return t.FirstByteTimeout
	}
	return DefaultFirstByteTimeout
}

func extraWait(t store.ResolvedTarget) time.Duration {
	return max(0, FirstByteTimeout(t)-DefaultFirstByteTimeout)
}

// Attempt sends req to one target and reads its first chunk, within
// FirstByteTimeout(t). req is the request as this target is to see it, which
// context handoff may have fitted to its window. A stream returned alongside
// an error is closed by Run.
type Attempt func(ctx context.Context, t store.ResolvedTarget, req *provider.ChatRequest) (provider.Stream, *provider.ChatChunk, error)

// Step is what became of one target, in the order the route lists them.
type Step struct {
	Vendor  provider.Vendor
	Model   string
	Outcome Outcome
	// Trimmed is set when the target was sent a conversation fitted to its
	// window.
	Trimmed bool
	// Err is why an attempt failed. It is for logs: it can quote the upstream.
	Err error
}

// Result is the target that answered, and the steps taken to reach it. Steps
// is filled in whether or not a target answered.
type Result struct {
	Stream provider.Stream
	First  *provider.ChatChunk
	Target store.ResolvedTarget
	Steps  []Step
	// Handoff is what context handoff did, filled in whether or not a target
	// answered.
	Handoff Handoff
}

// Attempts is how many targets were called, which leaves out the skipped.
func (r Result) Attempts() int {
	n := 0
	for _, s := range r.Steps {
		if s.Outcome != OutcomeSkipped {
			n++
		}
	}
	return n
}

// Router tries a route's targets in order, leaving out the ones recent
// failures say will fail again.
type Router struct {
	state *failOpen
	// recent is the in-process copy of what was last read from or written to
	// the shared store, absences included.
	recent  *Memory
	now     func() time.Time
	pending sync.WaitGroup
}

// New builds a Router over the store every instance shares. A nil logger is
// slog.Default().
func New(shared Store, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{
		state:  &failOpen{shared: shared, local: NewMemory(), logger: logger},
		recent: NewMemory(),
		now:    time.Now,
	}
}

// Wait blocks until the writes of every failure seen so far have finished.
func (r *Router) Wait() { r.pending.Wait() }

// Run calls attempt on each target until one produces a first chunk.
//
// Failover is legal only up to that chunk, so Run returns as soon as there is
// one and what happens to the stream afterwards is the caller's. When no
// target answers, the error is a single *provider.Error standing for all of
// them: rate limited, with the wait until the earliest limit lifts, if any
// target was refused or is being skipped for a limit; the failure itself if
// every attempt failed the same way, a context error carrying the largest
// window stated; otherwise an upstream failure. A context that ends stops the
// loop and its error is returned as it came.
//
// Context handoff follows the route's mode. reorder and trim try the targets
// whose known window fits req first. trim also sends each target with a known
// window req fitted to it, and a target that refuses req for its length is
// called once more when what it said makes the fitted request smaller. Windows
// stated in errors are kept whatever the mode.
func (r *Router) Run(ctx context.Context, route *store.ResolvedRoute, req *provider.ChatRequest, attempt Attempt) (Result, error) {
	var (
		res             Result
		seen            tally
		started         time.Time
		budgetWithWaits = budget
		// tooLong holds the models that refused this request for its length.
		// Another credential for the same model has the same window.
		tooLong  = map[string]bool{}
		refitted = map[int]bool{}
		// scales holds, per vendor, the upstream tokens per estimated token
		// an error showed: another vendor's tokenizer counts differently.
		scales = map[provider.Vendor]float64{}
	)
	known := r.lookup(ctx, route.Targets)
	targets, mode := route.Targets, route.ContextHandoff
	if mode == store.HandoffReorder || mode == store.HandoffTrim {
		targets, res.Handoff.Reordered = fitsFirst(targets, known, req)
	}
	// fit is req as t is to be sent it. ok is false when not even what
	// fitting never drops is under t's window: t is then sent req as it
	// came, and its own error decides, since the window may be a guess.
	fit := func(t store.ResolvedTarget) (send *provider.ChatRequest, rep handoff.Report, ok bool) {
		w := known.windowOf(t)
		if w == 0 || mode != store.HandoffTrim {
			return req, rep, true
		}
		scale := scales[t.Vendor]
		if scale == 0 {
			scale = handoff.DefaultRatio
		}
		send, rep, err := handoff.Fit(req, handoff.Budget(w, req.MaxTokens, scale))
		if err != nil {
			return req, handoff.Report{}, false
		}
		return send, rep, true
	}

	for i := 0; i < len(targets); i++ {
		t := targets[i]
		now := r.now()
		if seen.attempts >= max(1, route.MaxHops) {
			break
		}
		if seen.attempts >= 2 && now.Sub(started) >= budgetWithWaits {
			break
		}
		step := Step{Vendor: t.Vendor, Model: t.Model, Outcome: OutcomeSkipped}
		if s, blocked := known.blocking(t, now); blocked || tooLong[t.Model] {
			if blocked {
				seen.skipped(s)
			}
			res.Steps = append(res.Steps, step)
			continue
		}
		if seen.attempts == 0 {
			started = now
		}

		send, fitted, ok := fit(t)
		if !ok {
			res.Handoff.Unfittable = true
		}
		step.Trimmed = fitted.Changed()
		if !refitted[i] {
			budgetWithWaits += extraWait(t)
		}

		stream, first, err := attempt(ctx, t, send)
		if err == nil {
			r.succeeded(ctx, t, known)
			step.Outcome = OutcomeOK
			res.Steps = append(res.Steps, step)
			res.Stream, res.First, res.Target = stream, first, t
			res.Handoff.Fitted = fitted
			return res, nil
		}
		if stream != nil {
			_ = stream.Close()
		}
		// Nobody is left to answer, or no time to: the failure says nothing
		// about the target, whatever the adapter made of it. The trail still
		// shows the target the request's time ran out on.
		if ctx.Err() != nil {
			step.Outcome, step.Err = OutcomeTimeout, err
			// A client that hung up is not the vendor failing to answer.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				countFailure(ctx, t, step.Outcome)
			}
			res.Steps = append(res.Steps, step)
			return res, err
		}

		rl := ruleFor(err)
		placed := r.failed(ctx, t, rl, provider.RetryAfter(err))
		for key, s := range placed {
			known[key] = s.encode()
		}
		if rl.outcome == OutcomeContextTooLong {
			w, requested := provider.ContextLimit(err)
			if w > 0 {
				r.learnWindow(ctx, t, w, known)
			}
			if q := ratio(requested, send); q > 0 {
				scales[t.Vendor] = q
			}
			// Called once more only with a request that is strictly smaller
			// than the one it refused: the same one would fail the same way.
			if next, _, ok := fit(t); ok && !refitted[i] && handoff.Estimate(next) < handoff.Estimate(send) {
				refitted[i] = true
				i--
			} else {
				tooLong[t.Model] = true
			}
			seen.tooLong(err)
		}
		seen.failed(rl, placed)
		step.Outcome, step.Err = rl.outcome, err
		countFailure(ctx, t, step.Outcome)
		res.Steps = append(res.Steps, step)
	}
	return res, seen.exhausted(r.now())
}

func countFailure(ctx context.Context, t store.ResolvedTarget, o Outcome) {
	upstreamErrors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("vendor", string(t.Vendor)), attribute.String("outcome", string(o))))
}

// view is the stored state a request routes by: the raw values of its
// targets' keys, plus the skips its own failures place as it goes.
type view map[string]string

// blocking returns the skip that keeps t out for longest, if any does.
func (v view) blocking(t store.ResolvedTarget, now time.Time) (skip, bool) {
	var longest skip
	for _, subject := range subjects {
		if s, ok := decodeSkip(v[subject.key(t)]); ok && s.until.After(now) && s.until.After(longest.until) {
			longest = s
		}
	}
	return longest, !longest.until.IsZero()
}

// lookup reads every key the route's targets could be skipped under. What
// the in-process copy lacks is fetched in one round trip.
func (r *Router) lookup(ctx context.Context, targets []store.ResolvedTarget) view {
	known := view{}
	var missing []string
	for _, t := range targets {
		keys := []string{breakerCounter + vendor.id(t), window.key(t)}
		for _, subject := range subjects {
			keys = append(keys, subject.key(t))
		}
		for _, key := range keys {
			if _, dup := known[key]; dup {
				continue
			}
			value, held := r.recent.peek(key)
			known[key] = value
			if !held {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) == 0 {
		return known
	}

	values := r.state.Get(ctx, missing)
	fresh := make([]Entry, len(missing))
	for i, key := range missing {
		known[key] = values[i]
		fresh[i] = Entry{Key: key, Value: values[i], TTL: copyTTL}
	}
	_ = r.recent.Set(ctx, fresh)
	return known
}

// failed applies a rule's effects for a target that just failed. It returns
// the skips that are certain already, which the in-process copy takes at once
// so this instance does not walk into the same target again. The counting,
// and the skips that depend on a count, are written behind the request: the
// next attempt must not wait on Redis.
func (r *Router) failed(ctx context.Context, t store.ResolvedTarget, rl rule, hint time.Duration) map[string]skip {
	if len(rl.effects) == 0 {
		return nil
	}
	now := r.now()
	certain := map[string]skip{}
	var counters []Counter
	for _, e := range rl.effects {
		if e.counter != "" {
			counters = append(counters, Counter{Key: e.counter + e.subject.id(t), Window: e.window})
		}
		if e.counter == "" || e.threshold <= 1 {
			certain[e.subject.key(t)] = skip{outcome: rl.outcome, until: now.Add(e.ttl(hint, 1))}
		}
	}
	r.remember(ctx, certain, nil, nil)

	r.background(ctx, func(ctx context.Context) {
		counts := r.state.Incr(ctx, counters)
		placed := map[string]skip{}
		var entries []Entry
		counted := 0
		for _, e := range rl.effects {
			repeat := int64(1)
			if e.counter != "" {
				n := counts[counted]
				counted++
				if n < e.threshold {
					continue
				}
				repeat = n - e.threshold + 1
			}
			ttl := e.ttl(hint, repeat)
			s := skip{outcome: rl.outcome, until: now.Add(ttl)}
			placed[e.subject.key(t)] = s
			// Only the failure that reached the threshold opens the breaker;
			// the ones after it extend it. The store hands each count to one
			// caller, so concurrent failures and other instances agree on which.
			if e.subject.prefix == vendor.prefix && repeat == 1 {
				breakerOpened.Add(ctx, 1, metric.WithAttributes(attribute.String("vendor", string(t.Vendor))))
			}
			entries = append(entries, Entry{Key: e.subject.key(t), Value: s.encode(), TTL: ttl})
		}
		r.state.Set(ctx, entries)
		r.remember(ctx, placed, counters, counts)
	})
	return certain
}

// succeeded clears the vendor's failure count, when the request saw one.
func (r *Router) succeeded(ctx context.Context, t store.ResolvedTarget, known view) {
	key := breakerCounter + vendor.id(t)
	if known[key] == "" {
		return
	}
	_ = r.recent.Set(ctx, []Entry{{Key: key, TTL: copyTTL}})
	r.background(ctx, func(ctx context.Context) {
		r.state.Del(ctx, []string{key})
	})
}

// remember puts skips and counts into the in-process copy.
func (r *Router) remember(ctx context.Context, skips map[string]skip, counters []Counter, counts []int64) {
	entries := make([]Entry, 0, len(skips)+len(counters))
	for key, s := range skips {
		entries = append(entries, Entry{Key: key, Value: s.encode(), TTL: copyTTL})
	}
	for i, c := range counters {
		entries = append(entries, Entry{Key: c.Key, Value: strconv.FormatInt(counts[i], 10), TTL: copyTTL})
	}
	_ = r.recent.Set(ctx, entries)
}

// background runs work detached from the request, so that neither the next
// attempt nor a disconnect waits on it.
func (r *Router) background(ctx context.Context, work func(context.Context)) {
	r.pending.Add(1)
	go func() {
		defer r.pending.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		defer cancel()
		work(ctx)
	}()
}

// tally is what a run has seen so far, kept in the form exhaustion is
// answered from.
type tally struct {
	attempts int
	// kind is the failure every attempt so far shared, nil once two differed.
	kind error
	// limited is set once any target was refused for a limit or is being
	// skipped for one, and limitUntil is the earliest such skip's end.
	limited                       bool
	limitUntil                    time.Time
	widestWindow, widestRequested int
}

func (t *tally) tooLong(err error) {
	if w, requested := provider.ContextLimit(err); w > t.widestWindow {
		t.widestWindow, t.widestRequested = w, requested
	}
}

func (t *tally) skipped(s skip) {
	if isLimit(s.outcome) {
		t.limit(s.until)
	}
}

func (t *tally) failed(rl rule, placed map[string]skip) {
	if t.attempts == 0 {
		t.kind = rl.kind
	} else if t.kind != rl.kind {
		t.kind = nil
	}
	t.attempts++
	if rl.limit {
		t.limited = true
		for _, s := range placed {
			t.limit(s.until)
		}
	}
}

func (t *tally) limit(until time.Time) {
	t.limited = true
	if t.limitUntil.IsZero() || until.Before(t.limitUntil) {
		t.limitUntil = until
	}
}

func (t *tally) exhausted(now time.Time) *provider.Error {
	const detail = "every target failed or is being skipped"
	switch {
	case t.limited:
		return &provider.Error{Kind: provider.ErrRateLimited, RetryAfter: max(0, t.limitUntil.Sub(now)), Detail: detail}
	case t.kind == provider.ErrContextTooLong:
		return &provider.Error{Kind: t.kind, Detail: detail, Window: t.widestWindow, Requested: t.widestRequested}
	case t.kind != nil:
		return &provider.Error{Kind: t.kind, Detail: detail}
	}
	return &provider.Error{Kind: provider.ErrUpstream, Detail: detail}
}
