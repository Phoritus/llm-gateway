package ratelimit

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRuleConstructors(t *testing.T) {
	minute := PerMinute(60)
	if minute.Burst != 60 {
		t.Errorf("PerMinute(60).Burst = %v, want 60", minute.Burst)
	}
	if minute.Refill != 1 {
		t.Errorf("PerMinute(60).Refill = %v, want 1 token per second", minute.Refill)
	}

	hour := PerHour(3600)
	if hour.Refill != 1 {
		t.Errorf("PerHour(3600).Refill = %v, want 1 token per second", hour.Refill)
	}
	if hour.TTL <= 0 {
		t.Error("PerHour produced a rule with no TTL, so the bucket would never expire")
	}
}

func TestKeyEscapesSeparators(t *testing.T) {
	l := &Limiter{prefix: "rl"}

	// An identifier is caller-supplied (an address, an email hash). A colon in
	// it must not be able to shift the key into another scope.
	if got, want := l.key("login:ip", "1.2.3.4"), "rl:login_ip:1.2.3.4"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if l.key("a", "b:c") == l.key("a:b", "c") {
		t.Fatal("a colon in the identifier collides with another scope")
	}
}

// testRedis connects to the Redis named by REDIS_TEST_URL, or skips. These
// tests exercise the Lua script, which cannot be covered without a real
// server.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the token bucket tests (docker compose up -d)")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis at REDIS_TEST_URL is unreachable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// testLimiter is a limiter whose keys belong to this test alone.
//
// The Redis these tests use is shared: REDIS_TEST_URL names one logical
// database, and `go test ./...` runs this package at the same time as the
// others that connect to it. A bucket refilled by someone else's cleanup
// allows one more request than its burst, which is exactly the failure this
// prefix prevents. No test may flush the whole database for the same reason.
func testLimiter(t *testing.T, client *redis.Client) *Limiter {
	t.Helper()
	return &Limiter{
		client: client,
		script: redis.NewScript(bucketScript),
		prefix: "rltest:" + strings.ReplaceAll(t.Name(), "/", "_"),
	}
}

func TestBucketAllowsUpToBurstThenRefuses(t *testing.T) {
	client := testRedis(t)
	limiter := testLimiter(t, client)
	ctx := context.Background()

	id := "burst-" + time.Now().Format("150405.000000")
	rule := Rule{Burst: 3, Refill: 0.01, TTL: time.Minute}

	for i := range 3 {
		decision, err := limiter.AllowN(ctx, "test", id, rule, 1)
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !decision.Allowed {
			t.Fatalf("request %d was refused, want the first 3 allowed", i)
		}
	}

	decision, err := limiter.AllowN(ctx, "test", id, rule, 1)
	if err != nil {
		t.Fatalf("Allow past burst: %v", err)
	}
	if decision.Allowed {
		t.Fatal("the fourth request was allowed although the burst is 3")
	}
	// A client needs to know how long to wait, and a zero would have it retry
	// immediately and be refused again.
	if decision.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive duration", decision.RetryAfter)
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	client := testRedis(t)
	limiter := testLimiter(t, client)
	ctx := context.Background()

	id := "refill-" + time.Now().Format("150405.000000")
	// 20 tokens a second, so one token is back in 50ms.
	rule := Rule{Burst: 1, Refill: 20, TTL: time.Minute}

	if d, err := limiter.AllowN(ctx, "test", id, rule, 1); err != nil || !d.Allowed {
		t.Fatalf("first request: allowed = %v, err = %v", d.Allowed, err)
	}
	if d, err := limiter.AllowN(ctx, "test", id, rule, 1); err != nil || d.Allowed {
		t.Fatalf("second immediate request: allowed = %v, err = %v", d.Allowed, err)
	}

	time.Sleep(150 * time.Millisecond)

	if d, err := limiter.AllowN(ctx, "test", id, rule, 1); err != nil || !d.Allowed {
		t.Fatalf("after the refill window: allowed = %v, err = %v", d.Allowed, err)
	}
}

func TestResetClearsTheBucket(t *testing.T) {
	client := testRedis(t)
	limiter := testLimiter(t, client)
	ctx := context.Background()

	id := "reset-" + time.Now().Format("150405.000000")
	rule := Rule{Burst: 1, Refill: 0.001, TTL: time.Minute}

	if d, _ := limiter.AllowN(ctx, "test", id, rule, 1); !d.Allowed {
		t.Fatal("first request was refused")
	}
	if d, _ := limiter.AllowN(ctx, "test", id, rule, 1); d.Allowed {
		t.Fatal("second request was allowed with a burst of 1")
	}
	if err := limiter.Reset(ctx, "test", id); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d, _ := limiter.AllowN(ctx, "test", id, rule, 1); !d.Allowed {
		t.Fatal("the bucket still refuses after Reset")
	}
}

// The whole point of evaluating the bucket in Lua is that concurrent callers
// cannot both spend the same token. With the check-then-write split across
// round trips, this test would hand out more than the burst.
func TestBucketIsAtomicUnderConcurrency(t *testing.T) {
	client := testRedis(t)
	limiter := testLimiter(t, client)
	ctx := context.Background()

	id := "atomic-" + time.Now().Format("150405.000000")
	const burst = 10
	rule := Rule{Burst: burst, Refill: 0.001, TTL: time.Minute}

	const callers = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := limiter.AllowN(ctx, "test", id, rule, 1)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if decision.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != burst {
		t.Fatalf("%d of %d concurrent requests were allowed, want exactly %d", allowed, callers, burst)
	}
}

func TestAllowRejectsAnUnusableRule(t *testing.T) {
	limiter := New(&redis.Client{})

	for _, rule := range []Rule{{}, {Burst: 1}, {Refill: 1}, {Burst: -1, Refill: 1}} {
		if _, err := limiter.AllowN(context.Background(), "test", "id", rule, 1); err == nil {
			t.Fatalf("Allow with rule %+v returned no error", rule)
		}
	}
}
