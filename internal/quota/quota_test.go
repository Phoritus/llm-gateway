package quota

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

func limit(n int) *int { return &n }

// rows is a Source a test can change and break.
type rows struct {
	mu     sync.Mutex
	quotas []store.FreeQuota
	err    error
}

func (r *rows) FreeQuotas(context.Context) ([]store.FreeQuota, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quotas, r.err
}

func (r *rows) set(quotas []store.FreeQuota, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quotas, r.err = quotas, err
}

var (
	paid    = store.ResolvedTarget{CredentialID: "c-openai", Vendor: provider.VendorOpenAI, Model: "gpt"}
	groq    = store.ResolvedTarget{CredentialID: "c-groq", Vendor: provider.VendorGroq, Model: "llama"}
	unknown = store.ResolvedTarget{CredentialID: "c-groq", Vendor: provider.VendorGroq, Model: "not-in-the-catalog"}
	gemini  = store.ResolvedTarget{CredentialID: "c-gemini", Vendor: provider.VendorGemini, Model: "flash"}
	chain   = []store.ResolvedTarget{paid, groq, unknown, gemini}
)

var freeTiers = []store.FreeQuota{
	{Vendor: "groq", Model: "llama", RPM: limit(2), RPD: limit(100), TPM: limit(1000), TPD: limit(5000)},
	// A tier that publishes no token limits.
	{Vendor: "gemini", Model: "flash", RPM: limit(5), RPD: limit(50)},
}

func tracker(t *testing.T, limiter Limiter, quotas []store.FreeQuota) *Tracker {
	t.Helper()
	var c Catalog
	if err := c.Load(context.Background(), &rows{quotas: quotas}); err != nil {
		t.Fatal(err)
	}
	return New(&c, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func models(targets []store.ResolvedTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Model
	}
	return out
}

func wantOrder(t *testing.T, got []store.ResolvedTarget, want ...store.ResolvedTarget) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", models(got), models(want))
	}
}

func spend(t *testing.T, tr *Tracker, target store.ResolvedTarget, tokens float64) {
	t.Helper()
	if err := tr.Spend(context.Background(), target, tokens); err != nil {
		t.Fatalf("Spend: %v", err)
	}
}

func TestPrioritizePutsTargetsWithFreeQuotaFirstInTheRoutesOrder(t *testing.T) {
	tr := tracker(t, ratelimit.NewLocal(), freeTiers)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), groq, gemini, paid, unknown)
	if !slices.Equal(chain, []store.ResolvedTarget{paid, groq, unknown, gemini}) {
		t.Fatal("Prioritize reordered the slice it was given")
	}
}

func TestATargetOutOfFreeRequestsIsTriedLaterNotDropped(t *testing.T) {
	tr := tracker(t, ratelimit.NewLocal(), freeTiers)
	spend(t, tr, groq, 10)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), groq, gemini, paid, unknown)

	spend(t, tr, groq, 10)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), gemini, paid, groq, unknown)
}

func TestATargetWithoutRoomForTheRequestsTokensIsTriedLater(t *testing.T) {
	tr := tracker(t, ratelimit.NewLocal(), freeTiers)
	spend(t, tr, groq, 900)

	wantOrder(t, tr.Prioritize(context.Background(), chain, 100), groq, gemini, paid, unknown)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 200), gemini, paid, groq, unknown)
}

func TestARequestLargerThanTheWholeTierStillFitsAFullBucket(t *testing.T) {
	tr := tracker(t, ratelimit.NewLocal(), freeTiers)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 1e6), groq, gemini, paid, unknown)
}

func TestFreeQuotaIsCountedPerCredential(t *testing.T) {
	tr := tracker(t, ratelimit.NewLocal(), freeTiers)
	other := groq
	other.CredentialID = "c-groq-2"
	spend(t, tr, groq, 10)
	spend(t, tr, groq, 10)

	wantOrder(t, tr.Prioritize(context.Background(), []store.ResolvedTarget{groq, other}, 10), other, groq)
}

func TestAnAccountWideTierIsSharedByEveryModelOnTheCredential(t *testing.T) {
	gemma := store.ResolvedTarget{CredentialID: "c-or", Vendor: provider.VendorOpenRouter, Model: "gemma:free"}
	nemo := store.ResolvedTarget{CredentialID: "c-or", Vendor: provider.VendorOpenRouter, Model: "nemo:free"}
	otherKey := store.ResolvedTarget{CredentialID: "c-or-2", Vendor: provider.VendorOpenRouter, Model: "nemo:free"}
	tr := tracker(t, ratelimit.NewLocal(), []store.FreeQuota{
		{Vendor: "openrouter", Model: "gemma:free", RPD: limit(2), PerAccount: true},
		{Vendor: "openrouter", Model: "nemo:free", RPD: limit(2), PerAccount: true},
	})
	spend(t, tr, gemma, 10)
	spend(t, tr, gemma, 10)

	targets := []store.ResolvedTarget{paid, nemo, otherKey}
	wantOrder(t, tr.Prioritize(context.Background(), targets, 10), otherKey, paid, nemo)
}

func TestSpendingOnATargetWithNoTierCountsNothing(t *testing.T) {
	local := ratelimit.NewLocal()
	tr := tracker(t, local, freeTiers)
	spend(t, tr, paid, 10)
	spend(t, tr, unknown, 10)

	// Had the unknown model been counted on groq's key, groq would be out of
	// its two requests a minute.
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), groq, gemini, paid, unknown)
}

func TestAnEmptyCatalogKeepsTheRoutesOrder(t *testing.T) {
	tr := New(&Catalog{}, failing{}, nil)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), chain...)
	spend(t, tr, groq, 10)
}

// failing is a limiter that is down.
type failing struct{}

var errDown = errors.New("limiter down")

func (failing) Peek(context.Context, []ratelimit.Bucket) ([]float64, error) { return nil, errDown }
func (failing) Settle(context.Context, []ratelimit.Bucket) error            { return errDown }

func TestBucketsThatCannotBeReadKeepTheRoutesOrder(t *testing.T) {
	tr := tracker(t, failing{}, freeTiers)
	wantOrder(t, tr.Prioritize(context.Background(), chain, 10), chain...)
	if err := tr.Spend(context.Background(), groq, 10); !errors.Is(err, errDown) {
		t.Fatalf("Spend = %v, want the limiter's error", err)
	}
}

func TestDailyBucketsOutliveAnIdleHour(t *testing.T) {
	for _, b := range buckets(groq, Limits{RPM: limit(2), RPD: limit(100), TPM: limit(1000), TPD: limit(5000)}, 40) {
		daily := b.Scope == "free:rpd" || b.Scope == "free:tpd"
		if daily != (b.Rule.TTL == 25*time.Hour) {
			t.Errorf("%s has a TTL of %s", b.Scope, b.Rule.TTL)
		}
		if tokens := b.Scope == "free:tpm" || b.Scope == "free:tpd"; tokens != (b.Cost == 40) {
			t.Errorf("%s costs %v", b.Scope, b.Cost)
		}
		if b.ID != "c-groq:llama" {
			t.Errorf("%s is keyed by %q, want the credential and model", b.Scope, b.ID)
		}
	}
}

func TestAFailedLoadKeepsThePreviousCatalog(t *testing.T) {
	src := &rows{quotas: freeTiers}
	var c Catalog
	if err := c.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	src.set(nil, errors.New("database down"))
	if err := c.Load(context.Background(), src); err == nil {
		t.Fatal("Load swallowed the source's error")
	}
	if _, ok := c.limits(groq); !ok {
		t.Fatal("a failed load emptied the catalog")
	}

	src.set(freeTiers[1:], nil)
	if err := c.Load(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.limits(groq); ok {
		t.Fatal("a row removed from the source stayed in the catalog")
	}
}

func TestRunPicksUpACorrectedRow(t *testing.T) {
	src := &rows{}
	var c Catalog
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, src, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	src.set(freeTiers, nil)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		if l, ok := c.limits(groq); ok && *l.RPM == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog was never reloaded")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}
