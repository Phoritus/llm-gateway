package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests need a real Postgres, because what internal/store owns --
// transaction boundaries, the citext email column, single-use token updates --
// lives in SQL and would be meaningless against a fake. They do not touch Redis.
//
// They run in a database of their own, named after DATABASE_TEST_URL's with a
// "_store" suffix and created on demand. internal/api truncates every table in
// the database it is given, and `go test ./...` runs packages in parallel, so
// sharing one database would let a truncate there land in the middle of a test
// here.
const envTestDatabase = "DATABASE_TEST_URL"

const testHash = "$argon2id$test-hash-one"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv(envTestDatabase)
	if raw == "" {
		t.Skipf("%s is not set; skipping the database-backed store tests", envTestDatabase)
	}
	ctx := context.Background()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", envTestDatabase, err)
	}
	dbName := strings.TrimPrefix(u.Path, "/") + "_store"

	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatalf("connect to %s: %v", envTestDatabase, err)
	}
	defer func() { _ = admin.Close(ctx) }()

	var exists bool
	if err := admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
		t.Fatalf("look up database %s: %v", dbName, err)
	}
	if !exists {
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{dbName}.Sanitize()); err != nil {
			t.Fatalf("create database %s: %v", dbName, err)
		}
	}

	u.Path = "/" + dbName
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// One statement so the foreign keys never see an inconsistent state.
	if _, err := st.pool.Exec(ctx,
		`TRUNCATE provider_credentials, sessions, email_verification_tokens, password_reset_tokens,
		          workspace_members, workspaces, users CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

func signup(t *testing.T, st *Store, email string) SignupResult {
	t.Helper()
	res, err := st.CreateUserWithWorkspace(context.Background(), email, testHash, "Workspace of "+NormalizeEmail(email))
	if err != nil {
		t.Fatalf("signup %s: %v", email, err)
	}
	return res
}

func hashOf(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

// newSession creates a live session for a signed-up user and returns it with
// the token hash it can be looked up by.
func newSession(t *testing.T, st *Store, res SignupResult, label string) (*Session, []byte) {
	t.Helper()
	h := hashOf(label)
	sess, err := st.CreateSession(context.Background(), SessionInput{
		TokenHash:   h,
		UserID:      res.UserID,
		WorkspaceID: res.WorkspaceID,
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", label, err)
	}
	return sess, h
}

// alive reports whether a session still authenticates.
func alive(t *testing.T, st *Store, tokenHash []byte) bool {
	t.Helper()
	_, err := st.IdentityByTokenHash(context.Background(), tokenHash)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNotFound):
		return false
	default:
		t.Fatalf("resolve session: %v", err)
		return false
	}
}

func count(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func passwordHash(t *testing.T, st *Store, userID string) string {
	t.Helper()
	u, err := st.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	return u.PasswordHash
}
