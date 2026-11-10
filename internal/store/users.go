package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation is the Postgres error code for a unique index conflict.
const uniqueViolation = "23505"

// NormalizeEmail lowercases and trims an address. The column is citext so the
// database would deduplicate anyway, but normalising on the way in keeps
// lookups, rate-limit keys and outgoing mail consistent.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// SignupResult reports the rows created by a successful signup.
type SignupResult struct {
	UserID      string
	WorkspaceID string
}

// CreateUserWithWorkspace registers a user, creates their personal workspace
// and makes them its admin, in one transaction. The three rows are useless
// apart: a user with no workspace could not sign in anywhere, and a workspace
// with no admin could never be administered.
func (s *Store) CreateUserWithWorkspace(ctx context.Context, email, passwordHash, workspaceName string) (SignupResult, error) {
	var out SignupResult

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin signup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id`,
		NormalizeEmail(email), passwordHash,
	).Scan(&out.UserID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return out, ErrEmailTaken
		}
		return out, fmt.Errorf("insert user: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO workspaces (name, owner_id) VALUES ($1, $2) RETURNING id`,
		workspaceName, out.UserID,
	).Scan(&out.WorkspaceID); err != nil {
		return out, fmt.Errorf("insert workspace: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id) VALUES ($1, $2)`,
		out.WorkspaceID, out.UserID,
	); err != nil {
		return out, fmt.Errorf("insert membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit signup: %w", err)
	}
	return out, nil
}

const userColumns = `id, email, password_hash, email_verified_at, created_at, updated_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.EmailVerifiedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}

// UserByEmail looks up a sign-in identity. Callers must treat ErrNotFound and
// a wrong password identically so the response does not reveal whether an
// address is registered.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, NormalizeEmail(email)))
}

// UserByID looks up a user by primary key.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// DefaultWorkspace returns the workspace a user signs in to: the one they own
// if it still exists, otherwise their oldest membership.
//
// It returns the whole row rather than the id alone because the join that finds
// it already reads the workspaces table, so the name is free. Login needs the
// name for its response, and buying it here is what lets that handler answer
// from the rows it already holds instead of re-resolving the session it just
// created.
func (s *Store) DefaultWorkspace(ctx context.Context, userID string) (*Workspace, error) {
	var w Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT w.id, w.name, w.owner_id, w.created_at
		   FROM workspace_members m
		   JOIN workspaces w ON w.id = m.workspace_id
		  WHERE m.user_id = $1
		  ORDER BY (w.owner_id = $1) DESC, m.created_at ASC
		  LIMIT 1`, userID).Scan(&w.ID, &w.Name, &w.OwnerID, &w.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("select default workspace: %w", err)
	}
	return &w, nil
}

// ChangePasswordAndRevokeSessions replaces a user's password hash and revokes
// every session but one, in a single transaction, and reports how many it
// revoked.
//
// The two writes are one transaction because the alternative is a state nobody
// chose. Committing the hash first and revoking afterwards means a failure in
// between leaves the new password live and every other session still usable,
// while the caller is told 500 -- so the user concludes nothing happened, keeps
// using a password that no longer works, and never learns that whoever else was
// in the account still is. Changing a password is often a response to exactly
// that suspicion, which makes the half-applied version the worst of the three
// outcomes. Rolling both back is the honest failure: the 500 then means what it
// says.
//
// exceptSessionID is left alive so the caller is not signed out of the session
// they are changing their password from. Empty revokes all of them.
func (s *Store) ChangePasswordAndRevokeSessions(ctx context.Context, userID, passwordHash, exceptSessionID string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin password change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`,
		userID, passwordHash)
	if err != nil {
		return 0, fmt.Errorf("update password hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}

	// The exception travels as NULL rather than an empty string, for the reason
	// RevokeUserSessions gives: Postgres does not promise to short-circuit OR,
	// so a ''::uuid cast could be evaluated and fail.
	var except any
	if exceptSessionID != "" {
		except = exceptSessionID
	}
	revoked, err := tx.Exec(ctx,
		`UPDATE sessions
		    SET revoked_at = now()
		  WHERE user_id = $1
		    AND revoked_at IS NULL
		    AND ($2::uuid IS NULL OR id <> $2::uuid)`,
		userID, except)
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit password change: %w", err)
	}
	return revoked.RowsAffected(), nil
}
