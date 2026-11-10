package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateSession records a new session. Only the SHA-256 of the token reaches
// the database.
func (s *Store) CreateSession(ctx context.Context, in SessionInput) (*Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sessions (token_hash, user_id, workspace_id, expires_at, user_agent, ip)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, user_id, workspace_id, created_at, last_seen_at, expires_at, revoked_at`,
		in.TokenHash, in.UserID, in.WorkspaceID, in.ExpiresAt, in.UserAgent, in.IP,
	).Scan(&sess.ID, &sess.UserID, &sess.WorkspaceID, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &sess, nil
}

// IdentityByTokenHash resolves a session token hash to its caller in one
// query. Revoked and absolutely expired sessions are filtered out in SQL; the
// idle timeout is applied by the caller, which owns that policy.
//
// The join to workspace_members is what makes membership authoritative: if a
// member row is deleted, the very next request sees the change without any
// cache to invalidate.
func (s *Store) IdentityByTokenHash(ctx context.Context, tokenHash []byte) (*Identity, error) {
	var id Identity
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.user_id, s.workspace_id, s.created_at, s.last_seen_at, s.expires_at, s.revoked_at,
		        u.email, u.email_verified_at, w.name
		   FROM sessions s
		   JOIN users u ON u.id = s.user_id
		   JOIN workspaces w ON w.id = s.workspace_id
		   JOIN workspace_members m
		     ON m.workspace_id = s.workspace_id AND m.user_id = s.user_id
		  WHERE s.token_hash = $1
		    AND s.revoked_at IS NULL
		    AND s.expires_at > now()`,
		tokenHash,
	).Scan(&id.Session.ID, &id.Session.UserID, &id.Session.WorkspaceID, &id.Session.CreatedAt,
		&id.Session.LastSeenAt, &id.Session.ExpiresAt, &id.Session.RevokedAt,
		&id.Email, &id.EmailVerifiedAt, &id.WorkspaceName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("select identity: %w", err)
	}
	return &id, nil
}

// TouchSession refreshes the idle clock. Callers throttle how often they call
// it so an active session does not cause a write per request.
func (s *Store) TouchSession(ctx context.Context, sessionID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RevokeSession ends one session immediately.
func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		sessionID); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeUserSessions ends every session belonging to a user, optionally
// sparing one. Password changes and resets use this: a stolen password must
// not keep a session alive anywhere.
func (s *Store) RevokeUserSessions(ctx context.Context, userID, exceptSessionID string) (int64, error) {
	// The exception is passed as NULL rather than an empty string: Postgres
	// does not promise to short-circuit OR, so a ''::uuid cast could be
	// evaluated and fail.
	var except any
	if exceptSessionID != "" {
		except = exceptSessionID
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE sessions
		    SET revoked_at = now()
		  WHERE user_id = $1
		    AND revoked_at IS NULL
		    AND ($2::uuid IS NULL OR id <> $2::uuid)`,
		userID, except)
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteStaleSessions removes rows that can no longer authenticate anything:
// expired, or revoked long enough ago to be of no forensic use.
func (s *Store) DeleteStaleSessions(ctx context.Context, revokedGrace time.Duration) (int64, error) {
	// The grace period travels as seconds and is turned into an interval by
	// Postgres, rather than relying on the driver to encode a Go duration.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions
		  WHERE expires_at < now()
		     OR (revoked_at IS NOT NULL AND revoked_at < now() - make_interval(secs => $1))`,
		revokedGrace.Seconds())
	if err != nil {
		return 0, fmt.Errorf("delete stale sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
