package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxActiveGatewayKeysPerWorkspace = 50

type GatewayKey struct {
	ID          string
	WorkspaceID string
	Name        string
	Prefix      string
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

type NewGatewayKey struct {
	Name   string
	Prefix string
	Hash   []byte
}

type ActiveGatewayKey struct {
	ID          string
	WorkspaceID string
}

const gatewayKeyColumns = `id, workspace_id, name, prefix, last_used_at, revoked_at, created_at`

func scanGatewayKey(row pgx.Row) (GatewayKey, error) {
	var k GatewayKey
	err := row.Scan(&k.ID, &k.WorkspaceID, &k.Name, &k.Prefix, &k.LastUsedAt, &k.RevokedAt, &k.CreatedAt)
	return k, err
}

// CreateGatewayKey stores a minted key, or returns ErrLimitReached when the
// workspace already holds the maximum number of unrevoked keys.
func (s *Store) CreateGatewayKey(ctx context.Context, workspaceID string, in NewGatewayKey) (*GatewayKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create gateway key: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockWorkspaceForCap(ctx, tx, workspaceID); err != nil {
		return nil, err
	}

	k, err := scanGatewayKey(tx.QueryRow(ctx,
		`INSERT INTO gateway_keys (id, workspace_id, name, prefix, hash)
		 SELECT $1, $2, $3, $4, $5
		  WHERE (SELECT count(*) FROM gateway_keys WHERE workspace_id = $2 AND revoked_at IS NULL) < $6
		 RETURNING `+gatewayKeyColumns,
		uuid.NewString(), workspaceID, in.Name, in.Prefix, in.Hash, maxActiveGatewayKeysPerWorkspace))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLimitReached
	}
	if err != nil {
		return nil, fmt.Errorf("insert gateway key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit gateway key: %w", err)
	}
	return &k, nil
}

// ListGatewayKeys returns the workspace's keys, revoked ones included, newest
// first.
func (s *Store) ListGatewayKeys(ctx context.Context, workspaceID string) ([]GatewayKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+gatewayKeyColumns+` FROM gateway_keys
		  WHERE workspace_id = $1
		  ORDER BY created_at DESC, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list gateway keys: %w", err)
	}
	defer rows.Close()

	var out []GatewayKey
	for rows.Next() {
		k, err := scanGatewayKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan gateway key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list gateway keys: %w", err)
	}
	return out, nil
}

// RevokeGatewayKey marks an unrevoked key revoked and returns its hash, so the
// caller can evict it from any cache. ErrNotFound covers a key that is absent,
// already revoked, or another workspace's.
func (s *Store) RevokeGatewayKey(ctx context.Context, workspaceID, id string) ([]byte, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var hash []byte
	err := s.pool.QueryRow(ctx,
		`UPDATE gateway_keys SET revoked_at = now()
		  WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL
		 RETURNING hash`, workspaceID, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("revoke gateway key: %w", err)
	}
	return hash, nil
}

// ActiveGatewayKeyByHash resolves a presented key's hash, or returns
// ErrNotFound when it is unknown or revoked.
func (s *Store) ActiveGatewayKeyByHash(ctx context.Context, hash []byte) (*ActiveGatewayKey, error) {
	var k ActiveGatewayKey
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id FROM gateway_keys WHERE hash = $1 AND revoked_at IS NULL`,
		hash).Scan(&k.ID, &k.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select gateway key: %w", err)
	}
	return &k, nil
}

// TouchGatewayKeys records when each key was last used, in one statement. A
// time never moves last_used_at backwards, so instances flushing out of order
// cannot regress it.
func (s *Store) TouchGatewayKeys(ctx context.Context, lastUsed map[string]time.Time) error {
	if len(lastUsed) == 0 {
		return nil
	}
	ids := make([]string, 0, len(lastUsed))
	times := make([]time.Time, 0, len(lastUsed))
	for id, at := range lastUsed {
		ids = append(ids, id)
		times = append(times, at)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE gateway_keys k
		    SET last_used_at = GREATEST(k.last_used_at, u.at)
		   FROM unnest($1::uuid[], $2::timestamptz[]) AS u(id, at)
		  WHERE k.id = u.id`, ids, times); err != nil {
		return fmt.Errorf("touch gateway keys: %w", err)
	}
	return nil
}
