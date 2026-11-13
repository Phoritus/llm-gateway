package router

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// sharedTimeout bounds one call to the shared store. A read sits in front
	// of a provider call, so a store that is slow is treated as one that is
	// down.
	sharedTimeout = 100 * time.Millisecond
	// sharedCoolOff is how long the shared store is left alone after it
	// fails, so an outage does not add sharedTimeout to every request.
	sharedCoolOff = 5 * time.Second
)

// failOpen reads and writes skip state through the shared store and, while
// that is failing, through this instance's own memory.
//
// Routing fails open for the reason /v1's rate limiting does: skip state only
// saves attempts that would fail anyway, so losing it costs some wasted calls,
// while refusing to route without it would take the gateway down with Redis.
// Nothing here returns an error, because there is none a caller could act on.
type failOpen struct {
	shared Store
	local  *Memory
	logger *slog.Logger
	// retryAt is when the shared store may be tried again, in unix nanoseconds.
	retryAt atomic.Int64
	down    atomic.Bool
}

func (f *failOpen) Get(ctx context.Context, keys []string) (values []string) {
	f.do(ctx, func(ctx context.Context, s Store) (err error) {
		values, err = s.Get(ctx, keys)
		return err
	})
	return values
}

func (f *failOpen) Incr(ctx context.Context, counters []Counter) (counts []int64) {
	f.do(ctx, func(ctx context.Context, s Store) (err error) {
		counts, err = s.Incr(ctx, counters)
		return err
	})
	return counts
}

func (f *failOpen) Set(ctx context.Context, entries []Entry) {
	f.do(ctx, func(ctx context.Context, s Store) error { return s.Set(ctx, entries) })
}

func (f *failOpen) Del(ctx context.Context, keys []string) {
	f.do(ctx, func(ctx context.Context, s Store) error { return s.Del(ctx, keys) })
}

func (f *failOpen) do(ctx context.Context, call func(context.Context, Store) error) {
	if time.Now().UnixNano() >= f.retryAt.Load() {
		sharedCtx, cancel := context.WithTimeout(ctx, sharedTimeout)
		err := call(sharedCtx, f.shared)
		cancel()
		if err == nil {
			if f.down.CompareAndSwap(true, false) {
				f.logger.InfoContext(ctx, "router skip state recovered: shared through Redis again")
			}
			return
		}
		// A caller that went away says nothing about the store's health.
		if ctx.Err() == nil {
			f.retryAt.Store(time.Now().Add(sharedCoolOff).UnixNano())
			// Logged on the transition, not per call.
			if f.down.CompareAndSwap(false, true) {
				f.logger.WarnContext(ctx, "router skip state unavailable: kept in process until it recovers",
					slog.String("error", err.Error()))
			}
		}
	}
	_ = call(ctx, f.local)
}
