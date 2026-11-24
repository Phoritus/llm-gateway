package keycheck

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

func TestResultMapsEachProviderError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want store.KeyCheckResult
	}{
		{"nil", nil, store.KeyCheckOK},
		{"auth", &provider.Error{Kind: provider.ErrAuth}, store.KeyCheckRejected},
		{"model unavailable", &provider.Error{Kind: provider.ErrModelUnavailable}, store.KeyCheckRejected},
		{"quota", &provider.Error{Kind: provider.ErrQuota}, store.KeyCheckQuotaExhausted},
		{"rate limited", &provider.Error{Kind: provider.ErrRateLimited}, store.KeyCheckRateLimited},
		{"timeout", &provider.Error{Kind: provider.ErrTimeout}, store.KeyCheckProviderTimeout},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), store.KeyCheckProviderTimeout},
		{"upstream", &provider.Error{Kind: provider.ErrUpstream}, store.KeyCheckProviderError},
		{"other", errors.New("boom"), store.KeyCheckProviderError},
	} {
		if got := Result(tc.err); got != tc.want {
			t.Errorf("%s: Result = %q, want %q", tc.name, got, tc.want)
		}
	}
}

type fakeProvider struct {
	err  error
	seen []provider.Credential
}

func (f *fakeProvider) CheckKey(_ context.Context, cred provider.Credential) error {
	f.seen = append(f.seen, cred)
	return f.err
}

func (*fakeProvider) ChatStream(context.Context, *provider.ChatRequest, provider.Credential) (provider.Stream, error) {
	return nil, &provider.Error{Kind: provider.ErrUpstream}
}

func TestCheckPassesTheSecretAndReportsAnUnavailableVendor(t *testing.T) {
	fp := &fakeProvider{err: &provider.Error{Kind: provider.ErrAuth}}
	providers := map[provider.Vendor]provider.Provider{provider.VendorGroq: fp}

	got, err := Check(context.Background(), providers, store.UnsealedCredential{Vendor: provider.VendorGroq, Secret: "s3cret"})
	if got != store.KeyCheckRejected || err == nil {
		t.Fatalf("Check = %q, %v, want key_rejected with the error", got, err)
	}
	if len(fp.seen) != 1 || fp.seen[0].APIKey != "s3cret" {
		t.Fatalf("adapter saw %+v", fp.seen)
	}

	got, err = Check(context.Background(), providers, store.UnsealedCredential{Vendor: provider.VendorGemini})
	if got != store.KeyCheckProviderUnavailable || err != nil {
		t.Fatalf("no adapter: Check = %q, %v", got, err)
	}
}

type fakeStore struct {
	due           []store.DueCheck
	unsealErr     map[string]error
	recordErr     map[string]error
	recorded      map[string]store.KeyCheckResult
	claimInterval time.Duration
	claimLimit    int
}

func (s *fakeStore) ClaimDueChecks(_ context.Context, interval time.Duration, limit int) ([]store.DueCheck, error) {
	s.claimInterval, s.claimLimit = interval, limit
	return s.due, nil
}

func (s *fakeStore) Unseal(_ context.Context, _, id string) (store.UnsealedCredential, error) {
	if err := s.unsealErr[id]; err != nil {
		return store.UnsealedCredential{}, err
	}
	return store.UnsealedCredential{Vendor: provider.VendorGroq, Secret: "k-" + id}, nil
}

func (s *fakeStore) RecordCheck(_ context.Context, _, id string, check store.KeyCheck) error {
	if err := s.recordErr[id]; err != nil {
		return err
	}
	s.recorded[id] = check.Result
	return nil
}

func TestCheckDueRecordsOneResultPerClaimedCredential(t *testing.T) {
	st := &fakeStore{
		due: []store.DueCheck{
			{WorkspaceID: "w", ID: "ok"},
			{WorkspaceID: "w", ID: "gone"},
			{WorkspaceID: "w", ID: "deleted-meanwhile"},
			{WorkspaceID: "w", ID: "last"},
		},
		unsealErr: map[string]error{"gone": store.ErrNotFound},
		recordErr: map[string]error{"deleted-meanwhile": store.ErrNotFound},
		recorded:  map[string]store.KeyCheckResult{},
	}
	c := &Checker{
		Store:     st,
		Providers: map[provider.Vendor]provider.Provider{provider.VendorGroq: &fakeProvider{}},
		Interval:  24 * time.Hour,
		PerTick:   7,
	}

	n, err := c.CheckDue(context.Background())
	if err != nil {
		t.Fatalf("CheckDue: %v", err)
	}
	if n != 3 {
		t.Errorf("checked = %d, want 3 (the unsealable one is skipped)", n)
	}
	if st.claimInterval != 24*time.Hour || st.claimLimit != 7 {
		t.Errorf("claimed with interval=%v limit=%d", st.claimInterval, st.claimLimit)
	}
	if len(st.recorded) != 2 || st.recorded["ok"] != store.KeyCheckOK || st.recorded["last"] != store.KeyCheckOK {
		t.Errorf("recorded = %+v", st.recorded)
	}
}

func TestCheckDueStopsOnAStoreError(t *testing.T) {
	boom := errors.New("db down")
	st := &fakeStore{
		due:       []store.DueCheck{{WorkspaceID: "w", ID: "a"}, {WorkspaceID: "w", ID: "b"}},
		recordErr: map[string]error{"b": boom},
		recorded:  map[string]store.KeyCheckResult{},
	}
	c := &Checker{Store: st, Providers: map[provider.Vendor]provider.Provider{provider.VendorGroq: &fakeProvider{}}}

	n, err := c.CheckDue(context.Background())
	if !errors.Is(err, boom) || n != 1 {
		t.Fatalf("CheckDue = %d, %v, want 1 and the store error", n, err)
	}
}
