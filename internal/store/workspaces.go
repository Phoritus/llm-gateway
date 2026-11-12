package store

import (
	"context"
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
