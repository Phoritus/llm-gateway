package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricReader installs a meter provider once. The package's counter binds to
// the first global provider only, so every test that reads a metric shares it.
var metricReader = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

// droppedFor reads the running total of events dropped for a reason. The
// counter is cumulative across tests, so tests compare it before and after.
func droppedFor(t *testing.T, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "usage.events.dropped" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value("reason"); v.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return 0
}

type fakeWriter struct {
	mu      sync.Mutex
	batches [][]Event
	calls   int
	failing int
	block   chan struct{}
}

func (w *fakeWriter) WriteBatch(ctx context.Context, events []Event) error {
	if w.block != nil {
		select {
		case <-w.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.failing {
		return errors.New("database down")
	}
	w.batches = append(w.batches, append([]Event(nil), events...))
	return nil
}

func (w *fakeWriter) written() (n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.batches {
		n += len(b)
	}
	return n
}

func (w *fakeWriter) batchSizes() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var sizes []int
	for _, b := range w.batches {
		sizes = append(sizes, len(b))
	}
	return sizes
}

func ev(route string) Event { return Event{WorkspaceID: "ws", RouteAlias: route} }

func started(t *testing.T, w Writer, capacity int, tune ...func(*Recorder)) *Recorder {
	t.Helper()
	metricReader()
	r := NewRecorder(w, capacity, nil)
	r.flushEvery = 10 * time.Millisecond
	r.retryWait = time.Millisecond
	for _, f := range tune {
		f(r)
	}
	go r.Run(context.Background())
	return r
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

func closeWithin(t *testing.T, r *Recorder, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r.Close(ctx)
}

func TestRecordNeverBlocksWhileTheWriterIsStuck(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	r := started(t, w, 4, func(r *Recorder) { r.batchSize = 1 })

	done := make(chan struct{})
	go func() {
		for range 1000 {
			r.Record(ev("chat"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a stuck writer")
	}
	close(w.block)
	closeWithin(t, r, 5*time.Second)
}

func TestAFullQueueDropsTheOldestAndCountsIt(t *testing.T) {
	metricReader()
	before := droppedFor(t, reasonOverflow)
	w := &fakeWriter{}
	r := NewRecorder(w, 3, nil)

	for _, route := range []string{"a", "b", "c", "d", "e"} {
		r.Record(ev(route))
	}
	if got := droppedFor(t, reasonOverflow) - before; got != 2 {
		t.Errorf("counted %d overflow drops, want 2", got)
	}

	go r.Run(context.Background())
	closeWithin(t, r, 5*time.Second)
	sizes := w.batchSizes()
	if len(sizes) != 1 || sizes[0] != 3 {
		t.Fatalf("batches = %v, want one of 3", sizes)
	}
	var routes []string
	for _, e := range w.batches[0] {
		routes = append(routes, e.RouteAlias)
	}
	if got := routes; got[0] != "c" || got[1] != "d" || got[2] != "e" {
		t.Errorf("kept %v, want the newest three", got)
	}
}

func TestEventsAreBatchedBySize(t *testing.T) {
	w := &fakeWriter{}
	r := NewRecorder(w, 100, nil)
	r.batchSize = 5
	r.flushEvery = time.Hour
	for range 12 {
		r.Record(ev("chat"))
	}
	go r.Run(context.Background())
	eventually(t, "two full batches", func() bool { return w.written() >= 10 })
	closeWithin(t, r, 5*time.Second)

	if sizes := w.batchSizes(); len(sizes) != 3 || sizes[0] != 5 || sizes[1] != 5 || sizes[2] != 2 {
		t.Errorf("batches = %v, want 5, 5, 2", sizes)
	}
}

func TestAnIdleBatchIsWrittenOnTheTimer(t *testing.T) {
	w := &fakeWriter{}
	r := started(t, w, 100)
	r.Record(ev("chat"))
	eventually(t, "the timer flush", func() bool { return w.written() == 1 })
	closeWithin(t, r, 5*time.Second)
}

func TestCloseWritesWhatIsQueued(t *testing.T) {
	w := &fakeWriter{}
	r := NewRecorder(w, 100, nil)
	r.flushEvery = time.Hour
	for range 7 {
		r.Record(ev("chat"))
	}
	go r.Run(context.Background())
	closeWithin(t, r, 5*time.Second)

	if got := w.written(); got != 7 {
		t.Errorf("wrote %d events, want 7", got)
	}
	r.Record(ev("late"))
	if got := w.written(); got != 7 {
		t.Errorf("an event recorded after Close was written")
	}
}

func TestAFailedBatchIsRetriedOnceThenDroppedAndCounted(t *testing.T) {
	metricReader()
	before := droppedFor(t, reasonWriteFailed)
	w := &fakeWriter{failing: 2}
	r := started(t, w, 100)
	r.Record(ev("lost"))
	eventually(t, "two attempts", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.calls >= 2
	})
	eventually(t, "the drop to be counted", func() bool { return droppedFor(t, reasonWriteFailed)-before == 1 })

	r.Record(ev("kept"))
	eventually(t, "the next batch to be written", func() bool { return w.written() == 1 })
	closeWithin(t, r, 5*time.Second)
}

func TestAFailedBatchThatSucceedsOnRetryIsNotDropped(t *testing.T) {
	metricReader()
	before := droppedFor(t, reasonWriteFailed)
	w := &fakeWriter{failing: 1}
	r := started(t, w, 100)
	r.Record(ev("chat"))
	eventually(t, "the retry to write it", func() bool { return w.written() == 1 })
	closeWithin(t, r, 5*time.Second)
	if got := droppedFor(t, reasonWriteFailed) - before; got != 0 {
		t.Errorf("counted %d drops for a batch the retry wrote", got)
	}
}

func TestCloseGivesUpWhenItsDeadlinePasses(t *testing.T) {
	metricReader()
	before := droppedFor(t, reasonWriteFailed)
	w := &fakeWriter{block: make(chan struct{})}
	r := NewRecorder(w, 100, nil)
	r.flushEvery = time.Hour
	r.Record(ev("stuck"))
	go r.Run(context.Background())

	start := time.Now()
	closeWithin(t, r, 50*time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Close outlived its deadline")
	}
	if got := droppedFor(t, reasonWriteFailed) - before; got != 1 {
		t.Errorf("counted %d dropped events, want the 1 left unwritten", got)
	}
}
