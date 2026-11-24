package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

func (e *testEnv) models(key string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestV1ModelsListsOnlyTheKeysWorkspaceAliases(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	aliceKey := env.mockRoute(alice, "alice-chat")
	env.mockRoute(bob, "bobs-route")

	status, body := env.models(aliceKey)
	if status != http.StatusOK {
		t.Fatalf("models: %d %s", status, body)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "alice-chat" {
		t.Fatalf("alice's key lists %s, want alice-chat alone", body)
	}
	if age := time.Since(time.Unix(list.Data[0].Created, 0)); age < 0 || age > time.Minute {
		t.Fatalf("created = %d, want the route's creation time", list.Data[0].Created)
	}
	if strings.Contains(body, "echo") {
		t.Fatalf("the list names the upstream model: %s", body)
	}
}

func TestV1RefusesAKeyWhoseOwnerIsUnverified(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	env.signup(c, "carol@example.com")
	key, _, err := env.deps.GatewayKeys.Mint(t.Context(), env.workspaceOf("carol@example.com"), "app", nil)
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

// cachedChat sends a temperature-0 request and returns its X-Cache header.
func (e *testEnv) cachedChat(key, model string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": "hello through the gateway"}},
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "hello through the gateway") {
		e.t.Fatalf("chat: %d %s", resp.StatusCode, raw)
	}
	return resp.Header.Get("X-Cache")
}

// The exact cache against real Redis and a real route: the stored answer is
// found by the next request, and a PUT on the route, which changes nothing
// but updated_at here, stops it being served.
func TestV1AnswersARepeatFromTheCacheUntilTheRouteIsRewritten(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	key := env.mockRoute(alice, "team-chat")

	if got := env.cachedChat(key, "team-chat"); got != "MISS" {
		t.Fatalf("first request: X-Cache = %q, want MISS", got)
	}
	// The answer is stored after the response, so the hit is waited for.
	hit := func() bool {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if env.cachedChat(key, "team-chat") == "HIT" {
				return true
			}
		}
		return false
	}
	if !hit() {
		t.Fatal("a repeated request was never answered from the cache")
	}

	routes := alice.do(http.MethodGet, "/api/routes", nil)
	route := list(routes.body["data"])[0].(map[string]any)
	id, _ := route["id"].(string)
	rewrite := map[string]any{"alias": "team-chat", "targets": route["targets"]}
	if res := alice.do(http.MethodPut, "/api/routes/"+id, rewrite); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if got := env.cachedChat(key, "team-chat"); got != "MISS" {
		t.Fatalf("after the route was rewritten: X-Cache = %q, want MISS", got)
	}

	off := map[string]any{"alias": "team-chat", "targets": route["targets"], "cache": "off"}
	if res := alice.do(http.MethodPut, "/api/routes/"+id, off); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if got := env.cachedChat(key, "team-chat"); got != "" {
		t.Fatalf("with the route's cache off: X-Cache = %q, want none", got)
	}

	// The hits reach the dashboard: counted in the day's row, flagged on the
	// event, and carrying no tokens.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		usage := alice.do(http.MethodGet, "/api/usage", nil)
		var hits float64
		for _, d := range list(usage.body["days"]) {
			n, _ := d.(map[string]any)["cache_hits"].(float64)
			hits += n
		}
		flagged := false
		for _, e := range list(usage.body["recent"]) {
			event := e.(map[string]any)
			if event["cached"] == true {
				flagged = true
				if event["prompt_tokens"] != float64(0) || event["vendor"] != "mock" || event["attempts"] != float64(0) {
					t.Fatalf("a cache hit was recorded as %v", event)
				}
			}
		}
		if hits >= 1 && flagged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache hits never reached /api/usage: %s", usage.raw)
		}
	}
}
