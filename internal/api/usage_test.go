package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"llm-gateway/internal/store"
	"llm-gateway/internal/usage"
)

func (e *testEnv) writeUsage(workspaceID, route string, at time.Time, n int) {
	e.t.Helper()
	events := make([]store.UsageEvent, n)
	for i := range events {
		events[i] = store.UsageEvent{
			ID: uuid.NewString(), WorkspaceID: workspaceID, KeyID: "00000000-0000-4000-8000-000000000001", RouteAlias: route,
			Vendor: "groq", Model: "llama", Status: 200, Outcome: store.OutcomeOK, Attempts: 1,
			PromptTokens: 10, CompletionTokens: 5, LatencyMs: 100, TTFTMs: 20, CreatedAt: at,
		}
	}
	if err := e.store.WriteBatch(e.t.Context(), events); err != nil {
		e.t.Fatalf("write usage: %v", err)
	}
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func TestUsageShowsTheWorkspacesRollupAndRecentRequests(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	ws := env.workspaceOf("alice@example.com")
	now := time.Now().UTC()
	env.writeUsage(ws, "chat", now, 3)
	env.writeUsage(ws, "other", now.AddDate(0, 0, -2), 1)

	res := alice.do(http.MethodGet, "/api/usage", nil)
	if res.status != http.StatusOK {
		t.Fatalf("usage: %d %s", res.status, res.raw)
	}
	days, recent := list(res.body["days"]), list(res.body["recent"])
	if len(days) != 2 || len(recent) != 4 {
		t.Fatalf("got %d days and %d recent, want 2 and 4: %s", len(days), len(recent), res.raw)
	}
	today := days[1].(map[string]any)
	if today["route"] != "chat" || today["requests"] != float64(3) || today["prompt_tokens"] != float64(30) ||
		today["avg_latency_ms"] != float64(100) || today["day"] != now.Format("2006-01-02") {
		t.Errorf("today's row = %v", today)
	}

	narrow := alice.do(http.MethodGet, "/api/usage?route=other", nil)
	if got := list(narrow.body["days"]); len(got) != 1 || got[0].(map[string]any)["route"] != "other" {
		t.Errorf("route filter: %s", narrow.raw)
	}
	past := alice.do(http.MethodGet, "/api/usage?from="+now.AddDate(0, 0, -400).Format("2006-01-02")+
		"&to="+now.AddDate(0, 0, -300).Format("2006-01-02"), nil)
	if past.status != http.StatusOK || len(list(past.body["days"])) != 0 {
		t.Errorf("an empty range must be an empty list, got %d %s", past.status, past.raw)
	}
}

// The recorder against the real store: an event the database refuses, here
// one for a workspace that no longer exists, is dropped alone.
func TestAnUnstorableEventDoesNotCostOtherWorkspacesTheirs(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	ws := env.workspaceOf("alice@example.com")
	event := func(workspaceID string, promptTokens int) usage.Event {
		return usage.Event{
			WorkspaceID: workspaceID, KeyID: uuid.NewString(), RouteAlias: "chat", Vendor: "groq", Model: "llama",
			Status: 200, Outcome: store.OutcomeOK, Attempts: 1, PromptTokens: promptTokens, CreatedAt: time.Now().UTC(),
		}
	}

	rec := usage.NewRecorder(env.store, 100, nil)
	rec.Record(event(ws, 10))
	rec.Record(event(uuid.NewString(), 10))
	rec.Record(event(ws, 3_000_000_000)) // past int32, as an upstream may report
	go rec.Run(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rec.Close(ctx)

	res := alice.do(http.MethodGet, "/api/usage", nil)
	days := list(res.body["days"])
	if len(days) != 1 || len(list(res.body["recent"])) != 2 {
		t.Fatalf("alice's usage after a batch holding one unstorable event: %s", res.raw)
	}
	if d := days[0].(map[string]any); d["requests"] != float64(2) || d["prompt_tokens"] != float64(3_000_000_010) {
		t.Errorf("rollup = %v, want 2 requests and 3000000010 prompt tokens", d)
	}
}

func TestUsageNeverShowsAnotherWorkspace(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	env.signupAndLogin("bob@example.com")
	env.writeUsage(env.workspaceOf("bob@example.com"), "bobs-route", time.Now().UTC(), 2)

	res := alice.do(http.MethodGet, "/api/usage", nil)
	if res.status != http.StatusOK || len(list(res.body["days"])) != 0 || len(list(res.body["recent"])) != 0 {
		t.Fatalf("alice saw %d %s", res.status, res.raw)
	}
	// The workspace comes from the session, whatever the query says.
	res = alice.do(http.MethodGet, "/api/usage?workspace_id="+url.QueryEscape(env.workspaceOf("bob@example.com")), nil)
	if len(list(res.body["days"])) != 0 {
		t.Fatalf("a workspace_id in the query reached another tenant: %s", res.raw)
	}
}

func TestUsageRefusesABadDateRange(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")

	for _, q := range []string{
		"from=yesterday",
		"to=2026-13-01",
		"from=2026-03-05&to=2026-03-04",
		"from=2025-01-01&to=2026-01-02", // 367 days
	} {
		res := alice.do(http.MethodGet, "/api/usage?"+q, nil)
		if res.status != http.StatusBadRequest || res.errorCode() != "invalid_value" {
			t.Errorf("%s: %d %s, want 400 invalid_value", q, res.status, res.raw)
		}
	}
	ok := alice.do(http.MethodGet, "/api/usage?from=2025-01-01&to=2026-01-01", nil) // 366 days
	if ok.status != http.StatusOK {
		t.Errorf("366 days: %d %s", ok.status, ok.raw)
	}
}

func TestAV1RequestAppearsInUsage(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	key := env.mockRoute(alice, "team-chat")

	if status, body := env.chat(key, "team-chat"); status != http.StatusOK {
		t.Fatalf("chat: %d %s", status, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := alice.do(http.MethodGet, "/api/usage", nil)
		if recent := list(res.body["recent"]); len(recent) == 1 {
			e := recent[0].(map[string]any)
			if e["route"] != "team-chat" || e["vendor"] != "mock" || e["outcome"] != "ok" || e["status"] != float64(200) {
				t.Fatalf("recent = %v", e)
			}
			if len(list(res.body["days"])) != 1 {
				t.Fatalf("rollup = %s", res.raw)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the request never reached /api/usage: %s", res.raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
