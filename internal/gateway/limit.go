package gateway

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// RateLimiter spends several buckets as one step and corrects them afterwards.
// *ratelimit.FailOpen is the production one; *ratelimit.Local serves tests.
type RateLimiter interface {
	Take(ctx context.Context, buckets []ratelimit.Bucket) (ratelimit.Result, error)
	Settle(ctx context.Context, buckets []ratelimit.Bucket) error
}

// Limits is what one gateway key and one workspace may spend. The values come
// from config; nothing here defaults them.
type Limits struct {
	KeyRequests       ratelimit.Rule
	KeyTokens         ratelimit.Rule
	WorkspaceRequests ratelimit.Rule
	WorkspaceTokens   ratelimit.Rule
	// WorkspaceConcurrency is how many requests a workspace may have in
	// flight on this instance.
	WorkspaceConcurrency int
}

// The scopes are the label on the rejections metric, so they are fixed names.
const (
	scopeKeyRequests       = "v1:key:rpm"
	scopeKeyTokens         = "v1:key:tpm"
	scopeWorkspaceRequests = "v1:ws:rpm"
	scopeWorkspaceTokens   = "v1:ws:tpm"
	scopeAuthFailIP        = "v1:authfail:ip"
)

// authFailPerIP bounds how often one address may present a missing or invalid
// key. An unknown but well-formed key costs a Postgres lookup, and this is the
// only thing in front of it.
var authFailPerIP = ratelimit.PerMinute(30)

// settleTimeout bounds the reconcile, which runs after the response and is
// waited on by nobody.
const settleTimeout = 2 * time.Second

// estimateTokens is the spec's four characters per token, applied to the
// request body. It covers the prompt only: the completion's size is unknown
// until the upstream reports it, and the reconcile adds it then.
func estimateTokens(bodyBytes int) float64 {
	return math.Ceil(float64(bodyBytes) / 4)
}

// reserve takes one request and the estimated tokens from the key's and the
// workspace's buckets, all or nothing, and writes the 429 when any refuses.
// It returns the token buckets with what was actually taken from each, for
// settle.
//
// A limiter error lets the request through with nothing reserved. /v1 fails
// open: see ratelimit.FailOpen, which is what keeps this branch from being
// reached in production.
func (d Deps) reserve(w http.ResponseWriter, r *http.Request, key store.ActiveGatewayKey, estimate float64) ([]ratelimit.Bucket, bool) {
	tokens := []ratelimit.Bucket{
		{Scope: scopeKeyTokens, ID: key.ID, Rule: d.Limits.KeyTokens, Cost: estimate},
		{Scope: scopeWorkspaceTokens, ID: key.WorkspaceID, Rule: d.Limits.WorkspaceTokens, Cost: estimate},
	}
	res, err := d.Limiter.Take(r.Context(), []ratelimit.Bucket{
		{Scope: scopeKeyRequests, ID: key.ID, Rule: d.Limits.KeyRequests, Cost: 1},
		{Scope: scopeWorkspaceRequests, ID: key.WorkspaceID, Rule: d.Limits.WorkspaceRequests, Cost: 1},
		tokens[0], tokens[1],
	})
	if err != nil {
		d.logger().WarnContext(r.Context(), "rate limit check failed, request admitted",
			slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		return nil, true
	}

	// OpenAI's header names, which its SDKs read. Each is the tighter of the
	// key's and the workspace's bucket, since that is the one a client hits.
	h := w.Header()
	h.Set("X-RateLimit-Remaining-Requests", remaining(res.Remaining[0], res.Remaining[1]))
	h.Set("X-RateLimit-Remaining-Tokens", remaining(res.Remaining[2], res.Remaining[3]))

	if !res.Allowed {
		tooManyRequests(w, res.RetryAfter)
		return nil, false
	}
	for i := range tokens {
		// Take clamps a cost to the burst, so that is what there is to settle
		// against.
		tokens[i].Cost = min(tokens[i].Cost, tokens[i].Rule.Burst)
	}
	return tokens, true
}

// remaining formats the lower of two bucket levels. A bucket in debt reads as
// empty: a negative count means nothing to a client.
func remaining(a, b float64) string {
	return strconv.Itoa(int(math.Floor(max(0, min(a, b)))))
}

// spent is what a request turned out to cost upstream.
type spent struct {
	// started is set once the upstream produced a chunk, which is when it
	// began generating and so began costing tokens.
	started bool
	usage   *provider.Usage
}

func (s *spent) observe(c *provider.ChatChunk) {
	s.started = true
	if c.Usage != nil {
		u := *c.Usage
		s.usage = &u
	}
}

// settle corrects the token reservation to what the request really cost: the
// reported usage when there was one, nothing when the upstream failed before
// producing a chunk. A generation that started and reported no usage keeps its
// reservation, the only figure there is. The request count is never refunded.
//
// It runs detached from the request so that neither the client nor a
// disconnect waits on it.
func (d Deps) settle(ctx context.Context, reserved []ratelimit.Bucket, s *spent) {
	var actual float64
	switch {
	case s.usage != nil:
		actual = float64(s.usage.TotalTokens)
	case s.started:
		return
	}

	var corrections []ratelimit.Bucket
	for _, b := range reserved {
		if diff := actual - b.Cost; diff != 0 {
			b.Cost = diff
			corrections = append(corrections, b)
		}
	}
	if len(corrections) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		if err := d.Limiter.Settle(ctx, corrections); err != nil {
			d.logger().WarnContext(ctx, "rate limit reconcile failed",
				slog.String("request_id", telemetry.RequestIDFrom(ctx)),
				slog.String("error", err.Error()))
		}
	}()
}

// inflight counts each workspace's requests in progress.
//
// The count is per instance by design. Phase 1 runs one instance, and a count
// held in Redis would leak a slot whenever an instance died mid-request, which
// needs its own expiry scheme to undo.
type inflight struct {
	mu    sync.Mutex
	count map[string]int
}

// acquire takes a slot without waiting. The caller must call release once,
// and only when ok.
func (f *inflight) acquire(workspaceID string, limit int) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count[workspaceID] >= limit {
		return nil, false
	}
	f.count[workspaceID]++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.count[workspaceID]--; f.count[workspaceID] == 0 {
			delete(f.count, workspaceID)
		}
	}, true
}

// authBlocks remembers the addresses the failed-authentication limit has
// refused, so that their next requests are turned away without a key lookup
// or a round trip to Redis.
//
// An entry may outlive the second an in-process copy of shared state is
// normally allowed. It copies a Redis bucket that cannot refill before the
// RetryAfter it reported, so until then every instance would refuse too.
type authBlocks struct {
	mu        sync.Mutex
	until     map[string]time.Time
	nextPrune time.Time
}

// wait is how long the address is still blocked for, or zero.
func (b *authBlocks) wait(ip string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, time.Until(b.until[ip]))
}

func (b *authBlocks) block(ip string, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.After(b.nextPrune) {
		b.nextPrune = now.Add(time.Second)
		for addr, until := range b.until {
			if now.After(until) {
				delete(b.until, addr)
			}
		}
	}
	b.until[ip] = now.Add(wait)
}

// countAuthFailure spends one failed authentication from the client's address.
// When the address has none left it writes the 429 and reports false; the
// caller then says nothing more.
func (d Deps) countAuthFailure(w http.ResponseWriter, r *http.Request, ip string) bool {
	res, err := d.Limiter.Take(r.Context(), []ratelimit.Bucket{
		{Scope: scopeAuthFailIP, ID: ip, Rule: authFailPerIP, Cost: 1},
	})
	if err != nil {
		d.logger().WarnContext(r.Context(), "rate limit check failed, request admitted",
			slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
			slog.String("error", err.Error()))
		return true
	}
	if !res.Allowed {
		d.blocks.block(ip, res.RetryAfter)
		tooManyRequests(w, res.RetryAfter)
		return false
	}
	return true
}

// tooManyRequests answers a refusal by one of the gateway's own limits.
// Retry-After is rounded up: a client told to come back a moment early is
// refused a second time.
func tooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := max(1, int(math.Ceil(retryAfter.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, typeRateLimit, "rate_limit_exceeded",
		"Rate limit exceeded. Try again in "+strconv.Itoa(seconds)+"s.")
}

func tooManyConcurrentRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusTooManyRequests, typeRateLimit, "too_many_concurrent_requests",
		"This workspace has too many requests in progress. Try again when one finishes.")
}
