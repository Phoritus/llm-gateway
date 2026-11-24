package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/provider"
)

const testSecret = "sk-test-0123456789abcdefWXYZ"

func newTestCredentials(t *testing.T, st *Store) *Credentials {
	t.Helper()
	keys, err := crypto.NewKeyring([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return NewCredentials(st, keys)
}

func mustCreate(t *testing.T, c *Credentials, workspaceID, name string) *Credential {
	t.Helper()
	cred, err := c.Create(context.Background(), workspaceID,
		NewCredential{Provider: provider.VendorGroq, Name: name, Secret: testSecret})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return cred
}

func TestCredentialRoundTrip(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID

	first := mustCreate(t, c, ws, "first")
	second := mustCreate(t, c, ws, "second")
	if first.Last4 != "WXYZ" {
		t.Fatalf("last4 = %q", first.Last4)
	}

	list, err := c.List(ctx, ws)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v, want newest first", list)
	}

	got, err := c.Unseal(ctx, ws, first.ID)
	if err != nil || got.Secret != testSecret || got.Vendor != first.Provider {
		t.Fatalf("Unseal = %+v, %v", got, err)
	}

	var stored []byte
	if err := st.pool.QueryRow(ctx, `SELECT ciphertext FROM provider_credentials WHERE id = $1`, first.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(testSecret)) || bytes.Contains(stored, []byte("WXYZ")) {
		t.Fatal("ciphertext contains the plaintext")
	}

	if err := c.Delete(ctx, ws, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Unseal(ctx, ws, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Unseal after delete: %v", err)
	}
}

func TestCredentialsAreWorkspaceScoped(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	alice := signup(t, st, "a@example.com").WorkspaceID
	bob := signup(t, st, "b@example.com").WorkspaceID
	cred := mustCreate(t, c, alice, "alice")

	if list, err := c.List(ctx, bob); err != nil || len(list) != 0 {
		t.Fatalf("Bob's list = %+v, %v", list, err)
	}
	if _, err := c.Unseal(ctx, bob, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bob's Secret: %v", err)
	}
	if err := c.Delete(ctx, bob, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bob's Delete: %v", err)
	}
	if n := count(t, st, "provider_credentials"); n != 1 {
		t.Fatalf("rows = %d, want 1: Bob's delete removed Alice's row", n)
	}
}

func TestMalformedCredentialIDIsNotFound(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ws := signup(t, st, "a@example.com").WorkspaceID

	if _, err := c.Unseal(context.Background(), ws, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Secret: %v", err)
	}
	if err := c.Delete(context.Background(), ws, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete: %v", err)
	}
}

func TestCredentialCiphertextIsBoundToItsRow(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	other := signup(t, st, "b@example.com").WorkspaceID

	src := mustCreate(t, c, ws, "src")
	dst := mustCreate(t, c, ws, "dst")
	foreign := mustCreate(t, c, other, "foreign")

	copyOnto := func(from, to string) {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`UPDATE provider_credentials t SET key_id = s.key_id, ciphertext = s.ciphertext
			   FROM provider_credentials s WHERE s.id = $1 AND t.id = $2`, from, to); err != nil {
			t.Fatal(err)
		}
	}

	copyOnto(src.ID, dst.ID)
	if _, err := c.Unseal(ctx, ws, dst.ID); !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("ciphertext copied onto another row opened: %v", err)
	}

	copyOnto(foreign.ID, src.ID)
	if _, err := c.Unseal(ctx, ws, src.ID); !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("ciphertext copied from another tenant opened: %v", err)
	}
}

func TestCredentialProviderIsBoundToItsCiphertext(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	cred := mustCreate(t, c, ws, "x")

	if _, err := st.pool.Exec(ctx, `UPDATE provider_credentials SET provider = 'openai' WHERE id = $1`, cred.ID); err != nil {
		t.Fatal(err)
	}
	_, err := c.Unseal(ctx, ws, cred.ID)
	if !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("Unseal after provider change: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("error contains the secret")
	}
}

func TestACustomCredentialsBaseURLIsBoundToItsCiphertext(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	cred, err := c.Create(ctx, ws, NewCredential{Provider: provider.VendorCustom, Name: "mine", Secret: testSecret,
		BaseURL: "https://llm.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.BaseURL != "https://llm.example.com/v1" {
		t.Errorf("BaseURL = %q", cred.BaseURL)
	}
	open, err := c.Unseal(ctx, ws, cred.ID)
	if err != nil || open.Secret != testSecret || open.BaseURL != cred.BaseURL {
		t.Fatalf("Unseal = %+v, %v", open, err)
	}

	if _, err := st.pool.Exec(ctx, `UPDATE provider_credentials SET base_url = 'https://attacker.example/v1' WHERE id = $1`, cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Unseal(ctx, ws, cred.ID); !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("Unseal after the base URL changed: %v, want ErrOpen", err)
	}
}

func TestOnlyACustomCredentialHasABaseURL(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	if _, err := c.Create(ctx, ws, NewCredential{Provider: provider.VendorCustom, Name: "x", Secret: testSecret}); err == nil {
		t.Error("a custom credential without a base URL was stored")
	}
	if _, err := c.Create(ctx, ws, NewCredential{Provider: provider.VendorGroq, Name: "x", Secret: testSecret,
		BaseURL: "https://llm.example.com/v1"}); err == nil {
		t.Error("a groq credential with a base URL was stored")
	}
}

func TestCredentialLimit(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ws := signup(t, st, "a@example.com").WorkspaceID
	other := signup(t, st, "b@example.com").WorkspaceID

	for range maxCredentialsPerWorkspace {
		mustCreate(t, c, ws, "k")
	}
	_, err := c.Create(context.Background(), ws, NewCredential{Provider: provider.VendorGroq, Secret: testSecret})
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("51st create: %v", err)
	}
	mustCreate(t, c, other, "unaffected")
}

func TestCredentialLimitHoldsUnderConcurrentCreates(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID

	var wg sync.WaitGroup
	for range 4 * maxCredentialsPerWorkspace {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Create(ctx, ws, NewCredential{Provider: provider.VendorGroq, Secret: testSecret})
			if err != nil && !errors.Is(err, ErrLimitReached) {
				t.Errorf("concurrent create: %v", err)
			}
		}()
	}
	wg.Wait()

	creds, err := c.List(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != maxCredentialsPerWorkspace {
		t.Fatalf("workspace holds %d credentials, want exactly %d", len(creds), maxCredentialsPerWorkspace)
	}
}

func TestResolveRouteOpensTheChainInOrder(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	other := signup(t, st, "b@example.com").WorkspaceID

	first := mustCreate(t, c, ws, "first")
	second, err := c.Create(ctx, ws, NewCredential{Provider: provider.VendorMock, Secret: "sk-second-secret-0000"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRoute(ctx, ws, RouteSpec{Alias: "chat", MaxHops: 3, FreeFirst: true, Cache: CacheAlways, ContextHandoff: HandoffReorder, Targets: []RouteTarget{
		{CredentialID: second.ID, Model: "echo", FirstByteTimeout: 2 * time.Minute},
		{CredentialID: first.ID, Model: "llama-3"},
	}}); err != nil {
		t.Fatal(err)
	}

	route, err := c.ResolveRoute(ctx, ws, "chat")
	if err != nil {
		t.Fatal(err)
	}
	want := []ResolvedTarget{
		{CredentialID: second.ID, Vendor: provider.VendorMock, Model: "echo", Secret: "sk-second-secret-0000", FirstByteTimeout: 2 * time.Minute},
		{CredentialID: first.ID, Vendor: provider.VendorGroq, Model: "llama-3", Secret: testSecret},
	}
	if !route.FreeFirst || route.Cache != CacheAlways || route.Version == 0 {
		t.Fatalf("route settings = free_first %v cache %q version %d", route.FreeFirst, route.Cache, route.Version)
	}
	if route.MaxHops != 3 || len(route.Targets) != 2 || route.Targets[0] != want[0] || route.Targets[1] != want[1] {
		t.Fatalf("route = %+v, want %+v", route, want)
	}

	for name, args := range map[string][2]string{
		"another workspace": {other, "chat"},
		"another case":      {ws, "Chat"},
		"bad workspace id":  {"not-a-uuid", "chat"},
	} {
		if _, err := c.ResolveRoute(ctx, args[0], args[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestResealMovesEveryCredentialToTheCurrentKey(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	oldKey, newKey := []byte("0123456789abcdef0123456789abcdef"), []byte("fedcba9876543210fedcba9876543210")
	before := newTestCredentials(t, st)
	ws := signup(t, st, "a@example.com").WorkspaceID
	var ids []string
	for i := range 5 {
		ids = append(ids, mustCreate(t, before, ws, fmt.Sprintf("key %d", i)).ID)
	}
	custom, err := before.Create(ctx, ws, NewCredential{Provider: provider.VendorCustom, Name: "c", Secret: testSecret, BaseURL: "https://llm.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, custom.ID)

	rotated, err := crypto.NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	after := NewCredentials(st, rotated)
	n, err := after.Reseal(ctx, 2)
	if err != nil || n != len(ids) {
		t.Fatalf("Reseal = %d, %v; want %d", n, err, len(ids))
	}
	usage, err := after.KeyUsage(ctx)
	if err != nil || len(usage) != 1 || usage[rotated.Current()] != len(ids) {
		t.Fatalf("KeyUsage = %v, %v; want every row under the current key", usage, err)
	}
	if n, err := after.Reseal(ctx, 2); err != nil || n != 0 {
		t.Errorf("a second run = %d, %v; want nothing left to do", n, err)
	}

	newOnly, err := crypto.NewKeyring(newKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got, err := NewCredentials(st, newOnly).Unseal(ctx, ws, id); err != nil || got.Secret != testSecret {
			t.Errorf("credential %s does not open under the new key alone: %v", id, err)
		}
	}
}

func TestResealStopsAtACredentialItCannotOpen(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	mustCreate(t, newTestCredentials(t, st), ws, "x")

	forgetful, err := crypto.NewKeyring([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := NewCredentials(st, forgetful).Reseal(ctx, 10); err == nil || n != 0 {
		t.Fatalf("Reseal without the old key = %d, %v; want an error", n, err)
	}
}
