package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

//go:embed take.lua
var takeScript string

//go:embed peek.lua
var peekScript string

// Bucket is one bucket and what this call spends from it. Scope is the metric
// label of a refusal, so it must be a fixed name, exactly as for AllowN.
type Bucket struct {
	Scope, ID string
	Rule      Rule
	Cost      float64
}

// Result is the outcome of one Take.
type Result struct {
	Allowed bool
	// RetryAfter is the longest wait among the buckets that refused. Zero
	// when allowed.
	RetryAfter time.Duration
	// Remaining is the tokens left in each bucket after the call, in the
	// order the buckets were given.
	Remaining []float64
}

// Take admits only if every bucket holds its cost, then spends all of them in
// one atomic step. A refusal spends nothing.
//
// A cost above a bucket's Burst is clamped to Burst, for the check and for the
// spend: unclamped it could never be admitted. The caller reconciles the rest
// with Settle.
func (l *Limiter) Take(ctx context.Context, buckets []Bucket) (Result, error) {
	if err := checkRules(buckets); err != nil {
		return Result{}, err
	}
	res, refused, err := l.run(ctx, buckets, false)
	if err != nil {
		return Result{}, err
	}
	countRejections(ctx, buckets, refused)
	return res, nil
}

// Settle applies each Cost unconditionally. A negative cost refunds, up to
// Burst. A positive one adds debt: the bucket may go below zero, down to
// -Burst, so a single huge answer cannot lock it for more than about two
// refill periods.
func (l *Limiter) Settle(ctx context.Context, buckets []Bucket) error {
	if err := checkRules(buckets); err != nil {
		return err
	}
	_, _, err := l.run(ctx, buckets, true)
	return err
}

// Peek returns the level each bucket would have now, in the order given. It
// is read-only: no bucket is spent, created or kept alive, Cost is ignored,
// and nothing is counted as a rejection.
func (l *Limiter) Peek(ctx context.Context, buckets []Bucket) ([]float64, error) {
	if err := checkRules(buckets); err != nil {
		return nil, err
	}
	if len(buckets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(buckets))
	args := make([]any, 0, 1+2*len(buckets))
	args = append(args, time.Now().UnixMilli())
	for i, b := range buckets {
		keys[i] = l.key(b.Scope, b.ID)
		args = append(args, b.Rule.Burst, b.Rule.Refill)
	}
	raw, err := l.peek.Run(ctx, l.client, keys, args...).Int64Slice()
	if err != nil {
		return nil, fmt.Errorf("ratelimit: peek buckets: %w", err)
	}
	if len(raw) != len(buckets) {
		return nil, fmt.Errorf("ratelimit: peek script returned %d values, want %d", len(raw), len(buckets))
	}
	levels := make([]float64, len(raw))
	for i, v := range raw {
		levels[i] = float64(v) / 1000
	}
	return levels, nil
}

// run evaluates the script. Its second result marks the buckets that refused.
func (l *Limiter) run(ctx context.Context, buckets []Bucket, force bool) (Result, []bool, error) {
	if len(buckets) == 0 {
		return Result{Allowed: true}, nil, nil
	}
	keys := make([]string, len(buckets))
	// As in AllowN, the caller supplies the clock so every instance refills
	// from one timeline.
	args := make([]any, 0, 2+4*len(buckets))
	args = append(args, time.Now().UnixMilli(), boolArg(force))
	for i, b := range buckets {
		keys[i] = l.key(b.Scope, b.ID)
		args = append(args, b.Rule.Burst, b.Rule.Refill, b.spend(force), b.Rule.ttl().Milliseconds())
	}

	raw, err := l.take.Run(ctx, l.client, keys, args...).Int64Slice()
	if err != nil {
		return Result{}, nil, fmt.Errorf("ratelimit: evaluate buckets: %w", err)
	}
	if len(raw) != 1+2*len(buckets) {
		return Result{}, nil, fmt.Errorf("ratelimit: take script returned %d values, want %d", len(raw), 1+2*len(buckets))
	}

	res := Result{Allowed: raw[0] == 1, Remaining: make([]float64, len(buckets))}
	refused := make([]bool, len(buckets))
	for i := range buckets {
		res.Remaining[i] = float64(raw[1+2*i]) / 1000
		retry := time.Duration(raw[2+2*i]) * time.Millisecond
		refused[i] = retry > 0
		res.RetryAfter = max(res.RetryAfter, retry)
	}
	return res, refused, nil
}

// spend is the cost as applied: clamped to Burst for a Take, as given for a
// Settle.
func (b Bucket) spend(force bool) float64 {
	if force {
		return b.Cost
	}
	return min(b.Cost, b.Rule.Burst)
}

func boolArg(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r Rule) ttl() time.Duration {
	if r.TTL <= 0 {
		return time.Hour
	}
	return r.TTL
}

func checkRules(buckets []Bucket) error {
	for _, b := range buckets {
		if b.Rule.Refill <= 0 || b.Rule.Burst <= 0 {
			return fmt.Errorf("ratelimit: rule for %q must have a positive burst and refill", b.Scope)
		}
	}
	return nil
}

// countRejections counts a refusal once under each refusing bucket's scope.
func countRejections(ctx context.Context, buckets []Bucket, refused []bool) {
	for i, b := range buckets {
		if refused[i] {
			rejections.Add(ctx, 1, metric.WithAttributes(attribute.String("scope", b.Scope)))
		}
	}
}
