package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the single data-access layer. Every tenant-scoped method takes a
// workspace id as an explicit argument and filters on it, which is where the
// isolation invariant is enforced; handlers read that id from the session and
// never from a request body.
type Store struct {
	pool *pgxpool.Pool
}

// Pool exposes the connection pool for callers that need to run their own
// query, such as the readiness probe.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Open connects to Postgres and verifies the connection before returning, so
// an unreachable database fails startup rather than the first request.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// Free-tier Postgres allows few connections, so stay well under the limit
	// and recycle idle ones rather than holding them open.
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases every pooled connection.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping reports whether Postgres is currently reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
