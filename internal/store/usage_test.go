package store

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func usageEvent(workspaceID, route string, at time.Time, mutate ...func(*UsageEvent)) UsageEvent {
	e := UsageEvent{
		ID: uuid.NewString(), WorkspaceID: workspaceID, KeyID: uuid.NewString(), RouteAlias: route,
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

func TestTheRollupCountsCacheHits(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	hit := func(e *UsageEvent) { e.Cached, e.PromptTokens, e.CompletionTokens = true, 0, 0 }

	writeUsage(t, st, usageEvent(ws, "chat", noon), usageEvent(ws, "chat", noon.Add(time.Minute), hit))
	writeUsage(t, st, usageEvent(ws, "chat", noon.Add(2*time.Minute), hit))

	days := dailyOf(t, st, ws, "chat")
	if len(days) != 1 || days[0].Requests != 3 || days[0].CacheHits != 2 || days[0].PromptTokens != 10 {
		t.Fatalf("rollup = %+v, want 3 requests of which 2 cache hits, and one request's tokens", days)
	}
}

func TestTheFreeQuotaCatalogIsSeeded(t *testing.T) {
	st := newTestStore(t)
	quotas, err := st.FreeQuotas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var groq, openrouter bool
	for _, q := range quotas {
		switch {
		case q.Vendor == "groq" && q.Model == "openai/gpt-oss-120b":
			groq = true
			if q.RPD == nil || *q.RPD <= 0 || q.TPM == nil || q.RPM != nil || q.PerAccount {
				t.Errorf("groq row = %+v, want its per-model limits read", q)
			}
		case q.Vendor == "openrouter":
			openrouter = true
			if !q.PerAccount {
				t.Errorf("openrouter row = %+v, want its limit counted per account", q)
			}
		}
	}
	if !groq || !openrouter {
		t.Fatalf("catalog = %+v, want the seeded groq and openrouter rows", quotas)
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

func TestARetriedBatchCountsOnce(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	batch := []UsageEvent{usageEvent(ws, "chat", at), usageEvent(ws, "chat", at)}

	writeUsage(t, st, batch...)
	writeUsage(t, st, batch...)
	if days := dailyOf(t, st, ws, "chat"); len(days) != 1 || days[0].Requests != 2 || days[0].PromptTokens != 20 {
		t.Fatalf("rollup after the same batch twice = %+v, want one row of 2 requests", days)
	}
	if n := count(t, st, "usage_events"); n != 2 {
		t.Errorf("stored %d raw events, want 2", n)
	}

	// A batch that mixes a stored event with a new one adds only the new one.
	writeUsage(t, st, batch[0], usageEvent(ws, "chat", at))
	if days := dailyOf(t, st, ws, "chat"); days[0].Requests != 3 {
		t.Errorf("rollup = %+v, want 3 requests", days)
	}
}

// replyDroppingProxy forwards TCP to upstream. Once armed, it swallows every
// reply on the first connection whose client sends marker, so that client's
// statement runs on the server and the client never hears that it did.
func replyDroppingProxy(t *testing.T, upstream string, marker []byte) (addr string, arm func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var armed atomic.Bool

	serve := func(client net.Conn) {
		server, err := net.Dial("tcp", upstream)
		if err != nil {
			_ = client.Close()
			return
		}
		var mute atomic.Bool
		go func() {
			defer server.Close()
			buf := make([]byte, 64<<10)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					if bytes.Contains(buf[:n], marker) && armed.CompareAndSwap(true, false) {
						mute.Store(true)
					}
					if _, werr := server.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		defer client.Close()
		buf := make([]byte, 64<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 && !mute.Load() {
				if _, werr := client.Write(buf[:n]); werr != nil {
					_, _ = io.Copy(io.Discard, server)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(client)
		}
	}()
	return ln.Addr().String(), func() { armed.Store(true) }
}

func TestABatchRetriedAfterALostAcknowledgementCountsOnce(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	const route = "route-whose-reply-is-lost"

	u, err := url.Parse(st.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the store's url: %v", err)
	}
	var arm func()
	u.Host, arm = replyDroppingProxy(t, u.Host, []byte(route))
	proxied, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatalf("open through the proxy: %v", err)
	}
	defer proxied.Close()

	batch := []UsageEvent{usageEvent(ws, route, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))}
	arm()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := proxied.WriteBatch(ctx, batch); err == nil {
		t.Fatal("the first write heard a reply the proxy should have dropped")
	}
	if n := count(t, st, "usage_events"); n != 1 {
		t.Fatalf("the unacknowledged write stored %d events, want 1: the test did not lose an acknowledgement", n)
	}

	if err := proxied.WriteBatch(context.Background(), batch); err != nil {
		t.Fatalf("retry: %v", err)
	}
	days := dailyOf(t, st, ws, route)
	if n := count(t, st, "usage_events"); n != 1 || len(days) != 1 || days[0].Requests != 1 {
		t.Errorf("one request retried after a lost acknowledgement: %d events, rollup %+v; want 1 and 1", n, days)
	}
}

func TestTokenCountsPastInt32AreStored(t *testing.T) {
	st := newTestStore(t)
	ws := signup(t, st, "alice@example.com").WorkspaceID
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	huge := func(e *UsageEvent) { e.PromptTokens, e.CompletionTokens, e.LatencyMs, e.TTFTMs = 3e9, 3e9, 3e9, 3e9 }

	writeUsage(t, st, usageEvent(ws, "chat", at, huge))
	recent, err := st.Recent(context.Background(), ws, 1)
	if err != nil || len(recent) != 1 || recent[0].PromptTokens != 3e9 || recent[0].TTFTMs != 3e9 {
		t.Fatalf("recent = %+v (%v), want the event as it was written", recent, err)
	}
	if days := dailyOf(t, st, ws, "chat"); len(days) != 1 || days[0].PromptTokens != 3e9 || days[0].LatencyMsSum != 3e9 {
		t.Errorf("rollup = %+v", days)
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
