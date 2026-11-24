package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestANewCredentialIsDueAtOnceAndClaimingPushesItOut(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	cred := mustCreate(t, c, ws, "one")

	got, err := c.ClaimDueChecks(ctx, time.Hour, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 1 || got[0].ID != cred.ID || got[0].WorkspaceID != ws {
		t.Fatalf("first claim = %+v, want the new credential", got)
	}
	got, err = c.ClaimDueChecks(ctx, time.Hour, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("second claim = %+v, %v, want nothing", got, err)
	}
}

func TestClaimDueChecksHonoursItsLimit(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	for _, name := range []string{"a", "b", "c"} {
		mustCreate(t, c, ws, name)
	}

	first, err := c.ClaimDueChecks(ctx, time.Hour, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("claim limit 2 = %+v, %v", first, err)
	}
	rest, err := c.ClaimDueChecks(ctx, time.Hour, 2)
	if err != nil || len(rest) != 1 {
		t.Fatalf("second claim = %+v, %v, want the one left", rest, err)
	}
	for _, d := range first {
		if d.ID == rest[0].ID {
			t.Fatalf("credential %s claimed twice", d.ID)
		}
	}
}

func TestConcurrentClaimsNeverShareACredential(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	const total = 20
	for range total {
		mustCreate(t, c, ws, "k")
	}

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.ClaimDueChecks(ctx, time.Hour, 5)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range got {
				seen[d.ID]++
			}
		}()
	}
	wg.Wait()

	for id, n := range seen {
		if n != 1 {
			t.Errorf("credential %s claimed %d times", id, n)
		}
	}
	if len(seen) != total {
		t.Errorf("claimed %d distinct credentials, want %d", len(seen), total)
	}
}

func TestRecordCheckShowsInListAndUnseenCredentialHasNone(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	ws := signup(t, st, "a@example.com").WorkspaceID
	checked := mustCreate(t, c, ws, "checked")
	fresh := mustCreate(t, c, ws, "fresh")

	at := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if err := c.RecordCheck(ctx, ws, checked.ID, KeyCheck{Result: KeyCheckRejected, At: at}); err != nil {
		t.Fatalf("record: %v", err)
	}

	list, err := c.List(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Credential{}
	for _, cr := range list {
		byID[cr.ID] = cr
	}
	if lc := byID[fresh.ID].LastCheck; lc != nil {
		t.Errorf("never checked credential lists LastCheck = %+v, want nil", lc)
	}
	lc := byID[checked.ID].LastCheck
	if lc == nil || lc.Result != KeyCheckRejected || !lc.At.Equal(at) {
		t.Errorf("LastCheck = %+v, want key_rejected at %v", lc, at)
	}
}

func TestRecordCheckRefusesAnotherWorkspaceAndADeletedCredential(t *testing.T) {
	st := newTestStore(t)
	c := newTestCredentials(t, st)
	ctx := context.Background()
	alice := signup(t, st, "a@example.com").WorkspaceID
	bob := signup(t, st, "b@example.com").WorkspaceID
	cred := mustCreate(t, c, alice, "alice")
	check := KeyCheck{Result: KeyCheckOK, At: time.Now()}

	if err := c.RecordCheck(ctx, bob, cred.ID, check); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bob recording on Alice's credential: %v", err)
	}
	list, err := c.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].LastCheck != nil {
		t.Fatalf("Alice's credential after Bob's attempt = %+v, %v", list, err)
	}

	if err := c.Delete(ctx, alice, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordCheck(ctx, alice, cred.ID, check); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recording on a deleted credential: %v", err)
	}
	if err := c.RecordCheck(ctx, alice, uuid.NewString(), check); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recording on an unknown id: %v", err)
	}
}
