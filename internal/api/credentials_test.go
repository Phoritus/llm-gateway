package api

import (
	"net/http"
	"strings"
	"testing"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

const apiTestSecret = "sk-live-abcdefghijklmnopqrstWXYZ"

func createCredential(c *client, body map[string]any) response {
	return c.do(http.MethodPost, "/api/credentials", body)
}

func TestCredentialsCRUD(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	empty := alice.do(http.MethodGet, "/api/credentials", nil)
	if empty.status != http.StatusOK || !strings.Contains(empty.raw, `"data":[]`) {
		t.Fatalf("empty list: %d %s", empty.status, empty.raw)
	}

	created := createCredential(alice, map[string]any{"provider": "groq", "name": " work ", "secret": apiTestSecret})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	if strings.Contains(created.raw, apiTestSecret) {
		t.Fatalf("create response contains the secret: %s", created.raw)
	}
	if created.body["last4"] != "WXYZ" || created.body["name"] != "work" || created.body["provider"] != "groq" {
		t.Fatalf("create body = %s", created.raw)
	}
	id, _ := created.body["id"].(string)

	list := alice.do(http.MethodGet, "/api/credentials", nil)
	data, _ := list.body["data"].([]any)
	if list.status != http.StatusOK || len(data) != 1 || strings.Contains(list.raw, apiTestSecret) {
		t.Fatalf("list: %d %s", list.status, list.raw)
	}

	if res := alice.do(http.MethodDelete, "/api/credentials/"+id, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodDelete, "/api/credentials/"+id, nil); res.status != http.StatusNotFound {
		t.Fatalf("second delete: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodDelete, "/api/credentials/not-a-uuid", nil); res.status != http.StatusNotFound {
		t.Fatalf("malformed id: %d %s", res.status, res.raw)
	}
}

func TestCredentialValidation(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	for _, tc := range []struct {
		name, param string
		body        map[string]any
	}{
		{"unknown provider", "provider", map[string]any{"provider": "together", "secret": apiTestSecret}},
		{"short secret", "secret", map[string]any{"provider": "groq", "secret": "short-secret-value"}},
		{"secret with a space", "secret", map[string]any{"provider": "groq", "secret": "sk-live-abcdefgh ijklmnopqrstWXYZ"}},
		{"non-ASCII secret", "secret", map[string]any{"provider": "groq", "secret": "sk-live-abcdefghijklmnopqrsté"}},
		{"long name", "name", map[string]any{"provider": "groq", "secret": apiTestSecret, "name": strings.Repeat("n", 101)}},
		{"NUL in name", "name", map[string]any{"provider": "groq", "secret": apiTestSecret, "name": "a\u0000b"}},
		{"newline in name", "name", map[string]any{"provider": "groq", "secret": apiTestSecret, "name": "a\nb"}},
	} {
		res := createCredential(alice, tc.body)
		errObj, _ := res.body["error"].(map[string]any)
		if res.status != http.StatusBadRequest || errObj["param"] != tc.param || errObj["code"] != "invalid_value" {
			t.Errorf("%s: %d %s", tc.name, res.status, res.raw)
		}
		if s, _ := tc.body["secret"].(string); s != "" && strings.Contains(res.raw, s) {
			t.Errorf("%s: response echoes the secret: %s", tc.name, res.raw)
		}
	}
}

func TestCredentialsRequireVerifiedEmail(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	env.signup(c, "unverified@example.com")
	if res := env.login(c, "unverified@example.com", testPassword); res.status != http.StatusOK {
		t.Fatalf("login: %d %s", res.status, res.raw)
	}

	for _, res := range []response{
		c.do(http.MethodGet, "/api/credentials", nil),
		createCredential(c, map[string]any{"provider": "groq", "secret": apiTestSecret}),
	} {
		if res.status != http.StatusForbidden || res.errorCode() != "email_not_verified" {
			t.Fatalf("unverified: %d %s", res.status, res.raw)
		}
	}
}

func TestCredentialLimitOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	ws := env.workspaceOf("alice@example.com")
	for range 50 {
		in := store.NewCredential{Provider: provider.VendorGroq, Secret: apiTestSecret}
		if _, err := env.deps.Credentials.Create(t.Context(), ws, in); err != nil {
			t.Fatal(err)
		}
	}
	res := createCredential(alice, map[string]any{"provider": "groq", "secret": apiTestSecret})
	if res.status != http.StatusConflict || res.errorCode() != "credential_limit_reached" {
		t.Fatalf("51st: %d %s", res.status, res.raw)
	}
}
