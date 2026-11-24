package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeWriter struct {
	mu       sync.Mutex
	attempts [][]Event
	failing  int
}

func (w *fakeWriter) WriteBatch(_ context.Context, events []Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attempts = append(w.attempts, append([]Event(nil), events...))
	if len(w.attempts) <= w.failing {
		return errors.New("database down")
	}
	return nil
}

func closeWithin(t *testing.T, r *Recorder, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r.Close(ctx)
}

func TestARetryCarriesTheIDsOfTheFirstAttempt(t *testing.T) {
	w := &fakeWriter{failing: 1}
	r := NewRecorder(w, 100, nil)
	r.Record(Event{RouteAlias: "chat"})
	r.Record(Event{RouteAlias: "chat"})
	go r.Run(context.Background())
	closeWithin(t, r, 10*time.Second)

	if len(w.attempts) != 2 || len(w.attempts[0]) != 2 {
		t.Fatalf("attempts = %+v, want one batch of 2 tried twice", w.attempts)
	}
	first, retry := w.attempts[0], w.attempts[1]
	if first[0].ID == "" || first[0].ID == first[1].ID {
		t.Errorf("ids %q and %q, want two distinct ids", first[0].ID, first[1].ID)
	}
	if first[0].ID != retry[0].ID || first[1].ID != retry[1].ID {
		t.Errorf("the retry carried ids %q, %q, not the first attempt's %q, %q: the store cannot tell it is a retry",
			retry[0].ID, retry[1].ID, first[0].ID, first[1].ID)
	}
}

func TestAnEventKeepsTheIDItWasRecordedWith(t *testing.T) {
	w := &fakeWriter{}
	r := NewRecorder(w, 100, nil)
	r.Record(Event{ID: "11111111-1111-1111-1111-111111111111"})
	go r.Run(context.Background())
	closeWithin(t, r, 5*time.Second)

	if len(w.attempts) != 1 || w.attempts[0][0].ID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("attempts = %+v, want the caller's id kept", w.attempts)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
