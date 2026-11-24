package api

import (
	"context"
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
		{"custom without a base URL", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret}},
		{"base URL on another provider", "base_url", map[string]any{"provider": "groq", "secret": apiTestSecret, "base_url": "https://llm.example.com/v1"}},
		{"plain http base URL", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "http://llm.example.com/v1"}},
		{"base URL with a user", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://u:p@llm.example.com/v1"}},
		{"base URL with a query", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://llm.example.com/v1?x=1"}},
		{"base URL on loopback", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://127.0.0.1/v1"}},
		{"base URL at the metadata address", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://169.254.169.254/v1"}},
		{"base URL on a private IPv6 address", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://[fd00::1]/v1"}},
		{"base URL on localhost", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://localhost:8080/v1"}},
		{"base URL on localhost with a trailing dot", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://localhost./v1"}},
		{"base URL with only a port", "base_url", map[string]any{"provider": "custom", "secret": apiTestSecret, "base_url": "https://:443/v1"}},
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

func TestACustomCredentialKeepsItsBaseURL(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	res := createCredential(alice, map[string]any{"provider": "custom", "secret": apiTestSecret,
		"base_url": "https://LLM.example.com/v1/"})
	if res.status != http.StatusCreated || res.body["base_url"] != "https://llm.example.com/v1" {
		t.Fatalf("create: %d %s", res.status, res.raw)
	}
	other := createCredential(alice, map[string]any{"provider": "groq", "secret": apiTestSecret})
	if other.status != http.StatusCreated || other.body["base_url"] != nil {
		t.Fatalf("groq credential: %d %s", other.status, other.raw)
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

type keyChecker struct{ valid string }

func (k keyChecker) CheckKey(_ context.Context, cred provider.Credential) error {
	if cred.APIKey == k.valid {
		return nil
	}
	return &provider.Error{Kind: provider.ErrAuth}
}

func (keyChecker) ChatStream(context.Context, *provider.ChatRequest, provider.Credential) (provider.Stream, error) {
	return nil, &provider.Error{Kind: provider.ErrUpstream}
}

func TestCredentialKeyTest(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")

	idOf := func(provider, secret string) string {
		res := createCredential(alice, map[string]any{"provider": provider, "secret": secret})
		if res.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", provider, res.status, res.raw)
		}
		return res.body["id"].(string)
	}
	good := idOf("groq", apiTestSecret)
	bad := idOf("groq", "sk-live-not-the-right-key-0000")
	noAdapter := idOf("gemini", apiTestSecret)

	for _, tc := range []struct {
		id, code string
		ok       bool
	}{
		{good, "", true},
		{bad, "key_rejected", false},
		{noAdapter, "provider_unavailable", false},
	} {
		res := alice.do(http.MethodPost, "/api/credentials/"+tc.id+"/test", nil)
		if res.status != http.StatusOK || res.body["ok"] != tc.ok || (tc.code != "" && res.body["code"] != tc.code) {
			t.Errorf("test %s: %d %s, want ok=%v code=%q", tc.id, res.status, res.raw, tc.ok, tc.code)
		}
		if strings.Contains(res.raw, apiTestSecret) {
			t.Errorf("test response contains the secret: %s", res.raw)
		}
	}

	if res := bob.do(http.MethodPost, "/api/credentials/"+good+"/test", nil); res.status != http.StatusNotFound {
		t.Errorf("Bob testing Alice's credential: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodPost, "/api/credentials/not-a-uuid/test", nil); res.status != http.StatusNotFound {
		t.Errorf("malformed id: %d %s", res.status, res.raw)
	}
}
