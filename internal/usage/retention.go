package usage

import (
	"context"
	"log/slog"
	"time"
)

// pruneBatch is how many rows one delete statement removes.
const pruneBatch = 5000

// Pruner deletes raw events older than a cutoff and says how many it removed.
type Pruner interface {
	PruneEvents(ctx context.Context, before time.Time, batch int) (int64, error)
}

// RunRetention deletes raw events older than retention, once at start and then
// every interval, until ctx ends. The daily rollup is not touched.
func RunRetention(ctx context.Context, p Pruner, retention, every time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	prune := func() {
		n, err := p.PruneEvents(ctx, time.Now().Add(-retention), pruneBatch)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("usage retention failed", slog.Int64("deleted", n), slog.String("error", err.Error()))
		case n > 0:
			logger.Info("usage events pruned", slog.Int64("deleted", n))
		}
	}
	prune()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			prune()
		}
	}
}
