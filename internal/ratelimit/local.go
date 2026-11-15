package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// localSweepEvery is how often Local drops the buckets nobody has touched for
// their TTL. Redis expires its keys itself; here a call does it on the way in.
const localSweepEvery = time.Minute

// Local is an in-process limiter with the semantics of Limiter's Take, Settle
// and Peek. It is what the data plane falls back to when Redis is unreachable,
// and it is per instance: two instances each hand out a full allowance.
type Local struct {
	mu        sync.Mutex
	buckets   map[localKey]localBucket
	nextSweep time.Time
	now       func() time.Time
}

type localKey struct{ scope, id string }

type localBucket struct {
	tokens  float64
	at      time.Time
	expires time.Time
}

func NewLocal() *Local {
	return &Local{buckets: map[localKey]localBucket{}, now: time.Now}
}

// Take is Limiter.Take against this process's buckets.
func (l *Local) Take(ctx context.Context, buckets []Bucket) (Result, error) {
	if err := checkRules(buckets); err != nil {
		return Result{}, err
	}
	res, refused := l.run(buckets, false)
	countRejections(ctx, buckets, refused)
	return res, nil
}

// Settle is Limiter.Settle against this process's buckets.
func (l *Local) Settle(_ context.Context, buckets []Bucket) error {
	if err := checkRules(buckets); err != nil {
		return err
	}
	l.run(buckets, true)
	return nil
}

// Peek is Limiter.Peek against this process's buckets. A bucket past its TTL
// reads as full, as its expired key would in Redis.
func (l *Local) Peek(_ context.Context, buckets []Bucket) ([]float64, error) {
	if err := checkRules(buckets); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	levels := make([]float64, len(buckets))
	for i, b := range buckets {
		levels[i] = l.level(b, now)
	}
	return levels, nil
}

// level is what a bucket holds at now, after its refill.
func (l *Local) level(b Bucket, now time.Time) float64 {
	state, ok := l.buckets[localKey{b.Scope, b.ID}]
	if !ok || !now.Before(state.expires) {
		return b.Rule.Burst
	}
	elapsed := max(0, now.Sub(state.at).Seconds())
	return min(b.Rule.Burst, state.tokens+elapsed*b.Rule.Refill)
}

// run mirrors take.lua step for step; the tests hold the two to one table.
func (l *Local) run(buckets []Bucket, force bool) (Result, []bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	res := Result{Allowed: true, Remaining: make([]float64, len(buckets))}
	refused := make([]bool, len(buckets))
	for i, b := range buckets {
		have := l.level(b, now)
		res.Remaining[i] = have
		if cost := b.spend(force); !force && have < cost {
			res.Allowed, refused[i] = false, true
			wait := time.Duration(math.Ceil((cost-have)/b.Rule.Refill*1000)) * time.Millisecond
			res.RetryAfter = max(res.RetryAfter, wait)
		}
	}
	if !res.Allowed {
		return res, refused
	}

	for i, b := range buckets {
		left := max(-b.Rule.Burst, min(b.Rule.Burst, res.Remaining[i]-b.spend(force)))
		l.buckets[localKey{b.Scope, b.ID}] = localBucket{tokens: left, at: now, expires: now.Add(b.Rule.ttl())}
		res.Remaining[i] = left
	}
	return res, refused
}

func (l *Local) sweep(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(localSweepEvery)
	for key, state := range l.buckets {
		if !now.Before(state.expires) {
			delete(l.buckets, key)
		}
	}
}
