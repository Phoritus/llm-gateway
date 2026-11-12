package router

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

// testRedis is a Redis store whose keys belong to this test alone.
//
// REDIS_TEST_URL names one logical database that every package's tests share
// while `go test ./...` runs them in parallel, so the keys are prefixed with
// the test's name and deleted one by one afterwards, never flushed.
func testRedis(t *testing.T) *Redis {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the Redis skip-state tests")
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

	s := NewRedis(client)
	s.prefix = "routertest:" + strings.ReplaceAll(t.Name(), "/", "_") + ":" + time.Now().Format("150405.000000") + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := client.Keys(ctx, s.prefix+"*").Result()
		if len(keys) > 0 {
			client.Del(ctx, keys...)
		}
		_ = client.Close()
	})
	return s
}

// eachStore runs a test against Memory and, when REDIS_TEST_URL is set,
// Redis. The two must behave alike: the router swaps one for the other when
// Redis goes away.
func eachStore(t *testing.T, test func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemory()) })
	t.Run("redis", func(t *testing.T) { test(t, testRedis(t)) })
}

func mustGet(t *testing.T, s Store, keys ...string) []string {
	t.Helper()
	values, err := s.Get(context.Background(), keys)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return values
}

func mustIncr(t *testing.T, s Store, counters ...Counter) []int64 {
	t.Helper()
	counts, err := s.Incr(context.Background(), counters)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	return counts
}

func TestStoreValuesLastForTheirTTL(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		err := s.Set(context.Background(), []Entry{
			{Key: "brief", Value: "x", TTL: 150 * time.Millisecond},
			{Key: "lasting", Value: "y", TTL: time.Minute},
		})
		if err != nil {
			t.Fatalf("Set: %v", err)
		}
		if got := mustGet(t, s, "lasting", "absent", "brief"); !slices.Equal(got, []string{"y", "", "x"}) {
			t.Fatalf("Get = %q, want the values in the order asked and \"\" for the absent key", got)
		}
		time.Sleep(250 * time.Millisecond)
		if got := mustGet(t, s, "lasting", "brief"); !slices.Equal(got, []string{"y", ""}) {
			t.Errorf("Get after the short TTL = %q, want only the lasting value", got)
		}

		if err := s.Del(context.Background(), []string{"lasting", "absent"}); err != nil {
			t.Fatalf("Del: %v", err)
		}
		if got := mustGet(t, s, "lasting"); got[0] != "" {
			t.Errorf("Get after Del = %q", got)
		}
	})
}

func TestStoreCountersCountWithinAWindowThatStartsAtTheFirstCount(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		a, b := Counter{Key: "a", Window: time.Second}, Counter{Key: "b", Window: time.Minute}

		if got := mustIncr(t, s, a, b); !slices.Equal(got, []int64{1, 1}) {
			t.Fatalf("first counts = %v", got)
		}
		time.Sleep(600 * time.Millisecond)
		if got := mustIncr(t, s, a); got[0] != 2 {
			t.Fatalf("second count = %v, want 2", got)
		}
		if got := mustGet(t, s, "a"); got[0] != "2" {
			t.Errorf("a counter reads as %q, want its count", got[0])
		}
		// 1.2s after the first count: had the second one extended the
		// window, the counter would still be there.
		time.Sleep(600 * time.Millisecond)
		if got := mustIncr(t, s, a, b); !slices.Equal(got, []int64{1, 2}) {
			t.Errorf("counts after a's window = %v, want a started over and b still counting", got)
		}
	})
}

func TestStoresTakeNothingAsNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if values, err := s.Get(ctx, nil); err != nil || len(values) != 0 {
			t.Errorf("Get of no keys = %v, %v", values, err)
		}
		if counts, err := s.Incr(ctx, nil); err != nil || len(counts) != 0 {
			t.Errorf("Incr of no counters = %v, %v", counts, err)
		}
		if err := s.Set(ctx, nil); err != nil {
			t.Errorf("Set of no entries: %v", err)
		}
		if err := s.Del(ctx, nil); err != nil {
			t.Errorf("Del of no keys: %v", err)
		}
	})
}

func answering(_ context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
	return &fakeStream{}, &provider.ChatChunk{ID: t.CredentialID}, nil
}

// Two routers over one store are two instances of the gateway.
func TestAFailureOnOneInstanceIsSkippedOnAnother(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		first, second := New(s, nil), New(s, nil)
		// A model with a colon in it must not spill into another key segment.
		limited := target("cred:1", provider.VendorOpenRouter, "meta/llama:free")
		route := &store.ResolvedRoute{MaxHops: 3, Targets: []store.ResolvedTarget{
			limited, target("cred:2", provider.VendorMistral, "m"),
		}}

		_, err := first.Run(context.Background(), route,
			func(ctx context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
				if t == limited {
					return nil, nil, failure(provider.ErrRateLimited, 0)
				}
				return answering(ctx, t)
			})
		if err != nil {
			t.Fatal(err)
		}
		first.Wait()

		res, err := second.Run(context.Background(), route, answering)
		if err != nil {
			t.Fatal(err)
		}
		wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeOK)
	})
}

// downStore fails every call, as Redis does when it is unreachable.
type downStore struct{ calls atomic.Int64 }

var errDown = errors.New("connection refused")

func (d *downStore) Get(context.Context, []string) ([]string, error) {
	d.calls.Add(1)
	return nil, errDown
}

func (d *downStore) Incr(context.Context, []Counter) ([]int64, error) {
	d.calls.Add(1)
	return nil, errDown
}

func (d *downStore) Set(context.Context, []Entry) error {
	d.calls.Add(1)
	return errDown
}

func (d *downStore) Del(context.Context, []string) error {
	d.calls.Add(1)
	return errDown
}

func TestRoutingCarriesOnInProcessWhenTheSharedStoreIsDown(t *testing.T) {
	down := &downStore{}
	r := New(down, nil)
	limited := target("a", provider.VendorGroq, "m1")
	route := &store.ResolvedRoute{MaxHops: 3, Targets: []store.ResolvedTarget{limited, target("b", provider.VendorMistral, "m1")}}

	res, err := r.Run(context.Background(), route,
		func(ctx context.Context, t store.ResolvedTarget) (provider.Stream, *provider.ChatChunk, error) {
			if t == limited {
				return nil, nil, failure(provider.ErrRateLimited, 0)
			}
			return answering(ctx, t)
		})
	if err != nil {
		t.Fatalf("a store outage failed the request: %v", err)
	}
	wantOutcomes(t, res.Steps, OutcomeRateLimited, OutcomeOK)
	r.Wait()

	// Past the in-process copy's second, so the skip is read back from the
	// fallback rather than from the copy.
	time.Sleep(copyTTL + 100*time.Millisecond)
	res, err = r.Run(context.Background(), route, answering)
	if err != nil {
		t.Fatal(err)
	}
	wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeOK)

	if got := down.calls.Load(); got != 1 {
		t.Errorf("the failing store was called %d times, want once: it is left alone while it cools off", got)
	}
}
