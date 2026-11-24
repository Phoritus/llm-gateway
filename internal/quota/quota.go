// Package quota is free-first routing's view of the free tiers: the catalog of
// what each vendor allows one key on one model, the buckets that count what
// this gateway has used of it, and the ordering that puts targets with free
// quota left ahead of the rest.
//
// The counts are the gateway's own. An upstream may have been used from
// elsewhere, so a target that looks exhausted is tried later rather than
// dropped, and a real refusal is the router's to handle.
package quota

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

// Limits is one free tier: requests and tokens, per minute and per day. A nil
// limit is one the vendor does not report, and is not counted. PerAccount
// limits are the credential's across all its per-account models.
type Limits struct {
	RPM, RPD, TPM, TPD *int
	PerAccount         bool
}

type entry struct {
	vendor provider.Vendor
	model  string
}

// Source is where the catalog is read from. *store.Store is the production one.
type Source interface {
	FreeQuotas(ctx context.Context) ([]store.FreeQuota, error)
}

// Catalog is the free tiers in memory. The zero value is an empty catalog,
// under which no target is free and every route keeps its own order.
type Catalog struct {
	entries atomic.Pointer[map[entry]Limits]
}

// Load replaces the catalog with what the source holds now. When the read
// fails the catalog is left as it was.
func (c *Catalog) Load(ctx context.Context, src Source) error {
	rows, err := src.FreeQuotas(ctx)
	if err != nil {
		return err
	}
	entries := make(map[entry]Limits, len(rows))
	for _, r := range rows {
		entries[entry{provider.Vendor(r.Vendor), r.Model}] = Limits{RPM: r.RPM, RPD: r.RPD, TPM: r.TPM, TPD: r.TPD, PerAccount: r.PerAccount}
	}
	c.entries.Store(&entries)
	return nil
}

// Run reloads the catalog every interval until ctx ends, so a corrected row
// takes effect without a restart.
func (c *Catalog) Run(ctx context.Context, src Source, every time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := c.Load(ctx, src); err != nil && ctx.Err() == nil {
				logger.WarnContext(ctx, "free quota catalog refresh failed: keeping the previous catalog",
					slog.String("error", err.Error()))
			}
		}
	}
}

// Lists reports whether the model has a free tier in the catalog.
func (c *Catalog) Lists(v provider.Vendor, model string) bool {
	_, ok := c.limits(store.ResolvedTarget{Vendor: v, Model: model})
	return ok
}

func (c *Catalog) limits(t store.ResolvedTarget) (Limits, bool) {
	entries := c.entries.Load()
	if entries == nil {
		return Limits{}, false
	}
	l, ok := (*entries)[entry{t.Vendor, t.Model}]
	return l, ok
}

// dayTTL keeps a daily bucket for longer than its day, so an idle hour does
// not hand the whole day's allowance back.
const dayTTL = 25 * time.Hour

func perDay(n float64) ratelimit.Rule {
	return ratelimit.Rule{Burst: n, Refill: n / (24 * 60 * 60), TTL: dayTTL}
}

// windows is the four limits a free tier can carry. The scopes label the
// rate limiter's metrics, so they are fixed names.
var windows = []struct {
	scope  string
	limit  func(Limits) *int
	rule   func(float64) ratelimit.Rule
	tokens bool
}{
	{"free:rpm", func(l Limits) *int { return l.RPM }, ratelimit.PerMinute, false},
	{"free:rpd", func(l Limits) *int { return l.RPD }, perDay, false},
	{"free:tpm", func(l Limits) *int { return l.TPM }, ratelimit.PerMinute, true},
	{"free:tpd", func(l Limits) *int { return l.TPD }, perDay, true},
}

func buckets(t store.ResolvedTarget, l Limits, tokens float64) []ratelimit.Bucket {
	id := t.CredentialID + ":" + t.Model
	if l.PerAccount {
		id = t.CredentialID
	}
	var out []ratelimit.Bucket
	for _, w := range windows {
		n := w.limit(l)
		if n == nil {
			continue
		}
		cost := 1.0
		if w.tokens {
			cost = tokens
		}
		out = append(out, ratelimit.Bucket{
			Scope: w.scope, ID: id, Rule: w.rule(float64(*n)), Cost: cost,
		})
	}
	return out
}

// Limiter reads and corrects buckets. *ratelimit.FailOpen is the production
// one.
type Limiter interface {
	Peek(ctx context.Context, buckets []ratelimit.Bucket) ([]float64, error)
	Settle(ctx context.Context, buckets []ratelimit.Bucket) error
}

// Tracker counts what each credential has used of its free tiers.
type Tracker struct {
	catalog *Catalog
	limiter Limiter
	logger  *slog.Logger
}

// New builds a Tracker. A nil logger is slog.Default().
func New(catalog *Catalog, limiter Limiter, logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{catalog: catalog, limiter: limiter, logger: logger}
}

// Prioritize returns the targets with those that still have free quota first:
// a catalog entry, and room in every one of its buckets for one more request
// of the estimated tokens. The order within each group is the route's own.
// When the buckets cannot be read, so is the whole order.
func (t *Tracker) Prioritize(ctx context.Context, targets []store.ResolvedTarget, estimate float64) []store.ResolvedTarget {
	var (
		all []ratelimit.Bucket
		// span[i] is where target i's buckets sit in all; listed says it has
		// a catalog entry.
		span   = make([][2]int, len(targets))
		listed = make([]bool, len(targets))
		found  bool
	)
	for i, target := range targets {
		limits, ok := t.catalog.limits(target)
		if !ok {
			continue
		}
		listed[i], found = true, true
		from := len(all)
		all = append(all, buckets(target, limits, estimate)...)
		span[i] = [2]int{from, len(all)}
	}
	if !found {
		return targets
	}
	levels, err := t.limiter.Peek(ctx, all)
	if err != nil {
		t.logger.WarnContext(ctx, "free quota could not be read: keeping the route's order",
			slog.String("error", err.Error()))
		return targets
	}

	free := make([]store.ResolvedTarget, 0, len(targets))
	var rest []store.ResolvedTarget
	for i, target := range targets {
		if listed[i] && hasRoom(all[span[i][0]:span[i][1]], levels[span[i][0]:span[i][1]]) {
			free = append(free, target)
		} else {
			rest = append(rest, target)
		}
	}
	return append(free, rest...)
}

func hasRoom(buckets []ratelimit.Bucket, levels []float64) bool {
	for i, b := range buckets {
		// A request larger than the whole allowance could never have room,
		// so it is held to what a full bucket has, as ratelimit.Take does.
		if levels[i] < min(b.Cost, b.Rule.Burst) {
			return false
		}
	}
	return true
}

// Spend counts one request of the given tokens against the target's free
// tier. A target with no catalog entry has none, and nothing is counted.
func (t *Tracker) Spend(ctx context.Context, target store.ResolvedTarget, tokens float64) error {
	limits, ok := t.catalog.limits(target)
	if !ok {
		return nil
	}
	return t.limiter.Settle(ctx, buckets(target, limits, tokens))
}
