package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"llm-gateway/internal/store"
)

const (
	// batchSize is the most events one statement carries.
	batchSize = 200
	// flushEvery is how long an event waits for company before it is written.
	flushEvery = time.Second
	retryWait  = time.Second
	// writeTimeout bounds one attempt at a batch, and the whole of writing a
	// failed batch one event at a time, so that an unreachable database costs
	// the queue seconds of draining rather than all of it.
	writeTimeout = 10 * time.Second
)

// The reasons an event is lost. They are the label on the drop counter, so
// they are a fixed set.
const (
	reasonOverflow    = "overflow"
	reasonWriteFailed = "write_failed"
	reasonShutdown    = "shutdown"
)

var dropped, _ = otel.Meter("llm-gateway/usage").Int64Counter("usage.events.dropped",
	metric.WithDescription("Usage events lost, by reason."))

type Event = store.UsageEvent

// Writer stores a batch of events and their rollup in one step.
type Writer interface {
	WriteBatch(ctx context.Context, events []Event) error
}

// Recorder is the write side of usage. Record never waits, so logging cannot
// delay a response; Run drains the queue into the Writer in batches.
type Recorder struct {
	writer Writer
	logger *slog.Logger
	queue  chan Event

	// mu orders Close against Record: a send must not race the channel closing.
	mu     sync.RWMutex
	closed bool

	// runCtx ends the writes in flight when Close runs out of time.
	runCtx context.Context
	abort  context.CancelFunc
	done   chan struct{}

	batchSize  int
	flushEvery time.Duration
	retryWait  time.Duration
}

// NewRecorder builds a Recorder that holds up to capacity events. A nil logger
// is slog.Default().
func NewRecorder(w Writer, capacity int, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	runCtx, abort := context.WithCancel(context.Background())
	return &Recorder{
		writer: w, logger: logger, queue: make(chan Event, capacity),
		runCtx: runCtx, abort: abort, done: make(chan struct{}),
		batchSize: batchSize, flushEvery: flushEvery, retryWait: retryWait,
	}
}

// RegisterMetrics exposes how many events are waiting to be written.
func (r *Recorder) RegisterMetrics() error {
	_, err := otel.Meter("llm-gateway/usage").Int64ObservableGauge("usage.queue.depth",
		metric.WithDescription("Usage events waiting to be written."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(len(r.queue)))
			return nil
		}))
	return err
}

// Record queues an event and returns at once. A full queue gives up its oldest
// event for the new one: the freshest usage is the more useful to lose last.
//
// The event's ID is minted here, once, so every attempt to write it names the
// same row.
func (r *Recorder) Record(e Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		countDropped(1, reasonShutdown)
		return
	}
	e.ID = uuid.NewString()
	select {
	case r.queue <- e:
		return
	default:
	}
	select {
	case <-r.queue:
		countDropped(1, reasonOverflow)
	default:
	}
	select {
	case r.queue <- e:
	default:
		// Other requests refilled the slot first.
		countDropped(1, reasonOverflow)
	}
}

func countDropped(n int, reason string) {
	dropped.Add(context.Background(), int64(n), metric.WithAttributes(attribute.String("reason", reason)))
}

// Run writes queued events until Close has drained the queue or ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	defer close(r.done)
	stop := context.AfterFunc(ctx, r.abort)
	defer stop()

	batch := make([]Event, 0, r.batchSize)
	tick := time.NewTicker(r.flushEvery)
	defer tick.Stop()

	flush := func() {
		if len(batch) > 0 {
			r.write(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= r.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-r.runCtx.Done():
			lost := len(batch)
		drain:
			for {
				select {
				case _, ok := <-r.queue:
					if !ok {
						break drain
					}
					lost++
				default:
					break drain
				}
			}
			if lost > 0 {
				countDropped(lost, reasonShutdown)
			}
			return
		}
	}
}

// write stores a batch. A batch that fails twice is written one event at a
// time: the database refuses a whole batch for one row it cannot store, and
// that row's workspace must not cost every other workspace its events. A
// database that is down fails those writes too, inside one writeTimeout, so
// it cannot hold the queue, which overflows by itself soon enough.
func (r *Recorder) write(batch []Event) {
	err := r.attempt(r.runCtx, batch, 1)
	if err != nil {
		select {
		case <-time.After(r.retryWait):
			err = r.attempt(r.runCtx, batch, 2)
		case <-r.runCtx.Done():
		}
	}
	switch {
	case err == nil:
		return
	case r.runCtx.Err() != nil:
		countDropped(len(batch), reasonShutdown)
		return
	case len(batch) == 1:
		countDropped(1, reasonWriteFailed)
		return
	}

	ctx, cancel := context.WithTimeout(r.runCtx, writeTimeout)
	defer cancel()
	failed := 0
	for i := range batch {
		one := r.writer.WriteBatch(ctx, batch[i:i+1])
		if one == nil {
			continue
		}
		err = one
		if ctx.Err() != nil {
			rest := len(batch) - i
			if r.runCtx.Err() != nil {
				countDropped(rest, reasonShutdown)
			} else {
				failed += rest
			}
			break
		}
		failed++
	}
	if failed > 0 {
		countDropped(failed, reasonWriteFailed)
		r.logger.Warn("usage events dropped after writing their batch one at a time",
			slog.Int("events", failed), slog.Int("batch", len(batch)), slog.String("error", err.Error()))
	}
}

func (r *Recorder) attempt(ctx context.Context, batch []Event, n int) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err := r.writer.WriteBatch(ctx, batch)
	if err != nil {
		r.logger.Warn("usage batch write failed",
			slog.Int("events", len(batch)), slog.Int("attempt", n), slog.String("error", err.Error()))
	}
	return err
}

// Close stops accepting events and waits for the queue to be written. When ctx
// ends first, the write in progress is cut off and what is left is counted as
// dropped for shutdown, as is any event recorded from here on.
func (r *Recorder) Close(ctx context.Context) {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()

	select {
	case <-r.done:
	case <-ctx.Done():
		r.abort()
		<-r.done
	}
}
