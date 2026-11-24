package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/budget"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// Budget checks a request against its workspace's and key's monthly budgets,
// prices the answer and counts it. *budget.Budgets is the production one.
type Budget interface {
	Check(ctx context.Context, accounts []budget.Account) budget.Verdict
	Charge(t store.ResolvedTarget, prompt, completion int64) budget.Charge
	Add(ctx context.Context, workspaceID string, accounts []budget.Account, micros int64) error
}

const (
	headerCostBasis       = "X-Budget-Cost-Basis"
	typeInsufficientQuota = "insufficient_quota"
)

func budgetAccounts(key store.ActiveGatewayKey, route *store.ResolvedRoute) []budget.Account {
	return []budget.Account{
		{Scope: budget.ScopeWorkspace, ID: key.WorkspaceID, Limit: route.Settings.Budget},
		{Scope: budget.ScopeKey, ID: key.ID, Limit: key.Budget},
	}
}

func anyBudget(accounts []budget.Account) bool {
	for _, a := range accounts {
		if a.Limit != nil {
			return true
		}
	}
	return false
}

// budgetTargets returns the targets a cache miss may call. A spent blocking
// budget leaves only the free-tier ones, so a budget of 0 means free tiers
// only; with none of those it writes the 429 and returns false. An unpriced
// target does not count as free, since its cost is unknown.
func (d Deps) budgetTargets(w http.ResponseWriter, r *http.Request, accounts []budget.Account, targets []store.ResolvedTarget) ([]store.ResolvedTarget, bool) {
	if d.Budget == nil || !anyBudget(accounts) {
		return targets, true
	}
	v := d.Budget.Check(r.Context(), accounts)
	if v.Known {
		w.Header().Set("X-Budget-Remaining-USD", usd(v.Remaining))
	}
	if v.Exceeded == nil {
		return targets, true
	}
	var free []store.ResolvedTarget
	for _, t := range targets {
		if d.Budget.Charge(t, 0, 0).Basis == budget.Free {
			free = append(free, t)
		}
	}
	if len(free) > 0 {
		return free, true
	}
	whose := "This workspace's"
	if v.Exceeded.Scope == budget.ScopeKey {
		whose = "This gateway key's"
	}
	resets := budget.PeriodOf(time.Now()).ResetsAt().Format("2006-01-02")
	// No Retry-After: SDKs would sleep on it, and the wait is weeks.
	writeError(w, http.StatusTooManyRequests, typeInsufficientQuota, "budget_exceeded",
		whose+" monthly budget of "+budget.USD(v.Exceeded.Limit.MicroUSD)+" is spent. "+
			"It resets on "+resets+" at 00:00 UTC, or can be raised in the dashboard.")
	return nil, false
}

func usd(micros int64) string {
	return strconv.FormatFloat(float64(micros)/1e6, 'f', -1, 64)
}

// chargeBudgets prices a finished request and counts it against its budgets,
// detached like the reconcile. The result is the usage event's cost: nil when
// the answer could not be priced.
func (d Deps) chargeBudgets(ctx context.Context, o *observation, key store.ActiveGatewayKey, s *spent) *int64 {
	var zero int64
	switch {
	case !o.answered || o.cached:
		return &zero
	case d.Budget == nil:
		return nil
	}
	// A generation that started and reported nothing is priced at the
	// estimate, as prompt tokens: the only figure there is.
	prompt, completion := int64(o.estimate), int64(0)
	if s.usage != nil {
		prompt, completion = int64(s.usage.PromptTokens), int64(s.usage.CompletionTokens)
	}
	c := d.Budget.Charge(o.target, prompt, completion)
	if c.MicroUSD > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
			defer cancel()
			if err := d.Budget.Add(ctx, key.WorkspaceID, o.budgets, c.MicroUSD); err != nil {
				d.logger().WarnContext(ctx, "budget spend not counted",
					slog.String("request_id", telemetry.RequestIDFrom(ctx)),
					slog.String("error", err.Error()))
			}
		}()
	}
	if c.Basis == budget.Unpriced {
		return nil
	}
	return &c.MicroUSD
}
