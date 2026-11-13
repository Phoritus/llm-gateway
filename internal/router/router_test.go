package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is a Router over in-memory state, on a clock the test moves.
type harness struct {
	t     *testing.T
	r     *Router
	clock *fakeClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	shared := NewMemory()
	shared.now = clock.Now
	r := New(shared, nil)
	r.now, r.recent.now = clock.Now, clock.Now
	return &harness{t: t, r: r, clock: clock}
}

func target(credential string, vendor provider.Vendor, model string) store.ResolvedTarget {
	return store.ResolvedTarget{CredentialID: credential, Vendor: vendor, Model: model, Secret: "sk-" + credential}
}

func failure(kind error, retryAfter time.Duration) error {
	return &provider.Error{Kind: kind, RetryAfter: retryAfter}
}

type fakeStream struct{ closed bool }

func (s *fakeStream) Next() (*provider.ChatChunk, error) { return nil, errors.New("not read in tests") }
func (s *fakeStream) Close() error                       { s.closed = true; return nil }

// run routes over targets, failing each credential the way fail says, and
// returns the credentials that were called, in order. It waits for the
// failures' writes, so the next run sees them.
func (h *harness) run(maxHops int, fail map[string]error, targets ...store.ResolvedTarget) (Result, []string, error) {
	h.t.Helper()
	var called []string
	res, err := h.r.Run(context.Background(), &store.ResolvedRoute{ID: "r", MaxHops: maxHops, Targets: targets},
		func(_ context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
			called = append(called, t.CredentialID)
			if err := fail[t.CredentialID]; err != nil {
				return nil, nil, err
			}
			return &fakeStream{}, &provider.ChatChunk{ID: t.CredentialID}, nil
		})
	h.r.Wait()
	return res, called, err
}

// attempted reports whether a request for t alone reaches it.
func (h *harness) attempted(t store.ResolvedTarget) bool {
	h.t.Helper()
	_, called, _ := h.run(3, nil, t)
	return len(called) == 1
}

func outcomes(steps []Step) []Outcome {
	out := make([]Outcome, len(steps))
	for i, s := range steps {
		out[i] = s.Outcome
	}
	return out
}

func wantOutcomes(t *testing.T, steps []Step, want ...Outcome) {
	t.Helper()
	if got := outcomes(steps); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func wantCalled(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("called %v, want %v", got, want)
	}
}

func TestTheFirstTargetThatAnswersWins(t *testing.T) {
	h := newHarness(t)
	failed := &fakeStream{}
	a, b := target("a", provider.VendorGroq, "m1"), target("b", provider.VendorMistral, "m2")

	res, err := h.r.Run(context.Background(), &store.ResolvedRoute{MaxHops: 3, Targets: []store.ResolvedTarget{a, b}},
		func(_ context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
			if t.CredentialID == "a" {
				return failed, nil, failure(provider.ErrUpstream, 0)
			}
			return &fakeStream{}, &provider.ChatChunk{ID: "from-b"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != b || res.First.ID != "from-b" || res.Stream == nil {
		t.Errorf("result = %+v, want b's stream and first chunk", res)
	}
	if !failed.closed {
		t.Error("the stream whose first chunk failed was left open")
	}
	wantOutcomes(t, res.Steps, OutcomeUpstreamError, OutcomeOK)
	if res.Steps[0].Vendor != provider.VendorGroq || res.Steps[0].Model != "m1" || res.Steps[0].Err == nil {
		t.Errorf("first step = %+v, want the failed target and its error", res.Steps[0])
	}
	if res.Attempts() != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts())
	}
}

func TestEachFailureSkipsItsSubjectForItsTime(t *testing.T) {
	const (
		nothing = iota
		credentialOnly
		credentialAndModel
	)
	cases := []struct {
		name    string
		err     error
		skips   int
		lasting time.Duration
	}{
		{"auth", failure(provider.ErrAuth, 0), credentialOnly, 5 * time.Minute},
		{"quota", failure(provider.ErrQuota, 0), credentialAndModel, time.Hour},
		{"quota with a reset time", failure(provider.ErrQuota, 20*time.Minute), credentialAndModel, 20 * time.Minute},
		{"model unavailable", failure(provider.ErrModelUnavailable, 0), credentialAndModel, 24 * time.Hour},
		{"rate limited", failure(provider.ErrRateLimited, 7*time.Second), credentialOnly, 90 * time.Second},
		{"rate limited for longer than the backoff", failure(provider.ErrRateLimited, 5*time.Minute), credentialOnly, 5 * time.Minute},
		{"a retry-after beyond belief", failure(provider.ErrQuota, 1000*time.Hour), credentialAndModel, 24 * time.Hour},
		{"one upstream failure", failure(provider.ErrUpstream, 0), nothing, 0},
		{"one timeout", failure(provider.ErrTimeout, 0), nothing, 0},
		{"context too long", failure(provider.ErrContextTooLong, 0), nothing, 0},
		{"invalid request", failure(provider.ErrInvalidRequest, 0), nothing, 0},
		{"no adapter", ErrNoAdapter, nothing, 0},
		{"an unclassified error", errors.New("boom"), nothing, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			same := target("a", provider.VendorGroq, "m1")
			otherModel := target("a", provider.VendorGroq, "m2")
			otherCredential := target("b", provider.VendorGroq, "m1")

			if _, _, err := h.run(3, map[string]error{"a": tc.err}, same); err == nil {
				t.Fatal("the only target failed, yet the run succeeded")
			}

			wantSkipped := func(when string, tgt store.ResolvedTarget, want bool) {
				t.Helper()
				if got := !h.attempted(tgt); got != want {
					t.Errorf("%s: %s/%s skipped = %v, want %v", when, tgt.CredentialID, tgt.Model, got, want)
				}
			}
			wantSkipped("at once", same, tc.skips != nothing)
			wantSkipped("at once", otherModel, tc.skips == credentialOnly)
			wantSkipped("at once", otherCredential, false)
			if tc.skips == nothing {
				return
			}
			h.clock.Advance(tc.lasting - time.Second)
			wantSkipped("a second before it ends", same, true)
			h.clock.Advance(2 * time.Second)
			wantSkipped("a second after it ends", same, false)
		})
	}
}

func TestRateLimitBackoffGrowsOnRepeats(t *testing.T) {
	h := newHarness(t)
	a := target("a", provider.VendorGroq, "m1")
	limited := map[string]error{"a": failure(provider.ErrRateLimited, 0)}

	h.run(3, limited, a)
	h.clock.Advance(91 * time.Second)
	if _, called, _ := h.run(3, limited, a); len(called) != 1 {
		t.Fatal("the credential was still skipped after its first 90s")
	}
	h.clock.Advance(179 * time.Second)
	if h.attempted(a) {
		t.Error("a repeat within 15 minutes was skipped for less than 180s")
	}
	h.clock.Advance(2 * time.Second)
	if !h.attempted(a) {
		t.Error("a first repeat was skipped for more than 180s")
	}
}

func TestBackoffDoublesToItsCeilingAndHonoursTheHint(t *testing.T) {
	ttl := backoff(90*time.Second, 15*time.Minute)
	cases := []struct {
		hint   time.Duration
		repeat int64
		want   time.Duration
	}{
		{0, 1, 90 * time.Second},
		{0, 2, 180 * time.Second},
		{0, 4, 720 * time.Second},
		{0, 5, 15 * time.Minute},
		{0, 500, 15 * time.Minute},
		{7 * time.Second, 1, 90 * time.Second},
		{time.Hour, 1, time.Hour},
	}
	for _, tc := range cases {
		if got := ttl(tc.hint, tc.repeat); got != tc.want {
			t.Errorf("backoff(hint %v, repeat %d) = %v, want %v", tc.hint, tc.repeat, got, tc.want)
		}
	}
}

func TestBreakerOpensAfterFiveFailuresAndClosesAfterThirtySeconds(t *testing.T) {
	h := newHarness(t)
	// Five workspaces' credentials, so nothing but the vendor is shared.
	for _, id := range []string{"a", "b", "c", "d"} {
		h.run(3, map[string]error{id: failure(provider.ErrUpstream, 0)}, target(id, provider.VendorGroq, "m1"))
	}
	bystander := target("z", provider.VendorGroq, "m9")
	if !h.attempted(bystander) {
		t.Fatal("the breaker opened after four failures")
	}
	// That success cleared the count, so five more are needed, not one.
	for _, id := range []string{"e", "f", "g", "h"} {
		h.run(3, map[string]error{id: failure(provider.ErrTimeout, 0)}, target(id, provider.VendorGroq, "m1"))
	}
	if !h.attempted(bystander) {
		t.Fatal("the breaker opened on failures a success had interrupted")
	}
	for _, id := range []string{"i", "j", "k", "l", "m"} {
		h.run(3, map[string]error{id: failure(provider.ErrUpstream, 0)}, target(id, provider.VendorGroq, "m1"))
	}

	res, called, err := h.run(3, nil, bystander, target("y", provider.VendorMistral, "m1"))
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "y")
	wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeOK)

	h.clock.Advance(29 * time.Second)
	if h.attempted(bystander) {
		t.Error("the breaker closed before 30s")
	}
	h.clock.Advance(2 * time.Second)
	if !h.attempted(bystander) {
		t.Error("the breaker was still open after 30s")
	}
}

func TestBreakerCountsOnlyFailuresWithinAMinute(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		h.run(3, map[string]error{id: failure(provider.ErrUpstream, 0)}, target(id, provider.VendorGroq, "m1"))
		h.clock.Advance(20 * time.Second)
	}
	if !h.attempted(target("z", provider.VendorGroq, "m1")) {
		t.Error("five failures spread over 100s opened the breaker")
	}
}

func TestThreeFailuresInFifteenMinutesBenchTheCredentialAndModel(t *testing.T) {
	h := newHarness(t)
	a := target("a", provider.VendorGroq, "m1")
	failing := map[string]error{"a": failure(provider.ErrTimeout, 0)}

	for range 2 {
		h.run(3, failing, a)
		h.clock.Advance(5 * time.Minute)
	}
	if !h.attempted(a) {
		t.Fatal("benched after two failures")
	}
	h.run(3, failing, a)

	if h.attempted(a) {
		t.Error("not benched after three failures in fifteen minutes")
	}
	if !h.attempted(target("a", provider.VendorGroq, "m2")) {
		t.Error("the bench reached another model on the same credential")
	}
	h.clock.Advance(10*time.Minute - time.Second)
	if h.attempted(a) {
		t.Error("the bench ended before ten minutes")
	}
	h.clock.Advance(2 * time.Second)
	if !h.attempted(a) {
		t.Error("the bench outlasted ten minutes")
	}
}

func TestSkippedTargetsAreNotCalledAndAreReported(t *testing.T) {
	h := newHarness(t)
	a, b := target("a", provider.VendorGroq, "m1"), target("b", provider.VendorMistral, "m2")
	h.run(3, map[string]error{"a": failure(provider.ErrRateLimited, 0)}, a, b)

	res, called, err := h.run(3, nil, a, b)
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "b")
	wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeOK)
	if res.Attempts() != 1 {
		t.Errorf("Attempts = %d, want 1: a skip is not an attempt", res.Attempts())
	}
}

// A failure skips its subject for the rest of the request that saw it, too:
// two targets on one credential are not both sent into the same rate limit.
func TestAFailureSkipsItsSubjectWithinTheSameRequest(t *testing.T) {
	h := newHarness(t)
	res, called, err := h.run(3, map[string]error{"a": failure(provider.ErrRateLimited, 0)},
		target("a", provider.VendorGroq, "m1"), target("a", provider.VendorGroq, "m2"), target("b", provider.VendorMistral, "m1"))
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "a", "b")
	wantOutcomes(t, res.Steps, OutcomeRateLimited, OutcomeSkipped, OutcomeOK)
}

func TestMaxHopsCapsAttemptsAndSkipsDoNotCount(t *testing.T) {
	h := newHarness(t)
	a := target("a", provider.VendorGroq, "m1")
	h.run(3, map[string]error{"a": failure(provider.ErrAuth, 0)}, a)

	failing := map[string]error{"b": failure(provider.ErrInvalidRequest, 0), "c": failure(provider.ErrInvalidRequest, 0)}
	res, called, err := h.run(2, failing, a,
		target("b", provider.VendorMistral, "m1"), target("c", provider.VendorCerebras, "m1"), target("d", provider.VendorOpenAI, "m1"))
	if err == nil {
		t.Fatal("the run reached a target past its max hops")
	}
	wantCalled(t, called, "b", "c")
	wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeInvalidRequest, OutcomeInvalidRequest)
}

func TestTheBudgetStopsNewAttemptsButNeverTheFirstRetry(t *testing.T) {
	cases := []struct {
		name  string
		takes time.Duration
		want  []string
	}{
		{"a slow first attempt still gets its retry", 50 * time.Second, []string{"a", "b"}},
		{"within budget every hop is used", 20 * time.Second, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var called []string
			_, err := h.r.Run(context.Background(), &store.ResolvedRoute{MaxHops: 3, Targets: []store.ResolvedTarget{
				target("a", provider.VendorGroq, "m1"), target("b", provider.VendorMistral, "m1"), target("c", provider.VendorCerebras, "m1"),
			}}, func(_ context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
				called = append(called, t.CredentialID)
				h.clock.Advance(tc.takes)
				return nil, nil, failure(provider.ErrInvalidRequest, 0)
			})
			if err == nil {
				t.Fatal("every attempt failed, yet the run succeeded")
			}
			wantCalled(t, called, tc.want...)
		})
	}
}

func TestContextTooLongSkipsTheSameModelForThisRequestOnly(t *testing.T) {
	h := newHarness(t)
	sameModel := target("b", provider.VendorCerebras, "m1")
	res, called, err := h.run(3, map[string]error{"a": failure(provider.ErrContextTooLong, 0)},
		target("a", provider.VendorGroq, "m1"), sameModel, target("c", provider.VendorMistral, "m2"))
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "a", "c")
	wantOutcomes(t, res.Steps, OutcomeContextTooLong, OutcomeSkipped, OutcomeOK)

	if !h.attempted(sameModel) {
		t.Error("the model stayed skipped for a later request, which may be shorter")
	}
}

func TestAnEndedContextStopsTheRunAndBlamesNoTarget(t *testing.T) {
	h := newHarness(t)
	a, b := target("a", provider.VendorGroq, "m1"), target("b", provider.VendorMistral, "m1")
	ctx, cancel := context.WithCancel(context.Background())
	var called []string

	res, err := h.r.Run(ctx, &store.ResolvedRoute{MaxHops: 3, Targets: []store.ResolvedTarget{a, b}},
		func(ctx context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
			called = append(called, t.CredentialID)
			cancel()
			// An adapter may dress a cancellation up as its own failure.
			return nil, nil, failure(provider.ErrRateLimited, 0)
		})
	h.r.Wait()

	if !errors.Is(err, provider.ErrRateLimited) || res.Stream != nil {
		t.Fatalf("err = %v, want the attempt's own error passed through", err)
	}
	wantCalled(t, called, "a")
	if !h.attempted(a) {
		t.Error("a target was skipped for a failure that happened after the client left")
	}
}

func TestExhaustionIsReducedToOneError(t *testing.T) {
	limited := failure(provider.ErrRateLimited, 7*time.Second)
	quota := failure(provider.ErrQuota, 20*time.Minute)
	upstream := failure(provider.ErrUpstream, 0)
	tooLong := failure(provider.ErrContextTooLong, 0)
	invalid := failure(provider.ErrInvalidRequest, 0)

	cases := []struct {
		name       string
		errs       []error
		want       error
		retryAfter time.Duration
	}{
		{"any rate limit is a limit", []error{limited, upstream}, provider.ErrRateLimited, 90 * time.Second},
		{"any quota is a limit", []error{upstream, quota}, provider.ErrRateLimited, 20 * time.Minute},
		{"the earliest limit to lift is the wait", []error{quota, limited}, provider.ErrRateLimited, 90 * time.Second},
		{"a limit outranks context length", []error{limited, tooLong}, provider.ErrRateLimited, 90 * time.Second},
		{"all too long", []error{tooLong, tooLong}, provider.ErrContextTooLong, 0},
		{"all invalid", []error{invalid, invalid}, provider.ErrInvalidRequest, 0},
		{"all timeouts", []error{failure(provider.ErrTimeout, 0)}, provider.ErrTimeout, 0},
		{"all without an adapter", []error{ErrNoAdapter, ErrNoAdapter}, ErrNoAdapter, 0},
		{"mixed refusals", []error{tooLong, invalid}, provider.ErrUpstream, 0},
		{"mixed failures", []error{upstream, failure(provider.ErrAuth, 0)}, provider.ErrUpstream, 0},
		{"no targets", nil, provider.ErrUpstream, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			fail := map[string]error{}
			var targets []store.ResolvedTarget
			for i, err := range tc.errs {
				id := string(rune('a' + i))
				fail[id] = err
				// Distinct models, so one target's context refusal skips no other.
				targets = append(targets, target(id, provider.Vendors()[i], "m-"+id))
			}
			_, called, err := h.run(3, fail, targets...)
			if len(called) != len(tc.errs) {
				t.Fatalf("called %v, want every target tried", called)
			}
			wantExhausted(t, err, tc.want, tc.retryAfter)
		})
	}
}

func wantExhausted(t *testing.T, err, kind error, retryAfter time.Duration) {
	t.Helper()
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != kind {
		t.Fatalf("err = %v, want one provider error of kind %q", err, kind)
	}
	if perr.RetryAfter != retryAfter {
		t.Errorf("RetryAfter = %v, want %v", perr.RetryAfter, retryAfter)
	}
}

func TestExhaustionWhenEveryTargetIsSkipped(t *testing.T) {
	h := newHarness(t)
	limited := target("a", provider.VendorGroq, "m1")
	outOfQuota := target("b", provider.VendorMistral, "m1")
	badKey := target("c", provider.VendorCerebras, "m1")
	h.run(3, map[string]error{
		"a": failure(provider.ErrRateLimited, 0),
		"b": failure(provider.ErrQuota, 40*time.Second),
		"c": failure(provider.ErrAuth, 0),
	}, limited, outOfQuota, badKey)
	h.clock.Advance(10 * time.Second)

	t.Run("a limit skip is a 429 until the earliest one ends", func(t *testing.T) {
		res, called, err := h.run(3, nil, limited, outOfQuota, badKey)
		wantCalled(t, called)
		wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeSkipped, OutcomeSkipped)
		if res.Attempts() != 0 {
			t.Errorf("Attempts = %d, want 0", res.Attempts())
		}
		wantExhausted(t, err, provider.ErrRateLimited, 30*time.Second)
	})
	t.Run("no limit among the skips is an upstream failure", func(t *testing.T) {
		_, called, err := h.run(3, nil, badKey)
		wantCalled(t, called)
		wantExhausted(t, err, provider.ErrUpstream, 0)
	})
}

// metricReader installs a meter provider once: the package's counters bind to
// the first global provider only, so every test that reads one shares it.
var metricReader = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

// counted reads a counter's running total across all its label sets. It is
// cumulative across tests, so tests compare it before and after.
func counted(t *testing.T, name string) (total int64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

func TestAClientHangUpIsNotCountedAsAnUpstreamError(t *testing.T) {
	before := counted(t, "router.upstream.errors")
	h := newHarness(t)
	route := &store.ResolvedRoute{ID: "r", MaxHops: 3, Targets: []store.ResolvedTarget{target("c1", provider.VendorGroq, "m")}}
	waitOut := func(ctx context.Context, _ store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}

	ctx, hangUp := context.WithCancel(context.Background())
	hangUp()
	if _, err := h.r.Run(ctx, route, waitOut); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancellation", err)
	}
	if got := counted(t, "router.upstream.errors") - before; got != 0 {
		t.Errorf("a client hanging up counted %d upstream errors against the vendor, want 0", got)
	}

	// The request's own deadline running out on a target still is one.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := h.r.Run(ctx, route, waitOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the deadline", err)
	}
	if got := counted(t, "router.upstream.errors") - before; got != 1 {
		t.Errorf("a request that timed out on its target counted %d upstream errors, want 1", got)
	}
}

func TestBreakerOpenedCountsAnOpeningOnce(t *testing.T) {
	alwaysFail := func(context.Context, store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
		return nil, nil, failure(provider.ErrUpstream, 0)
	}

	t.Run("one request walking ten failing targets", func(t *testing.T) {
		before := counted(t, "router.breaker.opened")
		h := newHarness(t)
		var targets []store.ResolvedTarget
		for i := range 10 {
			targets = append(targets, target(fmt.Sprintf("c%d", i), provider.VendorGroq, "m"))
		}
		_, _ = h.r.Run(context.Background(), &store.ResolvedRoute{ID: "r", MaxHops: 10, Targets: targets}, alwaysFail)
		h.r.Wait()
		if got := counted(t, "router.breaker.opened") - before; got != 1 {
			t.Errorf("ten failures in a row counted %d openings, want 1", got)
		}
	})

	t.Run("ten requests failing at once", func(t *testing.T) {
		before := counted(t, "router.breaker.opened")
		h := newHarness(t)
		var arrived, done sync.WaitGroup
		arrived.Add(10)
		for i := range 10 {
			done.Go(func() {
				route := &store.ResolvedRoute{ID: "r", MaxHops: 3,
					Targets: []store.ResolvedTarget{target(fmt.Sprintf("c%d", i), provider.VendorGroq, "m")}}
				_, _ = h.r.Run(context.Background(), route,
					func(context.Context, store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
						arrived.Done()
						arrived.Wait()
						return nil, nil, failure(provider.ErrUpstream, 0)
					})
			})
		}
		done.Wait()
		h.r.Wait()
		if got := counted(t, "router.breaker.opened") - before; got != 1 {
			t.Errorf("ten concurrent failures counted %d openings, want 1", got)
		}
	})
}
