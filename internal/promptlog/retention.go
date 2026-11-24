package promptlog

import (
	"context"
	"log/slog"
	"time"
)

const pruneBatch = 1000

// Pruner deletes the logs each workspace's retention has passed.
type Pruner interface {
	PruneRequestLogs(ctx context.Context, now time.Time, batch int) (int64, error)
}

// RunRetention prunes logs once at start and then every interval, until ctx
// ends.
func RunRetention(ctx context.Context, p Pruner, every time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	prune := func() {
		n, err := p.PruneRequestLogs(ctx, time.Now(), pruneBatch)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("prompt log retention failed", slog.Int64("deleted", n), slog.String("error", err.Error()))
		case n > 0:
			logger.Info("prompt logs pruned", slog.Int64("deleted", n))
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
