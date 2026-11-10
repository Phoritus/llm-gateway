package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The two token tables are identical in shape and differ only in what spending
// a token does, which is why the create half is shared and the consume half is
// not. Each name is a constant in this file, never a caller's string, so the
// interpolation below cannot carry input.
const (
	verificationTokens = "email_verification_tokens"
	resetTokens        = "password_reset_tokens"
)

// createToken stores a token hash, replacing any outstanding one for the same
// user so that issuing a new link invalidates the one it replaces.
func (s *Store) createToken(ctx context.Context, table, userID string, tokenHash []byte, expiresAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin token insert on %s: %w", table, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM `+table+` WHERE user_id = $1 AND used_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("clear outstanding rows in %s: %w", table, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+table+` (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt); err != nil {
		return fmt.Errorf("insert into %s: %w", table, err)
	}
	return tx.Commit(ctx)
}

// CreateVerificationToken stores the hash of an email-verification token.
func (s *Store) CreateVerificationToken(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time) error {
	return s.createToken(ctx, verificationTokens, userID, tokenHash, expiresAt)
}

// CreatePasswordResetToken stores the hash of a password-reset token.
func (s *Store) CreatePasswordResetToken(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time) error {
	return s.createToken(ctx, resetTokens, userID, tokenHash, expiresAt)
}

// ConsumeVerificationToken marks a token used and the address verified, in one
// transaction. The UPDATE ... WHERE used_at IS NULL is what makes the token
// single-use even if two requests present it at the same moment: only one
// statement can match the row.
func (s *Store) ConsumeVerificationToken(ctx context.Context, tokenHash []byte) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin verification: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID string
	err = tx.QueryRow(ctx,
		`UPDATE email_verification_tokens
		    SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING user_id`,
		tokenHash).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("consume verification token: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users
		    SET email_verified_at = COALESCE(email_verified_at, now()), updated_at = now()
		  WHERE id = $1`,
		userID); err != nil {
		return "", fmt.Errorf("mark email verified: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit verification: %w", err)
	}
	return userID, nil
}

// ConsumePasswordResetToken spends a reset token, sets the new password hash
// and revokes every session the user holds, atomically. Doing all three in one
// transaction means a reset can never leave an old session usable.
func (s *Store) ConsumePasswordResetToken(ctx context.Context, tokenHash []byte, newPasswordHash string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID string
	err = tx.QueryRow(ctx,
		`UPDATE password_reset_tokens
		    SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING user_id`,
		tokenHash).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("consume reset token: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`,
		userID, newPasswordHash); err != nil {
		return "", fmt.Errorf("update password: %w", err)
	}

	// A password reset is the recovery path for a compromised account, so
	// every existing session dies with it.
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); err != nil {
		return "", fmt.Errorf("revoke sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit reset: %w", err)
	}
	return userID, nil
}

// DeleteStaleTokens removes expired and already-spent tokens from both token
// tables and reports how many rows went.
func (s *Store) DeleteStaleTokens(ctx context.Context, usedGrace time.Duration) (int64, error) {
	var total int64
	for _, table := range []string{verificationTokens, resetTokens} {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM `+table+`
			  WHERE expires_at < now()
			     OR (used_at IS NOT NULL AND used_at < now() - make_interval(secs => $1))`,
			usedGrace.Seconds())
		if err != nil {
			return total, fmt.Errorf("delete stale rows from %s: %w", table, err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}
