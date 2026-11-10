package auth

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper defaults. The grace periods keep recently revoked rows around long
// enough to answer "when was I signed out" before they are deleted.
const (
	defaultSweepInterval = 10 * time.Minute
	revokedGrace         = 7 * 24 * time.Hour
	usedTokenGrace       = 24 * time.Hour
)

// RunSweeper deletes expired sessions and spent tokens until the context is
// cancelled. It runs in-process as the spec's background jobs do.
//
// Nothing depends on it for correctness: every query already filters on
// expires_at and revoked_at, so a sweeper that never ran would leave the
// service correct and the tables large. That is why a failed pass only logs
// and waits for the next one.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx, logger)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context, logger *slog.Logger) {
	// Bound each pass so a slow delete cannot hold a connection past the next
	// tick or block shutdown.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	sessions, err := s.store.DeleteStaleSessions(ctx, revokedGrace)
	if err != nil {
		logger.ErrorContext(ctx, "sweep sessions", slog.String("error", err.Error()))
		return
	}
	tokens, err := s.store.DeleteStaleTokens(ctx, usedTokenGrace)
	if err != nil {
		logger.ErrorContext(ctx, "sweep tokens", slog.String("error", err.Error()))
		return
	}
	if sessions > 0 || tokens > 0 {
		logger.InfoContext(ctx, "swept expired auth rows",
			slog.Int64("sessions", sessions), slog.Int64("tokens", tokens))
	}
}
