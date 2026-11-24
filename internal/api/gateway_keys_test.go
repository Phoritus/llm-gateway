package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
)

func createGatewayKey(c *client, name string) response {
	return c.do(http.MethodPost, "/api/gateway-keys", map[string]any{"name": name})
}

func TestGatewayKeyLifecycle(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	ws := env.workspaceOf("alice@example.com")
	ctx := t.Context()

	created := createGatewayKey(alice, " prod app ")
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	token, _ := created.body["key"].(string)
	id, _ := created.body["id"].(string)
	if !strings.HasPrefix(token, auth.GatewayKeyPrefix) || created.body["prefix"] != token[:12] || created.body["name"] != "prod app" {
		t.Fatalf("create body = %s", created.raw)
	}

	got, err := env.deps.GatewayKeys.Authenticate(ctx, token)
	if err != nil || got.WorkspaceID != ws || got.ID != id {
		t.Fatalf("authenticate = %+v, %v; want key %s in workspace %s", got, err, id, ws)
	}

	list := alice.do(http.MethodGet, "/api/gateway-keys", nil)
	data, _ := list.body["data"].([]any)
	if list.status != http.StatusOK || len(data) != 1 || strings.Contains(list.raw, token) {
		t.Fatalf("list must show the key without its secret: %d %s", list.status, list.raw)
	}

	if err := env.deps.GatewayKeys.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	list = alice.do(http.MethodGet, "/api/gateway-keys", nil)
	if row := list.body["data"].([]any)[0].(map[string]any); row["last_used_at"] == nil {
		t.Fatalf("last_used_at not recorded after a use and a flush: %s", list.raw)
	}

	if res := alice.do(http.MethodDelete, "/api/gateway-keys/"+id, nil); res.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", res.status, res.raw)
	}
	if _, err := env.deps.GatewayKeys.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidGatewayKey) {
		t.Fatalf("revoked key on the revoking instance: err = %v, want ErrInvalidGatewayKey", err)
	}
	if res := alice.do(http.MethodDelete, "/api/gateway-keys/"+id, nil); res.status != http.StatusNotFound {
		t.Fatalf("second revoke: %d %s", res.status, res.raw)
	}
	list = alice.do(http.MethodGet, "/api/gateway-keys", nil)
	if row := list.body["data"].([]any)[0].(map[string]any); row["revoked_at"] == nil {
		t.Fatalf("revoked key listed without revoked_at: %s", list.raw)
	}
}

func TestRevocationReachesOtherInstances(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	other := auth.NewGatewayKeys(env.store)
	ctx := t.Context()

	created := createGatewayKey(alice, "")
	token, _ := created.body["key"].(string)
	if _, err := other.Authenticate(ctx, token); err != nil {
		t.Fatalf("other instance, before revoke: %v", err)
	}
	if res := alice.do(http.MethodDelete, "/api/gateway-keys/"+created.body["id"].(string), nil); res.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", res.status, res.raw)
	}

	time.Sleep(1100 * time.Millisecond)
	if _, err := other.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidGatewayKey) {
		t.Fatalf("other instance a second after revoke: err = %v, want ErrInvalidGatewayKey", err)
	}
}

func TestGatewayKeyRejectsUnknownAndMalformed(t *testing.T) {
	env := newTestEnv(t)
	for _, token := range []string{
		"",
		"sk-not-a-gateway-key",
		auth.GatewayKeyPrefix + strings.Repeat("A", 43),
		auth.GatewayKeyPrefix + strings.Repeat("A", 44),
	} {
		if _, err := env.deps.GatewayKeys.Authenticate(t.Context(), token); !errors.Is(err, auth.ErrInvalidGatewayKey) {
			t.Errorf("Authenticate(%q) err = %v, want ErrInvalidGatewayKey", token, err)
		}
	}
}

func TestGatewayKeysAreWorkspaceScoped(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")

	created := createGatewayKey(alice, "alice's")
	id, _ := created.body["id"].(string)

	if res := bob.do(http.MethodGet, "/api/gateway-keys", nil); strings.Contains(res.raw, id) {
		t.Fatalf("bob's list shows alice's key: %s", res.raw)
	}
	if res := bob.do(http.MethodDelete, "/api/gateway-keys/"+id, nil); res.status != http.StatusNotFound {
		t.Fatalf("bob revoking alice's key: %d %s", res.status, res.raw)
	}
	if _, err := env.deps.GatewayKeys.Authenticate(t.Context(), created.body["key"].(string)); err != nil {
		t.Fatalf("alice's key after bob's attempt: %v", err)
	}
}

func TestGatewayKeyLimitCountsOnlyActiveKeys(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	ws := env.workspaceOf("alice@example.com")

	var firstID string
	for i := range 50 {
		_, key, err := env.deps.GatewayKeys.Mint(t.Context(), ws, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = key.ID
		}
	}
	if res := createGatewayKey(alice, ""); res.status != http.StatusConflict || res.errorCode() != "gateway_key_limit_reached" {
		t.Fatalf("51st: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodDelete, "/api/gateway-keys/"+firstID, nil); res.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", res.status, res.raw)
	}
	if res := createGatewayKey(alice, ""); res.status != http.StatusCreated {
		t.Fatalf("after revoking one: %d %s", res.status, res.raw)
	}
}

func TestGatewayKeysRequireVerifiedEmail(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	env.signup(c, "unverified@example.com")
	if res := env.login(c, "unverified@example.com", testPassword); res.status != http.StatusOK {
		t.Fatalf("login: %d %s", res.status, res.raw)
	}
	for _, res := range []response{
		c.do(http.MethodGet, "/api/gateway-keys", nil),
		createGatewayKey(c, ""),
	} {
		if res.status != http.StatusForbidden || res.errorCode() != "email_not_verified" {
			t.Fatalf("unverified: %d %s", res.status, res.raw)
		}
	}
}
