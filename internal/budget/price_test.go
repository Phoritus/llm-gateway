package budget

import (
	"context"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

type priceRows []store.ModelPrice

func (r priceRows) ModelPrices(context.Context) ([]store.ModelPrice, error) { return r, nil }

type freeSet map[string]bool

func (f freeSet) Lists(v provider.Vendor, model string) bool { return f[string(v)+"/"+model] }

func pricer(t *testing.T, free freeSet, rows ...store.ModelPrice) Pricer {
	t.Helper()
	var p Prices
	if err := p.Load(context.Background(), priceRows(rows)); err != nil {
		t.Fatal(err)
	}
	return Pricer{Prices: &p, Free: free}
}

func target(vendor, model string) store.ResolvedTarget {
	return store.ResolvedTarget{Vendor: provider.Vendor(vendor), Model: model}
}

func TestAListedModelCostsItsPriceRoundedUpToAMicroUSD(t *testing.T) {
	// 0.075 and 0.30 USD per million tokens.
	p := pricer(t, nil, store.ModelPrice{Vendor: "groq", Model: "m", Input: 75_000, Output: 300_000})
	for _, c := range []struct {
		prompt, completion int64
		want               int64
	}{
		{1_000_000, 0, 75_000},
		{0, 1_000_000, 300_000},
		{1000, 1000, 375},
		{1, 0, 1}, // 0.075 micro-USD is charged as 1
		{0, 0, 0},
	} {
		got := p.Charge(target("groq", "m"), c.prompt, c.completion)
		if got != (Charge{MicroUSD: c.want, Basis: Listed}) {
			t.Errorf("%d in, %d out = %+v, want %d listed", c.prompt, c.completion, got, c.want)
		}
	}
}

func TestAFreeTierModelCostsNothingEvenWhenPriced(t *testing.T) {
	p := pricer(t, freeSet{"groq/m": true}, store.ModelPrice{Vendor: "groq", Model: "m", Input: 75_000, Output: 300_000})
	if got := p.Charge(target("groq", "m"), 1000, 1000); got != (Charge{Basis: Free}) {
		t.Errorf("charge = %+v, want free", got)
	}
}

func TestAModelWithNoPriceIsUnpricedAndCostsNothing(t *testing.T) {
	p := pricer(t, nil, store.ModelPrice{Vendor: "groq", Model: "m", Input: 1, Output: 1})
	for _, tg := range []store.ResolvedTarget{target("groq", "other"), target("openai", "m"), target("custom", "m")} {
		if got := p.Charge(tg, 1000, 1000); got != (Charge{Basis: Unpriced}) {
			t.Errorf("%s/%s = %+v, want unpriced", tg.Vendor, tg.Model, got)
		}
	}
}

func TestAnEmptyPricerPricesNothing(t *testing.T) {
	if got := (Pricer{}).Charge(target("openai", "m"), 10, 10); got != (Charge{Basis: Unpriced}) {
		t.Errorf("charge = %+v, want unpriced", got)
	}
}

func TestPeriodIsTheUTCCalendarMonth(t *testing.T) {
	bangkok := time.FixedZone("ICT", 7*3600)
	at := time.Date(2026, 12, 1, 3, 0, 0, 0, bangkok) // still November in UTC
	p := PeriodOf(at)
	if p != "2026-11" {
		t.Errorf("period = %s, want 2026-11", p)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !p.ResetsAt().Equal(want) {
		t.Errorf("resets at %s, want %s", p.ResetsAt(), want)
	}
	if got := PeriodOf(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)).ResetsAt(); got.Year() != 2027 || got.Month() != 1 {
		t.Errorf("December resets at %s", got)
	}
}
