package api

import (
	"net/http"
	"testing"
	"time"
)

func TestWorkspaceSettingsReplaceAndDefault(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")

	if res := alice.do(http.MethodGet, "/api/workspace/settings", nil); res.status != http.StatusOK || res.body["cache_enabled"] != true {
		t.Fatalf("a new workspace caches by default: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"cache_enabled": false}); res.status != http.StatusOK || res.body["cache_enabled"] != false {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodGet, "/api/workspace/settings", nil); res.body["cache_enabled"] != false {
		t.Fatalf("the put was not kept: %s", res.raw)
	}
	if res := bob.do(http.MethodGet, "/api/workspace/settings", nil); res.body["cache_enabled"] != true {
		t.Fatalf("one workspace's settings reached another: %s", res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"cache_enabeld": false}); res.status != http.StatusBadRequest || res.errorCode() != "unknown_field" {
		t.Fatalf("a misspelt field must be refused: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{}); res.status != http.StatusOK || res.body["cache_enabled"] != true {
		t.Fatalf("a put without cache_enabled must reset it: %d %s", res.status, res.raw)
	}
}

func TestV1HonoursTheWorkspaceCacheSwitch(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	key := env.mockRoute(alice, "team-chat")

	env.cachedChat(key, "team-chat")
	hit := false
	for deadline := time.Now().Add(5 * time.Second); !hit && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		hit = env.cachedChat(key, "team-chat") == "HIT"
	}
	if !hit {
		t.Fatal("a repeated request was never answered from the cache")
	}

	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"cache_enabled": false}); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if got := env.cachedChat(key, "team-chat"); got != "" {
		t.Fatalf("with the workspace's cache off: X-Cache = %q, want none", got)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"cache_enabled": true}); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if got := env.cachedChat(key, "team-chat"); got != "HIT" {
		t.Fatalf("with the workspace's cache back on: X-Cache = %q, want HIT", got)
	}
}

func TestWorkspaceSettingsSemanticCacheIsOffUntilSet(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	if res := alice.do(http.MethodGet, "/api/workspace/settings", nil); res.body["semantic_cache_enabled"] != false {
		t.Fatalf("the semantic cache must default off: %s", res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"semantic_cache_enabled": true}); res.status != http.StatusOK || res.body["semantic_cache_enabled"] != true {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodGet, "/api/workspace/settings", nil); res.body["semantic_cache_enabled"] != true || res.body["cache_enabled"] != true {
		t.Fatalf("the put was not kept, or changed the exact cache switch: %s", res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"cache_enabled": false}); res.body["semantic_cache_enabled"] != false {
		t.Fatalf("a put replaces the whole document: %s", res.raw)
	}
}
