package ratelimit

import (
	"context"
	"testing"
	"time"
)

func mustPeek(t *testing.T, l takeSettler, buckets ...Bucket) []float64 {
	t.Helper()
	levels, err := l.Peek(context.Background(), buckets)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(levels) != len(buckets) {
		t.Fatalf("Peek returned %d levels for %d buckets", len(levels), len(buckets))
	}
	return levels
}

func TestPeekReadsLevelsWithoutSpending(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		spent := Bucket{Scope: "peek-spent", ID: id, Rule: slow(5), Cost: 2}
		untouched := Bucket{Scope: "peek-untouched", ID: id, Rule: slow(7)}
		mustTake(t, l, spent)

		for range 2 {
			levels := mustPeek(t, l, spent, untouched)
			if !near(levels[0], 3) || !near(levels[1], 7) {
				t.Fatalf("levels = %v, want 3 left of 5 and a full 7", levels)
			}
		}
		// The cost on a peeked bucket is not spent: exactly three are still there.
		spent.Cost = 3
		if res := mustTake(t, l, spent); !res.Allowed {
			t.Fatal("a peek spent from the bucket")
		}
	})
}

func TestPeekReportsADebt(t *testing.T) {
	eachLimiter(t, func(t *testing.T, l takeSettler, id string) {
		b := Bucket{Scope: "peek-debt", ID: id, Rule: slow(5), Cost: 8}
		mustSettle(t, l, b)
		if levels := mustPeek(t, l, b); !near(levels[0], -3) {
			t.Fatalf("level = %v, want the bucket 3 in debt", levels[0])
		}
	})
}

func TestPeekCreatesNoBucket(t *testing.T) {
	b := Bucket{Scope: "peek-absent", ID: "id-" + time.Now().Format("150405.000000"), Rule: slow(5)}

	t.Run("local", func(t *testing.T) {
		l := NewLocal()
		mustPeek(t, l, b)
		if len(l.buckets) != 0 {
			t.Fatalf("a peek left %d buckets behind", len(l.buckets))
		}
	})
	t.Run("redis", func(t *testing.T) {
		client := testRedis(t)
		l := testLimiter(t, client)
		mustPeek(t, l, b)
		n, err := client.Exists(context.Background(), l.key(b.Scope, b.ID)).Result()
		if err != nil || n != 0 {
			t.Fatalf("a peek left the key behind: exists = %d, %v", n, err)
		}
	})
}

func TestPeekDoesNotKeepAnIdleBucketAlive(t *testing.T) {
	l := NewLocal()
	now := time.Now()
	l.now = func() time.Time { return now }
	b := Bucket{Scope: "peek-idle", ID: "id", Rule: Rule{Burst: 5, Refill: 0.0001, TTL: time.Minute}, Cost: 5}
	mustTake(t, l, b)

	now = now.Add(50 * time.Second)
	if levels := mustPeek(t, l, b); levels[0] > 1 {
		t.Fatalf("level = %v inside the TTL, want the bucket still drained", levels[0])
	}
	// Had the peek at 50s refreshed the bucket, it would outlive this.
	now = now.Add(20 * time.Second)
	if levels := mustPeek(t, l, b); !near(levels[0], 5) {
		t.Fatalf("level = %v past the TTL, want the bucket forgotten and full", levels[0])
	}
}

func TestPeekRejectsAnUnusableRule(t *testing.T) {
	if _, err := NewLocal().Peek(context.Background(), []Bucket{{Scope: "peek-bad", ID: "id"}}); err == nil {
		t.Fatal("Peek accepted a rule with no burst or refill")
	}
}

func TestFailOpenPeeksInProcessWhileThePrimaryIsDown(t *testing.T) {
	f, primary := newFailOpen()
	primary.failing.Store(true)
	b := Bucket{Scope: "failopen-peek", ID: "id", Rule: slow(4), Cost: 1}

	if res, err := f.Take(context.Background(), []Bucket{b}); err != nil || !res.Allowed {
		t.Fatalf("Take = %+v, %v", res, err)
	}
	levels, err := f.Peek(context.Background(), []Bucket{b})
	if err != nil || len(levels) != 1 || !near(levels[0], 3) {
		t.Fatalf("Peek with the primary down = %v, %v; want the fallback's 3", levels, err)
	}
}
