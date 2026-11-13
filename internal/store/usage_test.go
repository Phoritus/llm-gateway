package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func usageEvent(workspaceID, route string, at time.Time, mutate ...func(*UsageEvent)) UsageEvent {
	e := UsageEvent{
		WorkspaceID: workspaceID, KeyID: uuid.NewString(), RouteAlias: route,
		Vendor: "groq", Model: "llama", Status: 200, Outcome: OutcomeOK, Attempts: 1,
		PromptTokens: 10, CompletionTokens: 5, LatencyMs: 100, TTFTMs: 20, CreatedAt: at,
	}
	for _, m := range mutate {
		m(&e)
	}
	return e
}

func writeUsage(t *testing.T, st *Store, events ...UsageEvent) {
	t.Helper()
	if err := st.WriteBatch(context.Background(), events); err != nil {
		t.Fatalf("write batch: %v", err)
	}
}

func dailyOf(t *testing.T, st *Store, workspaceID, route string) []UsageDay {
	t.Helper()
	day := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	days, err := st.Daily(context.Background(), workspaceID, day, day.AddDate(100, 0, 0), route)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	return days
}

func TestWriteBatchRollsUpWhatItStored(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	failed := func(e *UsageEvent) { e.Status, e.Outcome = 502, "upstream_error" }
	hungUp := func(e *UsageEvent) { e.Status, e.Outcome = 499, OutcomeClientGone }

	writeUsage(t, st,
		usageEvent(ws, "chat", noon),
		usageEvent(ws, "chat", noon.Add(time.Minute), failed),
		usageEvent(ws, "chat", noon.Add(2*time.Minute), hungUp),
		usageEvent(ws, "chat", noon.Add(12*time.Hour)), // the next UTC day
		usageEvent(ws, "other", noon),
	)

	days := dailyOf(t, st, ws, "chat")
	if len(days) != 2 {
		t.Fatalf("got %d rollup rows, want 2: %+v", len(days), days)
	}
	d := days[0]
	if !d.Day.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day = %s", d.Day)
	}
	if d.Requests != 3 || d.Errors != 1 || d.PromptTokens != 30 || d.CompletionTokens != 15 || d.LatencyMsSum != 300 {
		t.Errorf("rollup = %+v, want 3 requests, 1 error (a client hanging up is not one), 30/15 tokens, 300ms", d)
	}
	if days[1].Requests != 1 {
		t.Errorf("next day = %+v", days[1])
	}
	if all := dailyOf(t, st, ws, ""); len(all) != 3 {
		t.Errorf("no route filter returned %d rows, want 3", len(all))
	}
	if n := count(t, st, "usage_events"); n != 5 {
		t.Errorf("stored %d raw events, want 5", n)
	}
}

func TestBatchesAddUp(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	writeUsage(t, st, usageEvent(ws, "chat", at), usageEvent(ws, "chat", at))
	writeUsage(t, st, usageEvent(ws, "chat", at))

	days := dailyOf(t, st, ws, "chat")
	if len(days) != 1 || days[0].Requests != 3 || days[0].PromptTokens != 30 {
		t.Fatalf("rollup = %+v, want one row of 3 requests", days)
	}
}

func TestAFailedBatchCountsNothing(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	// The second event names a workspace that does not exist.
	err := st.WriteBatch(context.Background(), []UsageEvent{
		usageEvent(ws, "chat", at),
		usageEvent(uuid.NewString(), "chat", at),
	})
	if err == nil {
		t.Fatal("a batch naming an unknown workspace was written")
	}
	if n := count(t, st, "usage_events") + count(t, st, "usage_daily"); n != 0 {
		t.Errorf("%d rows survived a failed batch", n)
	}
}

func TestUsageNeverCrossesWorkspaces(t *testing.T) {
	st := newTestStore(t)
	alice := signup(t, st, "alice@example.com").WorkspaceID
	bob := signup(t, st, "bob@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	writeUsage(t, st, usageEvent(alice, "chat", at), usageEvent(bob, "chat", at), usageEvent(bob, "chat", at))

	if days := dailyOf(t, st, alice, ""); len(days) != 1 || days[0].Requests != 1 {
		t.Errorf("alice's rollup = %+v", days)
	}
	recent, err := st.Recent(context.Background(), bob, 50)
	if err != nil || len(recent) != 2 {
		t.Fatalf("bob's recent: %v %+v", err, recent)
	}
	for _, e := range recent {
		if e.WorkspaceID != bob {
			t.Errorf("bob was shown a row of workspace %s", e.WorkspaceID)
		}
	}
	if days := dailyOf(t, st, "not-a-uuid", ""); len(days) != 0 {
		t.Errorf("a malformed id returned %+v", days)
	}
}

func TestRecentIsNewestFirstAndCapped(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	writeUsage(t, st,
		usageEvent(ws, "a", at),
		usageEvent(ws, "c", at.Add(2*time.Minute)),
		usageEvent(ws, "b", at.Add(time.Minute)),
	)
	recent, err := st.Recent(context.Background(), ws, 2)
	if err != nil || len(recent) != 2 || recent[0].RouteAlias != "c" || recent[1].RouteAlias != "b" {
		t.Fatalf("recent = %+v (%v)", recent, err)
	}
}

func TestPruneRemovesOldRawEventsAndKeepsTheRollup(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	now := time.Now().UTC()

	var events []UsageEvent
	for i := 0; i < 5; i++ {
		events = append(events, usageEvent(ws, "chat", now.AddDate(0, 0, -40)))
	}
	events = append(events, usageEvent(ws, "chat", now.Add(-time.Hour)))
	writeUsage(t, st, events...)

	// A batch size below the row count makes the loop run more than once.
	n, err := st.PruneEvents(context.Background(), now.AddDate(0, 0, -30), 2)
	if err != nil || n != 5 {
		t.Fatalf("pruned %d rows (%v), want 5", n, err)
	}
	if left := count(t, st, "usage_events"); left != 1 {
		t.Errorf("%d raw events left, want only the recent one", left)
	}
	var requests int64
	for _, d := range dailyOf(t, st, ws, "") {
		requests += d.Requests
	}
	if requests != 6 {
		t.Errorf("rollup counts %d requests after the prune, want all 6", requests)
	}
}
