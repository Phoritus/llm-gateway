package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"llm-gateway/migrations"
)

// Migrate brings the schema up to the latest embedded version. It runs at
// startup so a fresh deployment and a fresh developer checkout need no extra
// step, and it is safe to run concurrently: goose takes a lock on its own
// version table.
func (s *Store) Migrate(ctx context.Context) error {
	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
