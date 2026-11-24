package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func logsWhen(t *testing.T, c *client, path string, n int) response {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := c.do(http.MethodGet, path, nil)
		if res.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, res.status, res.raw)
		}
		if len(list(res.body["logs"])) == n {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never held %d logs: %s", path, n, res.raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// usageWhen waits until n requests have finished, by their usage events.
func usageWhen(t *testing.T, c *client, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := c.do(http.MethodGet, "/api/usage", nil)
		if len(list(res.body["recent"])) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage never held %d requests: %s", n, res.raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPromptLoggingSettingsDefaultOffAndValidateRetention(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	res := alice.do(http.MethodGet, "/api/workspace/settings", nil)
	if res.body["prompt_logging_enabled"] != false || res.body["prompt_log_retention_days"] != float64(7) {
		t.Fatalf("defaults: %s", res.raw)
	}
	for _, days := range []int{-1, 31} {
		res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"prompt_log_retention_days": days})
		if res.status != http.StatusBadRequest || res.errorCode() != "invalid_value" {
			t.Errorf("retention %d: %d %s, want 400", days, res.status, res.raw)
		}
	}
	res = alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"prompt_logging_enabled": true, "prompt_log_retention_days": 30})
	if res.status != http.StatusOK || res.body["prompt_log_retention_days"] != float64(30) {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	res = alice.do(http.MethodGet, "/api/workspace/settings", nil)
	if res.body["prompt_logging_enabled"] != true || res.body["prompt_log_retention_days"] != float64(30) || res.body["cache_enabled"] != true {
		t.Fatalf("the put was not kept: %s", res.raw)
	}
}

func TestPromptLogsRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	key := env.mockRoute(alice, "team-chat")

	if status, _ := env.chat(key, "team-chat"); status != http.StatusOK {
		t.Fatal("chat failed")
	}
	off := alice.do(http.MethodGet, "/api/logs", nil)
	logging, _ := off.body["logging"].(map[string]any)
	if off.status != http.StatusOK || logging["enabled"] != false || !strings.Contains(off.raw, "DELETE /api/logs") {
		t.Fatalf("logs with logging off: %d %s", off.status, off.raw)
	}

	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"prompt_logging_enabled": true}); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	for range 3 {
		if status, body := env.chat(key, "team-chat"); status != http.StatusOK {
			t.Fatalf("chat: %d %s", status, body)
		}
	}
	res := logsWhen(t, alice, "/api/logs", 3)
	l := list(res.body["logs"])[0].(map[string]any)
	if l["route"] != "team-chat" || l["status"] != float64(200) || l["outcome"] != "ok" ||
		!strings.Contains(l["request"].(string), "hello through the gateway") ||
		!strings.Contains(l["response"].(string), "hello through the gateway") {
		t.Fatalf("log = %v", l)
	}

	page := logsWhen(t, alice, "/api/logs?limit=2", 2)
	next, _ := page.body["next_cursor"].(string)
	if next == "" {
		t.Fatalf("a full page has no next_cursor: %s", page.raw)
	}
	rest := logsWhen(t, alice, "/api/logs?limit=2&cursor="+next, 1)
	if rest.body["next_cursor"] != nil {
		t.Errorf("the last page has a next_cursor: %s", rest.raw)
	}
	if got := logsWhen(t, alice, "/api/logs?route=other", 0); len(list(got.body["logs"])) != 0 {
		t.Error("route filter")
	}
	if got := bob.do(http.MethodGet, "/api/logs", nil); len(list(got.body["logs"])) != 0 {
		t.Fatalf("bob read alice's logs: %s", got.raw)
	}

	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"prompt_logging_enabled": false}); res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	env.chat(key, "team-chat")
	usageWhen(t, alice, 5)
	logsWhen(t, alice, "/api/logs", 3)

	if res := alice.do(http.MethodDelete, "/api/logs", nil); res.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.status, res.raw)
	}
	logsWhen(t, alice, "/api/logs", 0)
}

func TestPromptLogsRefuseBadParameters(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	for _, q := range []string{
		"limit=0", "limit=51", "limit=x", "from=yesterday", "to=2026-01-01",
		"from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z", "cursor=nope", "route=not%20an%20alias",
	} {
		res := alice.do(http.MethodGet, "/api/logs?"+q, nil)
		if res.status != http.StatusBadRequest || res.errorCode() != "invalid_value" {
			t.Errorf("%s: %d %s, want 400 invalid_value", q, res.status, res.raw)
		}
	}
}
