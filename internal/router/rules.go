package router

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

// Outcome is how one target fared. It is a header value and a log field, so
// it comes from this fixed set and never from the upstream.
type Outcome string

const (
	OutcomeOK               Outcome = "ok"
	OutcomeSkipped          Outcome = "skipped"
	OutcomeRateLimited      Outcome = "rate_limited"
	OutcomeQuota            Outcome = "quota"
	OutcomeAuth             Outcome = "auth"
	OutcomeModelUnavailable Outcome = "model_unavailable"
	OutcomeContextTooLong   Outcome = "context_too_long"
	OutcomeInvalidRequest   Outcome = "invalid_request"
	OutcomeTimeout          Outcome = "timeout"
	OutcomeUpstreamError    Outcome = "upstream_error"
	OutcomeUnavailable      Outcome = "unavailable"
)

// ErrNoAdapter is what an Attempt returns for a target whose vendor this
// gateway has no adapter for.
var ErrNoAdapter = errors.New("no adapter for the target's vendor")

// maxHint bounds how long an upstream's Retry-After is believed for. The
// header is the upstream's to write, and a nonsense value would otherwise
// bench a credential for as long as it says.
const maxHint = 24 * time.Hour

// subject is what a skip applies to: every target sharing the subject's key
// is skipped with it.
type subject struct {
	prefix string
	id     func(store.ResolvedTarget) string
}

var (
	credential = subject{"skip:c:", func(t store.ResolvedTarget) string {
		return escape(t.CredentialID)
	}}
	credentialModel = subject{"skip:cm:", func(t store.ResolvedTarget) string {
		return escape(t.CredentialID) + ":" + escape(t.Model)
	}}
	// vendor is the circuit breaker's subject, the only one that spans
	// workspaces.
	vendor = subject{"cb:", func(t store.ResolvedTarget) string {
		return escape(string(t.Vendor))
	}}

	subjects = []subject{credential, credentialModel, vendor}
)

func (s subject) key(t store.ResolvedTarget) string { return s.prefix + s.id(t) }

// escaper keeps a model or an id from spanning key segments. Models are
// user-defined and several upstreams put colons in theirs.
var escaper = strings.NewReplacer("%", "%25", ":", "%3A")

func escape(s string) string { return escaper.Replace(s) }

// effect is one skip a failure places.
type effect struct {
	subject subject
	// ttl is how long the skip lasts, given the upstream's Retry-After (zero
	// when it sent none) and how many times running the effect has fired.
	ttl func(hint time.Duration, repeat int64) time.Duration
	// counter, when set, names a count of these failures kept for window.
	// The skip is placed only once the count reaches threshold.
	counter   string
	window    time.Duration
	threshold int64
}

func fixed(d time.Duration) func(time.Duration, int64) time.Duration {
	return func(time.Duration, int64) time.Duration { return d }
}

// hintOr believes the upstream about when to come back, and re-checks after d
// when it did not say.
func hintOr(d time.Duration) func(time.Duration, int64) time.Duration {
	return func(hint time.Duration, _ int64) time.Duration {
		if hint > 0 {
			return min(hint, maxHint)
		}
		return d
	}
}

// backoff doubles base on each repeat up to ceiling, and never undercuts the
// upstream's own hint.
func backoff(base, ceiling time.Duration) func(time.Duration, int64) time.Duration {
	return func(hint time.Duration, repeat int64) time.Duration {
		grown := ceiling
		if doublings := repeat - 1; doublings < 16 {
			grown = min(ceiling, base<<doublings)
		}
		return max(grown, min(hint, maxHint))
	}
}

// breakerCounter counts a vendor's failures across all workspaces. A success
// clears it, so the breaker opens on failures in a row rather than on a busy
// vendor's ordinary error rate or on one workspace's slow model.
const breakerCounter = "cbn:"

// transient is what a 5xx or a timeout places: the vendor's breaker, and a
// bench for the one credential and model that keeps failing.
var transient = []effect{
	{subject: vendor, ttl: fixed(30 * time.Second), counter: breakerCounter, window: time.Minute, threshold: 5},
	{subject: credentialModel, ttl: fixed(10 * time.Minute), counter: "bench:", window: 15 * time.Minute, threshold: 3},
}

// rule says what one kind of failure is called and what it skips. A rule with
// no effects stores nothing: the next target is tried and the next request
// starts clean.
type rule struct {
	kind    error
	outcome Outcome
	// limit marks the failures answered 429 when the route is exhausted.
	limit   bool
	effects []effect
}

var upstreamRule = rule{kind: provider.ErrUpstream, outcome: OutcomeUpstreamError, effects: transient}

// rules is the skip-rule table.
//
// ErrQuota covers both an account out of credit and a daily quota, which the
// adapters cannot tell apart. It skips the narrower subject and, without a
// reset time from the upstream, re-checks hourly.
var rules = []rule{
	{kind: provider.ErrAuth, outcome: OutcomeAuth, effects: []effect{
		{subject: credential, ttl: fixed(5 * time.Minute)},
	}},
	{kind: provider.ErrQuota, outcome: OutcomeQuota, limit: true, effects: []effect{
		{subject: credentialModel, ttl: hintOr(time.Hour)},
	}},
	{kind: provider.ErrModelUnavailable, outcome: OutcomeModelUnavailable, effects: []effect{
		{subject: credentialModel, ttl: fixed(24 * time.Hour)},
	}},
	{kind: provider.ErrRateLimited, outcome: OutcomeRateLimited, limit: true, effects: []effect{
		{subject: credential, ttl: backoff(90*time.Second, 15*time.Minute),
			counter: "rlrep:", window: 15 * time.Minute, threshold: 1},
	}},
	upstreamRule,
	{kind: provider.ErrTimeout, outcome: OutcomeTimeout, effects: transient},
	// The model is skipped for the rest of this request only, which Run does.
	{kind: provider.ErrContextTooLong, outcome: OutcomeContextTooLong},
	// Adapters differ in what they accept, so the next target may take it.
	{kind: provider.ErrInvalidRequest, outcome: OutcomeInvalidRequest},
	{kind: ErrNoAdapter, outcome: OutcomeUnavailable},
}

// ruleFor classifies a failed attempt. An error no adapter classified is
// treated as the upstream failing.
func ruleFor(err error) rule {
	for _, r := range rules {
		if errors.Is(err, r.kind) {
			return r
		}
	}
	return upstreamRule
}

func isLimit(o Outcome) bool {
	for _, r := range rules {
		if r.outcome == o {
			return r.limit
		}
	}
	return false
}

// skip is a stored reason not to try a subject, and when it ends.
type skip struct {
	outcome Outcome
	until   time.Time
}

func (s skip) encode() string {
	return string(s.outcome) + "|" + strconv.FormatInt(s.until.UnixMilli(), 10)
}

func decodeSkip(value string) (skip, bool) {
	outcome, ms, ok := strings.Cut(value, "|")
	if !ok {
		return skip{}, false
	}
	until, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return skip{}, false
	}
	return skip{outcome: Outcome(outcome), until: time.UnixMilli(until)}, true
}
