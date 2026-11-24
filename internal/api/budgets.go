package api

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"time"

	"llm-gateway/internal/budget"
	"llm-gateway/internal/store"
)

// BudgetLedger reads what each account spent this month. *budget.Ledger is
// the production one.
type BudgetLedger interface {
	Spent(ctx context.Context, accounts []budget.Account) ([]int64, error)
	Period() budget.Period
}

// maxBudgetUSD bounds a monthly budget far above any real one, and far below
// what micro-USD in an int64 can hold.
const maxBudgetUSD = 1e9

// budgetFields is a budget as the API writes and reads it: USD, null for no
// budget, and the action, block unless stated.
type budgetFields struct {
	MonthlyBudgetUSD *float64           `json:"monthly_budget_usd"`
	BudgetAction     store.BudgetAction `json:"budget_action"`
}

func budgetFieldsOf(b *store.Budget) budgetFields {
	if b == nil {
		return budgetFields{BudgetAction: store.BudgetBlock}
	}
	amount := usdOf(b.MicroUSD)
	return budgetFields{MonthlyBudgetUSD: &amount, BudgetAction: b.Action}
}

// budget validates the fields, writing the 400 when they are wrong.
func (f *budgetFields) budget(w http.ResponseWriter) (*store.Budget, bool) {
	switch f.BudgetAction {
	case "":
		f.BudgetAction = store.BudgetBlock
	case store.BudgetBlock, store.BudgetWarn:
	default:
		invalidField(w, "budget_action", "budget_action must be block or warn.")
		return nil, false
	}
	if f.MonthlyBudgetUSD == nil {
		return nil, true
	}
	usd := *f.MonthlyBudgetUSD
	if usd < 0 || usd > maxBudgetUSD {
		invalidField(w, "monthly_budget_usd", "monthly_budget_usd must be a number of USD from 0 to 1000000000, or null for no budget.")
		return nil, false
	}
	return &store.Budget{MicroUSD: int64(math.Round(usd * 1e6)), Action: f.BudgetAction}, true
}

func usdOf(micros int64) float64 { return float64(micros) / 1e6 }

// spent reads the accounts' spend this month. Without a ledger, or when it
// cannot be read, every amount is unknown.
func (d Deps) spent(ctx context.Context, accounts []budget.Account) []*float64 {
	out := make([]*float64, len(accounts))
	if d.Budgets == nil || len(accounts) == 0 {
		return out
	}
	micros, err := d.Budgets.Spent(ctx, accounts)
	if err != nil {
		d.logger().WarnContext(ctx, "budget spend not read", slog.String("error", err.Error()))
		return out
	}
	for i, m := range micros {
		usd := usdOf(m)
		out[i] = &usd
	}
	return out
}

type budgetView struct {
	budgetFields
	SpentThisMonthUSD *float64  `json:"spent_this_month_usd"`
	Period            string    `json:"period"`
	ResetsAt          time.Time `json:"resets_at"`
}

func (d Deps) workspaceBudget(ctx context.Context, workspaceID string, b *store.Budget) budgetView {
	period := budget.PeriodOf(time.Now())
	if d.Budgets != nil {
		period = d.Budgets.Period()
	}
	return budgetView{
		budgetFields:      budgetFieldsOf(b),
		SpentThisMonthUSD: d.spent(ctx, []budget.Account{{Scope: budget.ScopeWorkspace, ID: workspaceID}})[0],
		Period:            string(period),
		ResetsAt:          period.ResetsAt(),
	}
}
