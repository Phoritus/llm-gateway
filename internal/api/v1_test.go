package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

func (e *testEnv) chat(key, model string) (int, string) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hello through the gateway"}},
	})
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (e *testEnv) mockRoute(c *client, alias string) string {
	e.t.Helper()
	cred := createCredential(c, map[string]any{"provider": "mock", "secret": apiTestSecret})
	if cred.status != http.StatusCreated {
		e.t.Fatalf("create mock credential: %d %s", cred.status, cred.raw)
	}
	credID, _ := cred.body["id"].(string)
	if res := c.do(http.MethodPost, "/api/routes", routeBody(alias, target(credID, "echo"))); res.status != http.StatusCreated {
		e.t.Fatalf("create route: %d %s", res.status, res.raw)
	}
	key := createGatewayKey(c, "app")
	if key.status != http.StatusCreated {
		e.t.Fatalf("mint key: %d %s", key.status, key.raw)
	}
	token, _ := key.body["key"].(string)
	return token
}

func TestV1ServesAWorkspaceRouteWithItsGatewayKey(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	key := env.mockRoute(alice, "team-chat")

	status, body := env.chat(key, "team-chat")
	if status != http.StatusOK || !strings.Contains(body, "hello through the gateway") {
		t.Fatalf("chat: %d %s", status, body)
	}

	if status, body := env.chat(key, "not-a-route"); status != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("unknown alias: %d %s, want 404 model_not_found", status, body)
	}
	if status, _ := env.chat("gwk_"+strings.Repeat("A", 43), "team-chat"); status != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d, want 401", status)
	}
}

func TestV1KeysSeeOnlyTheirOwnWorkspacesRoutes(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	env.mockRoute(alice, "alice-only")
	bobKey := env.mockRoute(bob, "bobs-route")

	if status, body := env.chat(bobKey, "alice-only"); status != http.StatusNotFound {
		t.Fatalf("bob's key reached alice's route: %d %s", status, body)
	}
}

func TestV1RefusesAKeyWhoseOwnerIsUnverified(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	env.signup(c, "carol@example.com")
	key, _, err := env.deps.GatewayKeys.Mint(t.Context(), env.workspaceOf("carol@example.com"), "app")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := env.chat(key, "anything"); status != http.StatusForbidden || !strings.Contains(body, "email_not_verified") {
		t.Fatalf("unverified owner: %d %s, want 403 email_not_verified", status, body)
	}
}

func TestProductionRefusesMockCredentials(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	env.deps.Config.Env = config.EnvProduction

	res := createCredential(alice, map[string]any{"provider": "mock", "secret": apiTestSecret})
	if res.status != http.StatusBadRequest || res.errorCode() != "invalid_value" {
		t.Fatalf("mock credential in production: %d %s, want 400 invalid_value", res.status, res.raw)
	}
	if strings.Contains(res.raw, "mock") {
		t.Fatalf("the refusal offers mock as a choice: %s", res.raw)
	}
}
