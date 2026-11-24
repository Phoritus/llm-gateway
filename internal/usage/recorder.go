package usage

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"llm-gateway/internal/batch"
	"llm-gateway/internal/store"
)

type Event = store.UsageEvent

// Writer stores a batch of events and their rollup in one step.
type Writer interface {
	WriteBatch(ctx context.Context, events []Event) error
}

// Recorder is the write side of usage. Record never waits, so logging cannot
// delay a response; Run drains the queue into the Writer in batches.
type Recorder struct {
	*batch.Queue[Event]
}

// NewRecorder builds a Recorder that holds up to capacity events. A nil logger
// is slog.Default().
func NewRecorder(w Writer, capacity int, logger *slog.Logger) *Recorder {
	return &Recorder{batch.New(batch.Config{
		Meter: "llm-gateway/usage", Dropped: "usage.events.dropped", Depth: "usage.queue.depth",
		Items: "usage events",
	}, w.WriteBatch, capacity, logger)}
}

// Record queues an event and returns at once.
//
// An event without an ID is given one here, once, so every attempt to write
// it names the same row. A caller that sets its own keeps it.
func (r *Recorder) Record(e Event) {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	r.Queue.Record(e)
}
