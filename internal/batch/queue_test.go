package batch

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricReader installs a meter provider once. A counter binds to the first
// global provider only, so every test that reads a metric shares it.
var metricReader = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

func droppedFor(t *testing.T, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != testConfig.Dropped {
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
	mu       sync.Mutex
	attempts [][]item
	batches  [][]item
	calls    int
	failing  int
	refuses  func(item) bool
	block    chan struct{}
}

func (w *fakeWriter) WriteBatch(ctx context.Context, events []item) error {
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
	w.attempts = append(w.attempts, append([]item(nil), events...))
	if w.calls <= w.failing {
		return errors.New("database down")
	}
	if w.refuses != nil && slices.ContainsFunc(events, w.refuses) {
		return errors.New("value out of range")
	}
	w.batches = append(w.batches, append([]item(nil), events...))
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

type item struct{ WorkspaceID, RouteAlias string }

var testConfig = Config{Meter: "llm-gateway/batch-test", Dropped: "batch_test.items.dropped",
	Depth: "batch_test.queue.depth", Items: "test items"}

func ev(route string) item { return item{WorkspaceID: "ws", RouteAlias: route} }

func newQueue(w *fakeWriter, capacity int) *Queue[item] {
	return New(testConfig, w.WriteBatch, capacity, nil)
}

func started(t *testing.T, w *fakeWriter, capacity int, tune ...func(*Queue[item])) *Queue[item] {
	t.Helper()
	metricReader()
	r := newQueue(w, capacity)
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

func closeWithin(t *testing.T, r *Queue[item], d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r.Close(ctx)
}

func TestRecordNeverBlocksWhileTheWriterIsStuck(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	r := started(t, w, 4, func(r *Queue[item]) { r.batchSize = 1 })

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
	before := droppedFor(t, ReasonOverflow)
	w := &fakeWriter{}
	r := newQueue(w, 3)

	for _, route := range []string{"a", "b", "c", "d", "e"} {
		r.Record(ev(route))
	}
	if got := droppedFor(t, ReasonOverflow) - before; got != 2 {
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

func TestItemsAreBatchedBySize(t *testing.T) {
	w := &fakeWriter{}
	r := newQueue(w, 100)
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
	r := newQueue(w, 100)
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
	before := droppedFor(t, ReasonWriteFailed)
	w := &fakeWriter{failing: 2}
	r := started(t, w, 100)
	r.Record(ev("lost"))
	eventually(t, "two attempts", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.calls >= 2
	})
	eventually(t, "the drop to be counted", func() bool { return droppedFor(t, ReasonWriteFailed)-before == 1 })

	r.Record(ev("kept"))
	eventually(t, "the next batch to be written", func() bool { return w.written() == 1 })
	closeWithin(t, r, 5*time.Second)
}

func TestAFailedBatchThatSucceedsOnRetryIsNotDropped(t *testing.T) {
	metricReader()
	before := droppedFor(t, ReasonWriteFailed)
	w := &fakeWriter{failing: 1}
	r := started(t, w, 100)
	r.Record(ev("chat"))
	eventually(t, "the retry to write it", func() bool { return w.written() == 1 })
	closeWithin(t, r, 5*time.Second)
	if got := droppedFor(t, ReasonWriteFailed) - before; got != 0 {
		t.Errorf("counted %d drops for a batch the retry wrote", got)
	}
}

func TestCloseGivesUpWhenItsDeadlinePasses(t *testing.T) {
	metricReader()
	before := droppedFor(t, ReasonShutdown)
	w := &fakeWriter{block: make(chan struct{})}
	r := newQueue(w, 100)
	r.flushEvery = time.Hour
	r.Record(ev("stuck"))
	go r.Run(context.Background())

	start := time.Now()
	closeWithin(t, r, 50*time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Close outlived its deadline")
	}
	if got := droppedFor(t, ReasonShutdown) - before; got != 1 {
		t.Errorf("counted %d events dropped for shutdown, want the 1 left unwritten", got)
	}
}

func TestAnItemRecordedAfterCloseIsDroppedForShutdown(t *testing.T) {
	metricReader()
	r := started(t, &fakeWriter{}, 10)
	closeWithin(t, r, 5*time.Second)
	overflow, shutdown := droppedFor(t, ReasonOverflow), droppedFor(t, ReasonShutdown)

	r.Record(ev("late"))
	if got := droppedFor(t, ReasonOverflow) - overflow; got != 0 {
		t.Errorf("counted %d overflow drops with an empty queue", got)
	}
	if got := droppedFor(t, ReasonShutdown) - shutdown; got != 1 {
		t.Errorf("counted %d shutdown drops, want 1", got)
	}
}

func TestOneRefusedItemCostsOnlyItself(t *testing.T) {
	metricReader()
	before := droppedFor(t, ReasonWriteFailed)
	w := &fakeWriter{refuses: func(e item) bool { return e.RouteAlias == "refused" }}
	r := newQueue(w, 100)
	r.retryWait = time.Millisecond
	for _, e := range []item{
		{WorkspaceID: "alice", RouteAlias: "chat"},
		{WorkspaceID: "bob", RouteAlias: "refused"},
		{WorkspaceID: "carol", RouteAlias: "chat"},
	} {
		r.Record(e)
	}
	go r.Run(context.Background())
	closeWithin(t, r, 5*time.Second)

	var kept []string
	for _, b := range w.batches {
		for _, e := range b {
			kept = append(kept, e.WorkspaceID)
		}
	}
	if !slices.Equal(kept, []string{"alice", "carol"}) {
		t.Errorf("stored events of %v, want alice and carol: one workspace's refused event took the others with it", kept)
	}
	if got := droppedFor(t, ReasonWriteFailed) - before; got != 1 {
		t.Errorf("counted %d failed writes, want the 1 refused event", got)
	}
}
