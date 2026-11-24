package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/budget"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

// fakeBudget answers every check with its verdict, prices with a real
// Pricer, and keeps what it was asked to add.
type fakeBudget struct {
	budget.Pricer
	mu      sync.Mutex
	verdict budget.Verdict
	added   []int64
}

func (b *fakeBudget) Check(context.Context, []budget.Account) budget.Verdict {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verdict
}

func (b *fakeBudget) Add(_ context.Context, _ string, _ []budget.Account, micros int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.added = append(b.added, micros)
	return nil
}

func (b *fakeBudget) set(v budget.Verdict) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verdict = v
}

func (b *fakeBudget) waitAdded(t *testing.T) []int64 {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		b.mu.Lock()
		added := append([]int64(nil), b.added...)
		b.mu.Unlock()
		if len(added) > 0 {
			return added
		}
	}
	t.Fatal("no spend was added")
	return nil
}

// echoPrice prices the mock's echo model at 1 USD per million tokens in and
// 2 out, which is 1 and 2 micro-USD a token.
var echoPrice = store.ModelPrice{Vendor: "mock", Model: "echo", Input: 1_000_000, Output: 2_000_000}

func pricedBudget(t *testing.T) *fakeBudget {
	t.Helper()
	var prices budget.Prices
	if err := prices.Load(context.Background(), priceTable{echoPrice}); err != nil {
		t.Fatal(err)
	}
	return &fakeBudget{Pricer: budget.Pricer{Prices: &prices}}
}

type priceTable []store.ModelPrice

func (p priceTable) ModelPrices(context.Context) ([]store.ModelPrice, error) { return p, nil }

// budgetedRoutes gives the workspace a budget, so the budget headers apply.
type budgetedRoutes struct {
	RouteResolver
	limit store.Budget
}

func (r budgetedRoutes) ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error) {
	route, err := r.RouteResolver.ResolveRoute(ctx, workspaceID, alias)
	if err == nil {
		route.Settings.Budget = &r.limit
	}
	return route, err
}

func budgetServer(t *testing.T, b *fakeBudget, action budget.Action) cacheFixture {
	t.Helper()
	return cacheServer(t, store.CacheAuto, func(d *Deps) {
		d.Budget = b
		d.Routes = budgetedRoutes{RouteResolver: d.Routes, limit: store.Budget{MicroUSD: 5_000_000, Action: action}}
	})
}

func spentWorkspace(limit store.Budget) budget.Verdict {
	return budget.Verdict{Known: true, Exceeded: &budget.Account{
		Scope: budget.ScopeWorkspace, ID: "ws1", Limit: &limit,
	}}
}

// freeModels is a free-quota catalog listing the mock models named.
type freeModels []string

func (f freeModels) Lists(v provider.Vendor, model string) bool {
	return v == provider.VendorMock && slices.Contains(f, model)
}

// A route of a listed and an unpriced target: neither is known to be free.
func TestASpentBlockingBudgetRefusesBeforeAnyUpstreamCall(t *testing.T) {
	b := pricedBudget(t)
	b.set(spentWorkspace(store.Budget{MicroUSD: 5_000_000, Action: budget.Block}))
	f := budgetServer(t, b, budget.Block)

	resp := post(t, f.srv, chatBody("echo,unpriced", false))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "budget_exceeded" || body.Error.Type != "insufficient_quota" {
		t.Errorf("error = %+v, want insufficient_quota / budget_exceeded", body.Error)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none: a client would sleep until next month", ra)
	}
	if got := resp.Header.Get("X-Budget-Remaining-USD"); got != "0" {
		t.Errorf("X-Budget-Remaining-USD = %q, want 0", got)
	}
	if n := f.upstreamCalls(); n != 0 {
		t.Errorf("the upstream was called %d times, want none", n)
	}
	e := recordedEvents(t, f.usage, 1)[0]
	if e.Status != http.StatusTooManyRequests || e.Outcome != outcomeBudgetExceeded || e.CostMicroUSD == nil || *e.CostMicroUSD != 0 {
		t.Errorf("event = %+v, want a refused request that cost nothing", e)
	}
}

func TestASpentBlockingBudgetStillRoutesToFreeTargets(t *testing.T) {
	b := pricedBudget(t)
	b.Free = freeModels{"gratis"}
	b.set(spentWorkspace(store.Budget{MicroUSD: 0, Action: budget.Block}))
	f := budgetServer(t, b, budget.Block)

	resp := post(t, f.srv, chatBody("echo,gratis", false))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the free target", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Budget-Cost-Basis"); got != "free" {
		t.Errorf("X-Budget-Cost-Basis = %q, want free", got)
	}
	f.calls.mu.Lock()
	defer f.calls.mu.Unlock()
	if !slices.Equal(f.calls.models, []string{"gratis"}) {
		t.Errorf("upstream calls = %v, want only the free target", f.calls.models)
	}
}

// checkless fails the test if a budget is checked.
type checkless struct {
	fakeBudget
	t *testing.T
}

func (c *checkless) Check(context.Context, []budget.Account) budget.Verdict {
	c.t.Error("Check was called with no budget set")
	return budget.Verdict{}
}

func TestWithoutABudgetNothingIsChecked(t *testing.T) {
	f := cacheServer(t, store.CacheAuto, func(d *Deps) { d.Budget = &checkless{t: t} })
	resp := post(t, f.srv, chatBody("echo", false))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestACacheHitIsServedPastABlockingBudget(t *testing.T) {
	b := pricedBudget(t)
	f := budgetServer(t, b, budget.Block)
	body := deterministic("echo", false, nil)
	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)

	b.set(spentWorkspace(store.Budget{MicroUSD: 5_000_000, Action: budget.Block}))
	resp := post(t, f.srv, body)
	wantCache(t, resp, "HIT")
	if got := resp.Header.Get("X-Budget-Cost-Basis"); got != "cached" {
		t.Errorf("X-Budget-Cost-Basis = %q, want cached", got)
	}
	if n := f.upstreamCalls(); n != 1 {
		t.Errorf("the upstream was called %d times, want only for the first request", n)
	}
}

func TestAWarningBudgetServesAndChargesTheAnswer(t *testing.T) {
	b := pricedBudget(t)
	b.set(budget.Verdict{Known: true, Remaining: 1_500_000})
	f := budgetServer(t, b, budget.Warn)

	resp := post(t, f.srv, chatBody("echo", false))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Budget-Remaining-USD"); got != "1.5" {
		t.Errorf("X-Budget-Remaining-USD = %q, want 1.5", got)
	}
	if got := resp.Header.Get("X-Budget-Cost-Basis"); got != "listed" {
		t.Errorf("X-Budget-Cost-Basis = %q, want listed", got)
	}
	e := recordedEvents(t, f.usage, 1)[0]
	want := int64(e.PromptTokens + 2*e.CompletionTokens)
	if e.CostMicroUSD == nil || *e.CostMicroUSD != want || want == 0 {
		t.Errorf("event cost = %v, want %d micro-USD", e.CostMicroUSD, want)
	}
	if added := b.waitAdded(t); added[0] != want {
		t.Errorf("added %v to the budgets, want %d", added, want)
	}
}

func TestAnUnpricedAnswerCostsNothingAndSaysSo(t *testing.T) {
	b := &fakeBudget{verdict: budget.Verdict{Known: true, Remaining: 5_000_000}}
	f := budgetServer(t, b, budget.Block)

	resp := post(t, f.srv, chatBody("echo", false))
	drain(resp)
	if got := resp.Header.Get("X-Budget-Cost-Basis"); got != "unpriced" {
		t.Errorf("X-Budget-Cost-Basis = %q, want unpriced", got)
	}
	if e := recordedEvents(t, f.usage, 1)[0]; e.CostMicroUSD != nil {
		t.Errorf("event cost = %d, want none: the answer has no price", *e.CostMicroUSD)
	}
	time.Sleep(20 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.added) != 0 {
		t.Errorf("added %v to the budgets, want nothing", b.added)
	}
}

func TestWithoutABudgetNoBudgetHeaderIsSet(t *testing.T) {
	f := cacheServer(t, store.CacheAuto, func(d *Deps) { d.Budget = pricedBudget(t) })
	resp := post(t, f.srv, chatBody("echo", false))
	drain(resp)
	for _, h := range []string{"X-Budget-Remaining-USD", "X-Budget-Cost-Basis"} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("%s = %q, want none", h, got)
		}
	}
}
