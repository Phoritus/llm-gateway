package store

import (
	"context"
	"fmt"
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
