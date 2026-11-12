package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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

	got, err := c.Secret(ctx, ws, first.ID)
	if err != nil || got != testSecret {
		t.Fatalf("Secret = %q, %v", got, err)
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
	if _, err := c.Secret(ctx, ws, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Secret after delete: %v", err)
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
	if _, err := c.Secret(ctx, bob, cred.ID); !errors.Is(err, ErrNotFound) {
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

	if _, err := c.Secret(context.Background(), ws, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
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
	if _, err := c.Secret(ctx, ws, dst.ID); !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("ciphertext copied onto another row opened: %v", err)
	}

	copyOnto(foreign.ID, src.ID)
	if _, err := c.Secret(ctx, ws, src.ID); !errors.Is(err, crypto.ErrOpen) {
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
	_, err := c.Secret(ctx, ws, cred.ID)
	if !errors.Is(err, crypto.ErrOpen) {
		t.Fatalf("Secret after provider change: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("error contains the secret")
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
