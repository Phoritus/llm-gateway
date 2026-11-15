package gateway

import (
	"context"
	"log/slog"

	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// FreeQuota orders a free-first route's targets and counts what each answer
// used of its target's free tier. *quota.Tracker is the production one.
type FreeQuota interface {
	Prioritize(ctx context.Context, targets []store.ResolvedTarget, estimate float64) []store.ResolvedTarget
	Spend(ctx context.Context, target store.ResolvedTarget, tokens float64) error
}

// spendFree counts an answered request against its target's free tier: the
// reported tokens, or the estimate when the upstream reported none. Every
// route spends, free-first or not, because the quota is the credential's
// whichever route used it. It runs detached, like the reconcile.
func (d Deps) spendFree(ctx context.Context, t store.ResolvedTarget, s *spent, estimate float64) {
	if d.Quota == nil {
		return
	}
	tokens := estimate
	if s.usage != nil {
		tokens = float64(s.usage.TotalTokens)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		if err := d.Quota.Spend(ctx, t, tokens); err != nil {
			d.logger().WarnContext(ctx, "free quota count failed",
				slog.String("request_id", telemetry.RequestIDFrom(ctx)),
				slog.String("error", err.Error()))
		}
	}()
}
