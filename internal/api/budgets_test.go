package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func keyID(t *testing.T, c *client) string {
	t.Helper()
	list := c.do(http.MethodGet, "/api/gateway-keys", nil)
	data, _ := list.body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("no gateway key: %s", list.raw)
	}
	id, _ := data[0].(map[string]any)["id"].(string)
	return id
}

func TestABlockingKeyBudgetRefusesOnceSpentAndShowsTheSpend(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	token := env.mockRoute(alice, "chat")
	id := keyID(t, alice)

	set := alice.do(http.MethodPut, "/api/gateway-keys/"+id+"/budget", map[string]any{"monthly_budget_usd": 1})
	if set.status != http.StatusOK || set.body["monthly_budget_usd"] != 1.0 || set.body["budget_action"] != "block" {
		t.Fatalf("set budget: %d %s", set.status, set.raw)
	}
	// The key cache holds the key without its budget for up to a second.
	time.Sleep(1100 * time.Millisecond)

	if status, body := env.chat(token, "chat"); status != http.StatusOK {
		t.Fatalf("first request under budget: %d %s", status, body)
	}
	var (
		status int
		body   string
	)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if status, body = env.chat(token, "chat"); status == http.StatusTooManyRequests {
			break
		}
	}
	if status != http.StatusTooManyRequests || !strings.Contains(body, "budget_exceeded") || !strings.Contains(body, "gateway key") {
		t.Fatalf("a spent key budget must refuse: %d %s", status, body)
	}

	keys := alice.do(http.MethodGet, "/api/gateway-keys", nil)
	row := keys.body["data"].([]any)[0].(map[string]any)
	if spent, _ := row["spent_this_month_usd"].(float64); spent <= 1 {
		t.Errorf("spent_this_month_usd = %v, want past the 1 USD budget: %s", row["spent_this_month_usd"], keys.raw)
	}

	for deadline := time.Now().Add(3 * time.Second); env.mailer.count() < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if last := env.mailer.last(t); !strings.Contains(last.Subject, "budget") || last.To != "alice@example.com" {
		t.Errorf("the owner was not mailed about the budget: %+v", last)
	}

	var usage response
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if usage = alice.do(http.MethodGet, "/api/usage", nil); len(list(usage.body["recent"])) >= 2 {
			break
		}
	}
	recent := list(usage.body["recent"])
	if len(recent) < 2 {
		t.Fatalf("usage: %s", usage.raw)
	}
	served := recent[len(recent)-1].(map[string]any)
	if cost, _ := served["cost_usd"].(float64); cost <= 1 {
		t.Errorf("the served request's cost_usd = %v: %s", served["cost_usd"], usage.raw)
	}
	b, _ := usage.body["budget"].(map[string]any)
	if b == nil || b["monthly_budget_usd"] != nil || b["period"] == "" || b["resets_at"] == nil {
		t.Errorf("usage budget = %v, want the workspace's (none) with its period", b)
	}
	if spent, _ := b["spent_this_month_usd"].(float64); spent <= 1 {
		t.Errorf("the workspace spend = %v, want the key's spend counted too", b["spent_this_month_usd"])
	}
}

func TestAWarningWorkspaceBudgetKeepsServing(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	token := env.mockRoute(alice, "chat")
	if res := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"monthly_budget_usd": 0, "budget_action": "warn"}); res.status != http.StatusOK {
		t.Fatalf("set budget: %d %s", res.status, res.raw)
	}
	for range 3 {
		if status, body := env.chat(token, "chat"); status != http.StatusOK {
			t.Fatalf("a warning budget refused: %d %s", status, body)
		}
	}
}

func TestBudgetFieldsAreValidated(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	for _, c := range []struct {
		body  map[string]any
		param string
	}{
		{map[string]any{"monthly_budget_usd": -1}, "monthly_budget_usd"},
		{map[string]any{"monthly_budget_usd": 2e9}, "monthly_budget_usd"},
		{map[string]any{"monthly_budget_usd": 5, "budget_action": "sometimes"}, "budget_action"},
	} {
		if res := alice.do(http.MethodPut, "/api/workspace/settings", c.body); res.status != http.StatusBadRequest || !strings.Contains(res.raw, c.param) {
			t.Errorf("settings %v: %d %s", c.body, res.status, res.raw)
		}
		c.body["name"] = "k"
		if res := alice.do(http.MethodPost, "/api/gateway-keys", c.body); res.status != http.StatusBadRequest || !strings.Contains(res.raw, c.param) {
			t.Errorf("new key %v: %d %s", c.body, res.status, res.raw)
		}
	}

	put := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{"monthly_budget_usd": 12.5, "budget_action": "warn"})
	if put.status != http.StatusOK || put.body["monthly_budget_usd"] != 12.5 || put.body["budget_action"] != "warn" {
		t.Fatalf("put: %d %s", put.status, put.raw)
	}
	if got := alice.do(http.MethodGet, "/api/workspace/settings", nil); got.body["monthly_budget_usd"] != 12.5 {
		t.Fatalf("the budget was not kept: %s", got.raw)
	}
	if got := alice.do(http.MethodPut, "/api/workspace/settings", map[string]any{}); got.body["monthly_budget_usd"] != nil || got.body["budget_action"] != "block" {
		t.Fatalf("a put without a budget must remove it: %s", got.raw)
	}

	created := alice.do(http.MethodPost, "/api/gateway-keys", map[string]any{"name": "ci", "monthly_budget_usd": 3, "budget_action": "warn"})
	if created.status != http.StatusCreated || created.body["monthly_budget_usd"] != 3.0 || created.body["spent_this_month_usd"] != 0.0 {
		t.Fatalf("create with a budget: %d %s", created.status, created.raw)
	}
}

func TestABudgetCannotBeSetOnAnotherWorkspacesKey(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	if res := createGatewayKey(alice, "app"); res.status != http.StatusCreated {
		t.Fatalf("mint: %d %s", res.status, res.raw)
	}
	id := keyID(t, alice)

	if res := bob.do(http.MethodPut, "/api/gateway-keys/"+id+"/budget", map[string]any{"monthly_budget_usd": 0}); res.status != http.StatusNotFound {
		t.Fatalf("bob set alice's key budget: %d %s", res.status, res.raw)
	}
	row := alice.do(http.MethodGet, "/api/gateway-keys", nil).body["data"].([]any)[0].(map[string]any)
	if row["monthly_budget_usd"] != nil {
		t.Errorf("alice's key has a budget she never set: %v", row)
	}
	if res := alice.do(http.MethodPut, "/api/gateway-keys/not-a-uuid/budget", map[string]any{}); res.status != http.StatusNotFound {
		t.Errorf("a malformed id: %d %s", res.status, res.raw)
	}
}
