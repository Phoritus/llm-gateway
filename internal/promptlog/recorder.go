package promptlog

import (
	"context"
	"log/slog"

	"llm-gateway/internal/batch"
	"llm-gateway/internal/store"
)

const (
	queueMemory = 16 << 20
	QueueSize   = queueMemory / (2 * MaxBodyBytes)
	batchSize   = 50
)

// Writer stores a batch of logs all or nothing, and a log it is handed twice
// once.
type Writer interface {
	WriteRequestLogs(ctx context.Context, logs []store.RequestLog) error
}

// Recorder queues logs without waiting and writes them in batches.
type Recorder struct {
	*batch.Queue[store.RequestLog]
}

// NewRecorder builds a Recorder of QueueSize logs. A nil logger is
// slog.Default().
func NewRecorder(w Writer, logger *slog.Logger) *Recorder {
	return &Recorder{batch.New(batch.Config{
		Meter: "llm-gateway/promptlog", Dropped: "prompt_log.entries.dropped", Depth: "prompt_log.queue.depth",
		Items: "prompt logs", BatchSize: batchSize,
	}, w.WriteRequestLogs, QueueSize, logger)}
}
