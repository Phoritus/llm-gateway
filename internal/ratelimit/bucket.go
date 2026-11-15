package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// rejections counts denied checks. It resolves against the global meter on
// first use of the package, so it is a no-op until telemetry.Setup installs a
// provider.
var rejections, _ = otel.Meter("llm-gateway/ratelimit").Int64Counter("ratelimit.rejections",
	metric.WithDescription("Checks the token bucket denied, by scope."))

//go:embed bucket.lua
var bucketScript string

// Rule describes one bucket: how much can burst at once and how fast it
// refills.
type Rule struct {
	// Burst is the bucket capacity, the most that can be spent at once.
	Burst float64
	// Refill is the sustained rate in tokens per second.
	Refill float64
	// TTL is how long an untouched bucket survives in Redis. The spec keeps
	// every shared value expiring so no state outlives its usefulness.
	TTL time.Duration
}

// PerMinute builds a rule allowing n requests a minute, with the whole
// minute's worth available as burst.
func PerMinute(n float64) Rule {
	return Rule{Burst: n, Refill: n / 60, TTL: time.Hour}
}

// PerHour builds a rule allowing n requests an hour.
func PerHour(n float64) Rule {
	return Rule{Burst: n, Refill: n / 3600, TTL: 2 * time.Hour}
}

// Decision is the outcome of one check.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until the request would succeed. Zero when
	// allowed.
	RetryAfter time.Duration
}

// Limiter is a Redis-backed token bucket shared by every instance. AllowN
// serves the control plane's auth endpoints, one bucket a call; Take, Settle
// and Peek serve the data plane, which works on several buckets at once.
type Limiter struct {
	client *redis.Client
	script *redis.Script
	take   *redis.Script
	peek   *redis.Script
	prefix string
}

// New builds a limiter. The script is registered once and invoked with
// EVALSHA, so a check is a single round trip carrying only the hash.
func New(client *redis.Client) *Limiter {
	return &Limiter{
		client: client,
		script: redis.NewScript(bucketScript),
		take:   redis.NewScript(takeScript),
		peek:   redis.NewScript(peekScript),
		prefix: "rl",
	}
}

// AllowN spends cost tokens. The data plane uses a cost above one to reserve
// estimated tokens before a provider call. A denial is counted under the
// scope label, so scope must stay a fixed name chosen by the caller
// ("login:ip"), never something derived from the request: each distinct
// value becomes a metric series.
func (l *Limiter) AllowN(ctx context.Context, scope, id string, rule Rule, cost float64) (Decision, error) {
	if rule.Refill <= 0 || rule.Burst <= 0 {
		return Decision{}, fmt.Errorf("ratelimit: rule for %q must have a positive burst and refill", scope)
	}
	ttl := rule.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}

	// The caller supplies the clock so that every instance refills from one
	// timeline, rather than each trusting its own.
	now := time.Now().UnixMilli()

	res, err := l.script.Run(ctx, l.client,
		[]string{l.key(scope, id)},
		rule.Burst, rule.Refill, now, cost, ttl.Milliseconds(),
	).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: evaluate bucket: %w", err)
	}
	// The script also returns the tokens left in the bucket, as res[1]. Nothing
	// reads it yet; the length check keeps the Go side pinned to the script's
	// contract so a change to either is a compile-or-test failure rather than a
	// silent reinterpretation of the slice.
	if len(res) != 3 {
		return Decision{}, fmt.Errorf("ratelimit: bucket script returned %d values, want 3", len(res))
	}

	if res[0] != 1 {
		rejections.Add(ctx, 1, metric.WithAttributes(attribute.String("scope", scope)))
	}

	return Decision{
		Allowed:    res[0] == 1,
		RetryAfter: time.Duration(res[2]) * time.Millisecond,
	}, nil
}

// Reset clears a bucket. A successful login drops the failure bucket for that
// account so a legitimate user is not held back by their own earlier typos.
func (l *Limiter) Reset(ctx context.Context, scope, id string) error {
	if err := l.client.Del(ctx, l.key(scope, id)).Err(); err != nil {
		return fmt.Errorf("ratelimit: reset bucket: %w", err)
	}
	return nil
}

// key follows the rl:{scope}:{id} pattern from the spec's Redis table.
func (l *Limiter) key(scope, id string) string {
	// Colons would let one identifier impersonate another scope, and an
	// address or email is caller-supplied.
	return l.prefix + ":" + strings.ReplaceAll(scope, ":", "_") + ":" + strings.ReplaceAll(id, ":", "_")
}
