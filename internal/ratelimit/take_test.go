package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slow refills so little during a test that a bucket's level is effectively
// what the test spent.
func slow(burst float64) Rule {
	return Rule{Burst: burst, Refill: 0.001, TTL: time.Minute}
}

// eachLimiter runs a test against the in-process limiter and, when
// REDIS_TEST_URL is set, the Lua one. The two must behave alike: the gateway
// swaps one for the other when Redis goes away.
func eachLimiter(t *testing.T, test func(t *testing.T, l takeSettler, id string)) {
	t.Helper()
	id := "id-" + time.Now().Format("150405.000000")
	t.Run("local", func(t *testing.T) { test(t, NewLocal(), id) })
	t.Run("redis", func(t *testing.T) { test(t, testLimiter(t, testRedis(t)), id) })
}

func mustTake(t *testing.T, l takeSettler, buckets ...Bucket) Result {
	t.Helper()
	res, err := l.Take(context.Background(), buckets)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	return res
}

func mustSettle(t *testing.T, l takeSettler, buckets ...Bucket) {
	t.Helper()
	if err := l.Settle(context.Background(), buckets); err != nil {
		t.Fatalf("Settle: %v", err)
	}
}

func TestTakeSpendsEveryBucketOrNone(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		wide := Bucket{Scope: "wide", ID: id, Rule: slow(5), Cost: 1}
		narrow := Bucket{Scope: "narrow", ID: id, Rule: slow(1), Cost: 1}

		if res := mustTake(t, l, wide, narrow); !res.Allowed {
			t.Fatal("the first take was refused with both buckets full")
		}
		res := mustTake(t, l, wide, narrow)
		if res.Allowed {
			t.Fatal("a take was allowed with the narrow bucket empty")
		}
		if res.RetryAfter <= 0 {
			t.Errorf("RetryAfter = %v, want a positive wait", res.RetryAfter)
		}

		// Four are left only if the refusal took nothing from the wide bucket.
		wide.Cost = 4
		if res := mustTake(t, l, wide); !res.Allowed {
			t.Fatal("the refusal by the narrow bucket spent the wide one")
		}
		wide.Cost = 1
		if res := mustTake(t, l, wide); res.Allowed {
			t.Fatal("the wide bucket holds more than its burst allows")
		}
	})
}

func TestTakeReportsWhatIsLeft(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		res := mustTake(t, l,
			Bucket{Scope: "a", ID: id, Rule: slow(10), Cost: 3},
			Bucket{Scope: "b", ID: id, Rule: slow(100), Cost: 40})
		if len(res.Remaining) != 2 || !near(res.Remaining[0], 7) || !near(res.Remaining[1], 60) {
			t.Fatalf("Remaining = %v, want about [7 60]", res.Remaining)
		}
	})
}

func TestTakeRetryIsTheLongestWait(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		quick := Bucket{Scope: "quick", ID: id, Rule: Rule{Burst: 1, Refill: 1, TTL: time.Minute}, Cost: 1}
		sluggish := Bucket{Scope: "sluggish", ID: id, Rule: Rule{Burst: 1, Refill: 0.1, TTL: time.Minute}, Cost: 1}

		mustTake(t, l, quick, sluggish)
		res := mustTake(t, l, quick, sluggish)
		if res.Allowed {
			t.Fatal("allowed with both buckets empty")
		}
		// The quick bucket is back in a second, the sluggish one in ten. A
		// client told one second would retry into a second refusal.
		if res.RetryAfter < 9*time.Second || res.RetryAfter > 10*time.Second {
			t.Fatalf("RetryAfter = %v, want the sluggish bucket's ten seconds", res.RetryAfter)
		}
	})
}

func TestTakeClampsACostAboveTheBurst(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		b := Bucket{Scope: "clamp", ID: id, Rule: slow(10), Cost: 50}

		res := mustTake(t, l, b)
		if !res.Allowed {
			t.Fatal("a cost above the burst can never be admitted unless it is clamped")
		}
		if !near(res.Remaining[0], 0) {
			t.Fatalf("Remaining = %v, want the whole burst spent and no more", res.Remaining)
		}
	})
}

func TestSettleRefundStopsAtTheBurst(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		b := Bucket{Scope: "refund", ID: id, Rule: slow(10), Cost: 3}
		mustTake(t, l, b)

		b.Cost = -100
		mustSettle(t, l, b)

		b.Cost = 10
		if res := mustTake(t, l, b); !res.Allowed {
			t.Fatal("the refund did not fill the bucket")
		}
		b.Cost = 1
		if res := mustTake(t, l, b); res.Allowed {
			t.Fatal("the refund filled the bucket past its burst")
		}
	})
}

func TestSettleDebtMakesTheNextTakeWait(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		rule := Rule{Burst: 10, Refill: 1, TTL: time.Minute}
		b := Bucket{Scope: "debt", ID: id, Rule: rule, Cost: 10}
		mustTake(t, l, b)

		b.Cost = 5
		mustSettle(t, l, b)

		b.Cost = 1
		res := mustTake(t, l, b)
		if res.Allowed {
			t.Fatal("a take was allowed from a bucket in debt")
		}
		// Five to clear the debt and one for the token.
		if res.RetryAfter < 5*time.Second || res.RetryAfter > 6*time.Second {
			t.Fatalf("RetryAfter = %v, want about 6s", res.RetryAfter)
		}

		b.Cost = 1000
		mustSettle(t, l, b)
		b.Cost = 1
		if res := mustTake(t, l, b); res.RetryAfter > 11*time.Second {
			t.Fatalf("RetryAfter = %v: the debt went below -Burst", res.RetryAfter)
		}
	})
}

func TestTakeRefillsOverTime(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		// 20 tokens a second, so one token is back in 50ms.
		b := Bucket{Scope: "refill", ID: id, Rule: Rule{Burst: 1, Refill: 20, TTL: time.Minute}, Cost: 1}

		if !mustTake(t, l, b).Allowed {
			t.Fatal("first take refused")
		}
		if mustTake(t, l, b).Allowed {
			t.Fatal("second immediate take allowed")
		}
		time.Sleep(150 * time.Millisecond)
		if !mustTake(t, l, b).Allowed {
			t.Fatal("refused after the refill window")
		}
	})
}

func TestTakeRejectsAnUnusableRule(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		bad := []Bucket{{Scope: "ok", ID: id, Rule: slow(1), Cost: 1}, {Scope: "bad", ID: id, Cost: 1}}
		if _, err := l.Take(context.Background(), bad); err == nil {
			t.Error("Take with a zero rule returned no error")
		}
		if err := l.Settle(context.Background(), bad); err == nil {
			t.Error("Settle with a zero rule returned no error")
		}
	})
}

// Concurrent callers must not both spend the same token, and a caller the
// narrow bucket refuses must not have spent the wide one on the way.
func TestTakeIsAtomicUnderConcurrency(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		const burst, callers = 10, 50
		narrow := Bucket{Scope: "narrow", ID: id, Rule: slow(burst), Cost: 1}
		wide := Bucket{Scope: "wide", ID: id, Rule: slow(100), Cost: 1}

		var (
			wg      sync.WaitGroup
			allowed atomic.Int64
		)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := l.Take(context.Background(), []Bucket{wide, narrow})
				if err != nil {
					t.Errorf("Take: %v", err)
					return
				}
				if res.Allowed {
					allowed.Add(1)
				}
			}()
		}
		wg.Wait()

		if allowed.Load() != burst {
			t.Fatalf("%d of %d concurrent takes were allowed, want exactly %d", allowed.Load(), callers, burst)
		}
		wide.Cost = 0
		if res := mustTake(t, l, wide); !near(res.Remaining[0], 100-burst) {
			t.Fatalf("the wide bucket holds %v, want %d: refused takes spent it", res.Remaining[0], 100-burst)
		}
	})
}

func TestTakeCountsARefusalUnderEachRefusingScope(t *testing.T) {
	reader := metricReader()
	l := NewLocal()
	id := "id"
	empty := Bucket{Scope: "take-test:empty", ID: id, Rule: slow(1), Cost: 1}
	full := Bucket{Scope: "take-test:full", ID: id, Rule: slow(100), Cost: 1}

	for range 3 {
		mustTake(t, l, full, empty)
	}

	if got := rejectionsFor(t, reader, empty.Scope); got != 2 {
		t.Errorf("rejections for the empty bucket = %d, want 2", got)
	}
	// The label names what ran out; the bucket that could have paid is not it.
	if got := rejectionsFor(t, reader, full.Scope); got > 0 {
		t.Errorf("rejections for the bucket that still had tokens = %d, want none", got)
	}
}

func TestLocalForgetsIdleBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLocal()
	l.now = func() time.Time { return now }

	mustTake(t, l, Bucket{Scope: "idle", ID: "a", Rule: slow(1), Cost: 1})
	now = now.Add(2 * time.Minute)
	mustTake(t, l, Bucket{Scope: "idle", ID: "b", Rule: slow(1), Cost: 1})

	if len(l.buckets) != 1 {
		t.Fatalf("%d buckets held, want the idle one dropped after its TTL", len(l.buckets))
	}
}

// near allows for the refill that happens between two calls and for the
// thousandths the Lua script reports in.
func near(got, want float64) bool {
	return got >= want-0.001 && got <= want+0.01
}

// flaky is a primary whose health a test controls.
type flaky struct {
	*Local
	failing atomic.Bool
	calls   atomic.Int64
}

var errPrimaryDown = errors.New("primary down")

func (f *flaky) Take(ctx context.Context, buckets []Bucket) (Result, error) {
	f.calls.Add(1)
	if f.failing.Load() {
		return Result{}, errPrimaryDown
	}
	return f.Local.Take(ctx, buckets)
}

func (f *flaky) Settle(ctx context.Context, buckets []Bucket) error {
	f.calls.Add(1)
	if f.failing.Load() {
		return errPrimaryDown
	}
	return f.Local.Settle(ctx, buckets)
}

func (f *flaky) Peek(ctx context.Context, buckets []Bucket) ([]float64, error) {
	f.calls.Add(1)
	if f.failing.Load() {
		return nil, errPrimaryDown
	}
	return f.Local.Peek(ctx, buckets)
}

func newFailOpen() (*FailOpen, *flaky) {
	primary := &flaky{Local: NewLocal()}
	return &FailOpen{
		primary:  primary,
		fallback: NewLocal(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, primary
}

func TestFailOpenLimitsInProcessWhileThePrimaryIsDown(t *testing.T) {
	f, primary := newFailOpen()
	primary.failing.Store(true)
	b := Bucket{Scope: "failopen", ID: "id", Rule: slow(1), Cost: 1}

	res, err := f.Take(context.Background(), []Bucket{b})
	if err != nil || !res.Allowed {
		t.Fatalf("Take with the primary down = %+v, %v; want it admitted by the fallback", res, err)
	}
	// Open is not unlimited: the fallback still holds the request to its rule.
	res, err = f.Take(context.Background(), []Bucket{b})
	if err != nil || res.Allowed {
		t.Fatalf("second Take = %+v, %v; want the fallback to refuse past the burst", res, err)
	}
	if err := f.Settle(context.Background(), []Bucket{b}); err != nil {
		t.Fatalf("Settle with the primary down: %v", err)
	}

	if got := primary.calls.Load(); got != 1 {
		t.Fatalf("the primary was called %d times, want once and then left alone for the cool-off", got)
	}
}

func TestFailOpenReturnsToThePrimaryAfterTheCoolOff(t *testing.T) {
	f, primary := newFailOpen()
	primary.failing.Store(true)
	b := Bucket{Scope: "failopen", ID: "id", Rule: slow(1), Cost: 1}
	mustTake(t, f, b)

	primary.failing.Store(false)
	f.retryAt.Store(time.Now().Add(-time.Second).UnixNano())

	// The primary's bucket is still full: the outage's take went to the fallback.
	if res := mustTake(t, f, b); !res.Allowed {
		t.Fatal("the recovered primary was not used")
	}
	if got := primary.calls.Load(); got != 2 {
		t.Fatalf("the primary was called %d times, want 2", got)
	}
}

func TestFailOpenDoesNotBlameThePrimaryForACallerThatLeft(t *testing.T) {
	f, primary := newFailOpen()
	primary.failing.Store(true)
	b := Bucket{Scope: "failopen", ID: "id", Rule: slow(5), Cost: 1}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Take(gone, []Bucket{b}); err != nil {
		t.Fatalf("Take: %v", err)
	}
	primary.failing.Store(false)
	mustTake(t, f, b)

	if got := primary.calls.Load(); got != 2 {
		t.Fatalf("the primary was called %d times, want 2: a cancelled caller started a cool-off", got)
	}
}
