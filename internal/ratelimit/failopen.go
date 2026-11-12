package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// primaryTimeout bounds one call to the primary. A limiter that is slow
	// is treated as one that is down: the request should not wait on it.
	primaryTimeout = 250 * time.Millisecond
	// primaryCoolOff is how long the primary is left alone after it fails, so
	// an outage does not add primaryTimeout to every request.
	primaryCoolOff = 5 * time.Second
)

// takeSettler is the pair of operations FailOpen forwards.
type takeSettler interface {
	Take(ctx context.Context, buckets []Bucket) (Result, error)
	Settle(ctx context.Context, buckets []Bucket) error
}

// FailOpen limits through the primary and, while that is failing, through the
// fallback.
//
// The data plane fails open where the control plane fails closed. Its limits
// protect a workspace's own provider budget rather than a credential, so an
// outage that loosens them costs the workspace some quota, while one that
// refused every request would take the gateway offline along with Redis.
// Readiness already drops an instance whose Redis is down.
//
// The two limiters share no state, so around an outage a reservation taken
// from one may be settled against the other. Either way the error is bounded
// by the bucket's own floor and ceiling.
type FailOpen struct {
	primary, fallback takeSettler
	logger            *slog.Logger
	// retryAt is when the primary may be tried again, in unix nanoseconds.
	retryAt atomic.Int64
	down    atomic.Bool
}

func NewFailOpen(primary *Limiter, fallback *Local, logger *slog.Logger) *FailOpen {
	return &FailOpen{primary: primary, fallback: fallback, logger: logger}
}

func (f *FailOpen) Take(ctx context.Context, buckets []Bucket) (Result, error) {
	var res Result
	err := f.do(ctx, func(ctx context.Context, l takeSettler) (err error) {
		res, err = l.Take(ctx, buckets)
		return err
	})
	return res, err
}

func (f *FailOpen) Settle(ctx context.Context, buckets []Bucket) error {
	return f.do(ctx, func(ctx context.Context, l takeSettler) error {
		return l.Settle(ctx, buckets)
	})
}

func (f *FailOpen) do(ctx context.Context, call func(context.Context, takeSettler) error) error {
	if time.Now().UnixNano() < f.retryAt.Load() {
		return call(ctx, f.fallback)
	}

	primaryCtx, cancel := context.WithTimeout(ctx, primaryTimeout)
	err := call(primaryCtx, f.primary)
	cancel()
	if err == nil {
		if f.down.CompareAndSwap(true, false) {
			f.logger.InfoContext(ctx, "rate limiter recovered: limiting through Redis again")
		}
		return nil
	}
	// A caller that went away says nothing about the primary's health.
	if ctx.Err() == nil {
		f.retryAt.Store(time.Now().Add(primaryCoolOff).UnixNano())
		// Logged on the transition, not per request: an outage would otherwise
		// write one line for every call it degrades.
		if f.down.CompareAndSwap(false, true) {
			f.logger.WarnContext(ctx, "rate limiter unavailable: limiting in process until it recovers",
				slog.String("error", err.Error()))
		}
	}
	return call(ctx, f.fallback)
}
