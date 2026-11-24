package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func requestLog(workspaceID, route string, at time.Time) RequestLog {
	return RequestLog{
		ID: uuid.NewString(), WorkspaceID: workspaceID, KeyID: uuid.NewString(), RouteAlias: route,
		Vendor: "mock", Model: "echo", Status: 200, Outcome: OutcomeOK, CreatedAt: at,
		Request: `{"model":"` + route + `"}`, Response: `{"choices":[]}`,
	}
}

func writeLogs(t *testing.T, st *Store, logs ...RequestLog) {
	t.Helper()
	if err := st.WriteRequestLogs(context.Background(), logs); err != nil {
		t.Fatalf("write logs: %v", err)
	}
}

func logIDs(t *testing.T, st *Store, workspaceID string, q LogQuery) []string {
	t.Helper()
	logs, err := st.RequestLogs(context.Background(), workspaceID, q)
	if err != nil {
		t.Fatalf("request logs: %v", err)
	}
	ids := make([]string, len(logs))
	for i, l := range logs {
		ids[i] = l.ID
	}
	return ids
}

func TestRequestLogsPageNewestFirstWithoutSkippingTies(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	var logs []RequestLog
	for i := range 5 {
		logs = append(logs, requestLog(ws, "chat", noon.Add(time.Duration(i)*time.Minute)))
	}
	logs = append(logs, requestLog(ws, "chat", noon.Add(4*time.Minute)))
	writeLogs(t, st, logs...)

	var paged []string
	q := LogQuery{Limit: 2}
	for range 10 {
		page, err := st.RequestLogs(context.Background(), ws, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range page {
			paged = append(paged, l.ID)
		}
		if len(page) < q.Limit {
			break
		}
		last := page[len(page)-1]
		q.After = &LogCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	all := logIDs(t, st, ws, LogQuery{Limit: 100})
	if len(all) != 6 || !slices.Equal(paged, all) {
		t.Fatalf("paged %v, want every log once in the order of %v", paged, all)
	}
	first, _ := st.RequestLogs(context.Background(), ws, LogQuery{Limit: 1})
	if !first[0].CreatedAt.Equal(noon.Add(4 * time.Minute)) {
		t.Errorf("first log at %s, want the newest", first[0].CreatedAt)
	}
}

func TestRequestLogsFilterByRouteAndTime(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	early, late, other := requestLog(ws, "chat", noon), requestLog(ws, "chat", noon.Add(time.Hour)), requestLog(ws, "code", noon)
	writeLogs(t, st, early, late, other)

	if got := logIDs(t, st, ws, LogQuery{Route: "code", Limit: 10}); !slices.Equal(got, []string{other.ID}) {
		t.Errorf("route filter = %v", got)
	}
	if got := logIDs(t, st, ws, LogQuery{From: noon.Add(time.Minute), Limit: 10}); !slices.Equal(got, []string{late.ID}) {
		t.Errorf("from filter = %v", got)
	}
	if got := logIDs(t, st, ws, LogQuery{Route: "chat", To: noon.Add(time.Hour), Limit: 10}); !slices.Equal(got, []string{early.ID}) {
		t.Errorf("to filter (exclusive) = %v", got)
	}
}

func TestRequestLogsStayInTheirWorkspace(t *testing.T) {
	st := newTestStore(t)
	alice := signup(t, st, "alice@example.com").WorkspaceID
	bob := signup(t, st, "bob@example.com").WorkspaceID
	writeLogs(t, st, requestLog(alice, "chat", time.Now()))

	if got := logIDs(t, st, bob, LogQuery{Limit: 10}); len(got) != 0 {
		t.Errorf("bob read alice's logs: %v", got)
	}
	if n, err := st.DeleteRequestLogs(context.Background(), bob); err != nil || n != 0 {
		t.Errorf("bob's delete removed %d (%v)", n, err)
	}
	if got := logIDs(t, st, alice, LogQuery{Limit: 10}); len(got) != 1 {
		t.Errorf("alice has %d logs after bob's delete, want 1", len(got))
	}
	if n, err := st.DeleteRequestLogs(context.Background(), alice); err != nil || n != 1 {
		t.Errorf("alice's delete removed %d (%v), want 1", n, err)
	}
	if left := count(t, st, "request_logs"); left != 0 {
		t.Errorf("%d logs left", left)
	}
}

func TestARetriedLogBatchStoresEachLogOnce(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	l := requestLog(ws, "chat", time.Now())
	writeLogs(t, st, l)
	writeLogs(t, st, l)
	if n := count(t, st, "request_logs"); n != 1 {
		t.Errorf("stored %d logs, want 1", n)
	}
}

func TestPruneRequestLogsKeepsEachWorkspacesOwnRetention(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	short := signup(t, st, "short@example.com").WorkspaceID
	unset := signup(t, st, "unset@example.com").WorkspaceID
	if err := st.SetWorkspaceSettings(ctx, short, WorkspaceSettings{PromptLogRetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	shortOld, shortNew := requestLog(short, "chat", now.Add(-2*day)), requestLog(short, "chat", now.Add(-time.Hour))
	unsetOld, unsetKept := requestLog(unset, "chat", now.Add(-8*day)), requestLog(unset, "chat", now.Add(-2*day))
	writeLogs(t, st, shortOld, shortNew, unsetOld, unsetKept)

	n, err := st.PruneRequestLogs(ctx, now, 1)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2", n, err)
	}
	if got := logIDs(t, st, short, LogQuery{Limit: 10}); !slices.Equal(got, []string{shortNew.ID}) {
		t.Errorf("short retention kept %v, want only the hour-old log", got)
	}
	if got := logIDs(t, st, unset, LogQuery{Limit: 10}); !slices.Equal(got, []string{unsetKept.ID}) {
		t.Errorf("default retention kept %v, want the 2-day-old log and not the 8-day-old one", got)
	}
}
