package budget

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

// Basis says why a request cost what it did. It is a response header and an
// API field, so it is one of these constants.
type Basis string

const (
	// Listed is a model with a row in model_prices.
	Listed Basis = "listed"
	// Free is a model in the free-quota catalog, which costs nothing.
	Free Basis = "free"
	// Unpriced is a model with no price row: a custom endpoint, or a model
	// newer than the table. It costs nothing rather than being refused, so
	// that adding a model never waits on a price.
	Unpriced Basis = "unpriced"
	// Cached is an answer from the cache, which calls no upstream.
	Cached Basis = "cached"
)

// Charge is what one request costs against its budgets.
type Charge struct {
	MicroUSD int64
	Basis    Basis
}

// picoPerMicro converts a price in pico-USD per token to micro-USD.
const picoPerMicro = 1_000_000

// cost is the tokens at the price, rounded up to a whole micro-USD.
func cost(p store.ModelPrice, prompt, completion int64) int64 {
	pico := prompt*p.Input + completion*p.Output
	return (pico + picoPerMicro - 1) / picoPerMicro
}

type entry struct {
	vendor provider.Vendor
	model  string
}

// PriceSource is where prices are read from. *store.Store is the production one.
type PriceSource interface {
	ModelPrices(ctx context.Context) ([]store.ModelPrice, error)
}

// Prices is model_prices in memory. The zero value prices nothing.
type Prices struct {
	entries atomic.Pointer[map[entry]store.ModelPrice]
}

// Load replaces the prices with what the source holds now. When the read
// fails the prices are left as they were.
func (p *Prices) Load(ctx context.Context, src PriceSource) error {
	rows, err := src.ModelPrices(ctx)
	if err != nil {
		return err
	}
	entries := make(map[entry]store.ModelPrice, len(rows))
	for _, r := range rows {
		entries[entry{provider.Vendor(r.Vendor), r.Model}] = r
	}
	p.entries.Store(&entries)
	return nil
}

// Run reloads the prices every interval until ctx ends, so a corrected row
// takes effect without a restart.
func (p *Prices) Run(ctx context.Context, src PriceSource, every time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := p.Load(ctx, src); err != nil && ctx.Err() == nil {
				logger.WarnContext(ctx, "model price refresh failed: keeping the previous prices",
					slog.String("error", err.Error()))
			}
		}
	}
}

func (p *Prices) lookup(v provider.Vendor, model string) (store.ModelPrice, bool) {
	if p == nil {
		return store.ModelPrice{}, false
	}
	entries := p.entries.Load()
	if entries == nil {
		return store.ModelPrice{}, false
	}
	price, ok := (*entries)[entry{v, model}]
	return price, ok
}

// FreeTiers says whether a model has a free tier. *quota.Catalog is the
// production one.
type FreeTiers interface {
	Lists(v provider.Vendor, model string) bool
}

// Pricer prices an answer from the target that gave it.
type Pricer struct {
	Prices *Prices
	Free   FreeTiers
}

// Charge is what the target's answer of these tokens costs. A free-tier
// model is free even when it also has a price, since a free key pays nothing.
func (p Pricer) Charge(t store.ResolvedTarget, prompt, completion int64) Charge {
	if p.Free != nil && p.Free.Lists(t.Vendor, t.Model) {
		return Charge{Basis: Free}
	}
	price, ok := p.Prices.lookup(t.Vendor, t.Model)
	if !ok {
		return Charge{Basis: Unpriced}
	}
	return Charge{MicroUSD: cost(price, prompt, completion), Basis: Listed}
}
