package store

import (
	"context"
	"testing"
	"time"
)

func TestWriteBatchRollsUpCostAndUnpriced(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	costs := func(micros int64) func(*UsageEvent) {
		return func(e *UsageEvent) { e.CostMicroUSD = &micros }
	}

	writeUsage(t, st,
		usageEvent(ws, "chat", noon, costs(1500)),
		usageEvent(ws, "chat", noon, costs(250)),
		usageEvent(ws, "chat", noon),
	)

	days := dailyOf(t, st, ws, "chat")
	if len(days) != 1 || days[0].CostMicroUSD != 1750 || days[0].Unpriced != 1 {
		t.Fatalf("rollup = %+v, want 1750 micro-USD and 1 unpriced", days)
	}
	recent, err := st.Recent(context.Background(), ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	var priced, unpriced int
	for _, e := range recent {
		if e.CostMicroUSD == nil {
			unpriced++
		} else {
			priced++
		}
	}
	if priced != 2 || unpriced != 1 {
		t.Errorf("recent has %d priced and %d unpriced events, want 2 and 1", priced, unpriced)
	}
}

func TestModelPricesReadsTheSeedInPicoUSD(t *testing.T) {
	st := newTestStore(t)
	prices, err := st.ModelPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prices {
		if p.Vendor == "groq" && p.Model == "openai/gpt-oss-20b" {
			if p.Input != 75_000 || p.Output != 300_000 || p.CachedInput != nil {
				t.Errorf("gpt-oss-20b = %+v, want 0.075 and 0.30 USD per million tokens, no cached price", p)
			}
			return
		}
	}
	t.Fatalf("no seeded price for groq's gpt-oss-20b among %d rows", len(prices))
}

func TestAGatewayKeyBudgetIsStoredAndAuthenticatedWithTheKey(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	alice := signup(t, st, "alice@example.com")
	bob := signup(t, st, "bob@example.com")
	key, err := st.CreateGatewayKey(ctx, alice.WorkspaceID, NewGatewayKey{
		Name: "ci", Prefix: "gwk_ci", Hash: hashOf("ci"), Budget: &Budget{MicroUSD: 5_000_000, Action: BudgetWarn},
	})
	if err != nil {
		t.Fatal(err)
	}
	if key.Budget == nil || *key.Budget != (Budget{MicroUSD: 5_000_000, Action: BudgetWarn}) {
		t.Fatalf("created key's budget = %+v", key.Budget)
	}

	if _, err := st.SetGatewayKeyBudget(ctx, bob.WorkspaceID, key.ID, nil); err != ErrNotFound {
		t.Fatalf("another workspace's budget change = %v, want ErrNotFound", err)
	}
	if _, err := st.SetGatewayKeyBudget(ctx, alice.WorkspaceID, key.ID, &Budget{MicroUSD: 0, Action: BudgetBlock}); err != nil {
		t.Fatal(err)
	}
	active, err := st.ActiveGatewayKeyByHash(ctx, hashOf("ci"))
	if err != nil {
		t.Fatal(err)
	}
	if active.Budget == nil || *active.Budget != (Budget{MicroUSD: 0, Action: BudgetBlock}) {
		t.Errorf("authenticated key's budget = %+v, want a blocking budget of 0", active.Budget)
	}

	email, name, err := st.BudgetContact(ctx, alice.WorkspaceID, key.ID)
	if err != nil || email != "alice@example.com" || name != "ci" {
		t.Errorf("contact = %q, %q, %v", email, name, err)
	}
	if _, name, _ := st.BudgetContact(ctx, bob.WorkspaceID, key.ID); name != "" {
		t.Errorf("bob's contact names alice's key %q", name)
	}
}
