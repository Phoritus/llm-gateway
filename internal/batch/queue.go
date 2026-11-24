package batch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	defaultBatchSize = 200
	flushEvery       = time.Second
	retryWait        = time.Second
	writeTimeout     = 10 * time.Second
)

// The reasons an item is lost. They are the label on the drop counter, so
// they are a fixed set.
const (
	ReasonOverflow    = "overflow"
	ReasonWriteFailed = "write_failed"
	ReasonShutdown    = "shutdown"
)

// Config names a queue's metrics and log lines. Every name is a literal at
// the call site: a metric name per tenant would be a series per tenant.
type Config struct {
	// Meter is the instrumentation scope, "llm-gateway/usage" for instance.
	Meter string
	// Dropped is the counter of lost items, labelled by reason.
	Dropped string
	// Depth is the gauge of items waiting to be written.
	Depth string
	// Items names what is queued, in log lines: "usage events".
	Items string
	// BatchSize is the most items one write carries; zero is 200.
	BatchSize int
}

// Queue is the write side of a store. Record never waits, so a response is
// never delayed by it; Run drains the queue into write in batches.
type Queue[T any] struct {
	write   func(context.Context, []T) error
	cfg     Config
	logger  *slog.Logger
	queue   chan T
	dropped metric.Int64Counter

	closeMu sync.RWMutex
	closed  bool

	runCtx context.Context
	abort  context.CancelFunc
	done   chan struct{}

	batchSize  int
	flushEvery time.Duration
	retryWait  time.Duration
}

// New builds a Queue that holds up to capacity items and stores them with
// write, which must store a batch all or nothing and must store an item it
// is handed twice once. A nil logger is slog.Default().
func New[T any](cfg Config, write func(context.Context, []T) error, capacity int, logger *slog.Logger) *Queue[T] {
	if logger == nil {
		logger = slog.Default()
	}
	size := cfg.BatchSize
	if size <= 0 {
		size = defaultBatchSize
	}
	dropped, _ := otel.Meter(cfg.Meter).Int64Counter(cfg.Dropped,
		metric.WithDescription("Items lost, by reason: "+cfg.Items+"."))
	runCtx, abort := context.WithCancel(context.Background())
	return &Queue[T]{
		write: write, cfg: cfg, logger: logger, queue: make(chan T, capacity), dropped: dropped,
		runCtx: runCtx, abort: abort, done: make(chan struct{}),
		batchSize: size, flushEvery: flushEvery, retryWait: retryWait,
	}
}

// RegisterMetrics exposes how many items are waiting to be written.
func (q *Queue[T]) RegisterMetrics() error {
	_, err := otel.Meter(q.cfg.Meter).Int64ObservableGauge(q.cfg.Depth,
		metric.WithDescription("Items waiting to be written: "+q.cfg.Items+"."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(len(q.queue)))
			return nil
		}))
	return err
}

// Record queues an item and returns at once. A full queue gives up its oldest
// item for the new one: the freshest is the more useful to lose last.
func (q *Queue[T]) Record(item T) {
	q.closeMu.RLock()
	defer q.closeMu.RUnlock()
	if q.closed {
		q.countDropped(1, ReasonShutdown)
		return
	}
	select {
	case q.queue <- item:
		return
	default:
	}
	select {
	case <-q.queue:
		q.countDropped(1, ReasonOverflow)
	default:
	}
	select {
	case q.queue <- item:
	default:
		q.countDropped(1, ReasonOverflow)
	}
}

func (q *Queue[T]) countDropped(n int, reason string) {
	q.dropped.Add(context.Background(), int64(n), metric.WithAttributes(attribute.String("reason", reason)))
}

// Run writes queued items until Close has drained the queue or ctx ends.
func (q *Queue[T]) Run(ctx context.Context) {
	defer close(q.done)
	stop := context.AfterFunc(ctx, q.abort)
	defer stop()

	batch := make([]T, 0, q.batchSize)
	tick := time.NewTicker(q.flushEvery)
	defer tick.Stop()

	flush := func() {
		if len(batch) > 0 {
			q.store(batch)
			clear(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case item, ok := <-q.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, item)
			if len(batch) >= q.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-q.runCtx.Done():
			lost := len(batch)
		drain:
			for {
				select {
				case _, ok := <-q.queue:
					if !ok {
						break drain
					}
					lost++
				default:
					break drain
				}
			}
			if lost > 0 {
				q.countDropped(lost, ReasonShutdown)
			}
			return
		}
	}
}

func (q *Queue[T]) store(batch []T) {
	err := q.attempt(q.runCtx, batch, 1)
	if err != nil {
		select {
		case <-time.After(q.retryWait):
			err = q.attempt(q.runCtx, batch, 2)
		case <-q.runCtx.Done():
		}
	}
	switch {
	case err == nil:
		return
	case q.runCtx.Err() != nil:
		q.countDropped(len(batch), ReasonShutdown)
		return
	case len(batch) == 1:
		q.countDropped(1, ReasonWriteFailed)
		return
	}
	q.writeEachAlone(batch)
}

// writeEachAlone writes a batch that failed one item at a time, so an item
// that cannot be stored costs only itself.
func (q *Queue[T]) writeEachAlone(batch []T) {
	ctx, cancel := context.WithTimeout(q.runCtx, writeTimeout)
	defer cancel()
	var err error
	failed := 0
	for i := range batch {
		one := q.write(ctx, batch[i:i+1])
		if one == nil {
			continue
		}
		err = one
		if ctx.Err() != nil {
			rest := len(batch) - i
			if q.runCtx.Err() != nil {
				q.countDropped(rest, ReasonShutdown)
			} else {
				failed += rest
			}
			break
		}
		failed++
	}
	if failed > 0 {
		q.countDropped(failed, ReasonWriteFailed)
		q.logger.Warn(q.cfg.Items+" dropped after writing their batch one at a time",
			slog.Int("items", failed), slog.Int("batch", len(batch)), slog.String("error", err.Error()))
	}
}

func (q *Queue[T]) attempt(ctx context.Context, batch []T, n int) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err := q.write(ctx, batch)
	if err != nil {
		q.logger.Warn(q.cfg.Items+" batch write failed",
			slog.Int("items", len(batch)), slog.Int("attempt", n), slog.String("error", err.Error()))
	}
	return err
}

// Close stops accepting items and waits for the queue to be written. When ctx
// ends first, the write in progress is cut off and what is left is counted as
// dropped for shutdown, as is any item recorded from here on.
func (q *Queue[T]) Close(ctx context.Context) {
	q.closeMu.Lock()
	if !q.closed {
		q.closed = true
		close(q.queue)
	}
	q.closeMu.Unlock()

	select {
	case <-q.done:
	case <-ctx.Done():
		q.abort()
		<-q.done
	}
}
