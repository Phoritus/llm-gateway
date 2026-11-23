package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// IsMember reports whether a user belongs to a workspace.
//
// Membership is the whole authorisation model in Phase 1: a session already
// carries the workspace it was created against, and IdentityByTokenHash joins
// through this table, so a request from a non-member resolves to no identity
// at all. There is no role to consult, and so nothing a handler can forget to
// check.
//
// Which is why no handler calls this: the join has already decided. Its
// callers are the signup and cross-tenant isolation tests, which assert the
// membership row exists and that one workspace's member is not another's. A
// handler reaching for it would be re-checking what resolving the session
// proved.
func (s *Store) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND user_id = $2
		 )`,
		workspaceID, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("select membership: %w", err)
	}
	return exists, nil
}

// lockWorkspaceForCap serializes the count-then-insert of a per-workspace cap.
// Under READ COMMITTED, concurrent inserts each count the rows before any of
// them commits and all pass the cap. NO KEY UPDATE rather than UPDATE, so the
// lock does not block foreign-key checks from inserts into other tables that
// reference the workspace.
func lockWorkspaceForCap(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR NO KEY UPDATE`, workspaceID); err != nil {
		return fmt.Errorf("lock workspace: %w", err)
	}
	return nil
}

// WorkspaceSettings is the typed form of workspaces.settings. Every field's
// zero value is its default, so a row written before a field existed, and a
// ResolvedRoute built without settings, mean the defaults.
type WorkspaceSettings struct {
	// CacheOff turns caching off for every route in the workspace, whatever
	// the route's own mode.
	CacheOff bool `json:"cache_off,omitempty"`
	// SemanticCache opts the workspace into the semantic cache, which also
	// needs the deployment to run with SEMANTIC_CACHE on.
	SemanticCache bool `json:"semantic_cache,omitempty"`
}

func (s *Store) WorkspaceSettings(ctx context.Context, workspaceID string) (WorkspaceSettings, error) {
	id, err := parseID(workspaceID)
	if err != nil {
		return WorkspaceSettings{}, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT settings FROM workspaces WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceSettings{}, ErrNotFound
	}
	if err != nil {
		return WorkspaceSettings{}, fmt.Errorf("select workspace settings: %w", err)
	}
	return parseSettings(raw)
}

// SetWorkspaceSettings replaces a workspace's settings.
func (s *Store) SetWorkspaceSettings(ctx context.Context, workspaceID string, in WorkspaceSettings) error {
	id, err := parseID(workspaceID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode workspace settings: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE workspaces SET settings = $2 WHERE id = $1`, id, raw)
	if err != nil {
		return fmt.Errorf("update workspace settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func parseSettings(raw []byte) (WorkspaceSettings, error) {
	var out WorkspaceSettings
	if err := json.Unmarshal(raw, &out); err != nil {
		return WorkspaceSettings{}, fmt.Errorf("decode workspace settings: %w", err)
	}
	return out, nil
}
