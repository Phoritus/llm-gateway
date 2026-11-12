package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/provider"
)

const maxCredentialsPerWorkspace = 50

// Credentials stores provider API keys sealed under the master key.
type Credentials struct {
	store *Store
	keys  *crypto.Keyring
}

func NewCredentials(s *Store, keys *crypto.Keyring) *Credentials {
	return &Credentials{store: s, keys: keys}
}

// Credential is the view of a stored key that may leave the server.
type Credential struct {
	ID          string
	WorkspaceID string
	Provider    provider.Vendor
	Name        string
	Last4       string
	CreatedAt   time.Time
}

type NewCredential struct {
	Provider provider.Vendor
	Name     string
	Secret   string
}

func credentialAAD(workspaceID, id uuid.UUID, vendor provider.Vendor) []byte {
	aad := append([]byte("llm-gateway/provider-credential v1\x00"), workspaceID[:]...)
	aad = append(aad, id[:]...)
	return append(aad, vendor...)
}

func last4(secret string) string {
	r := []rune(secret)
	if len(r) > 4 {
		r = r[len(r)-4:]
	}
	return string(r)
}

// Create seals the secret and stores it, or returns ErrLimitReached when the
// workspace is full.
func (c *Credentials) Create(ctx context.Context, workspaceID string, in NewCredential) (*Credential, error) {
	ws, err := uuid.Parse(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace id: %w", err)
	}
	id := uuid.New()
	sealed := c.keys.Seal([]byte(in.Secret), credentialAAD(ws, id, in.Provider))
	suffix := last4(in.Secret)

	tx, err := c.store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create credential: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Under READ COMMITTED, concurrent inserts each count the rows before any
	// of them commits and all pass the cap. Locking the workspace row
	// serializes creates within one workspace and leaves others unaffected.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID); err != nil {
		return nil, fmt.Errorf("lock workspace: %w", err)
	}

	var out Credential
	err = tx.QueryRow(ctx,
		`INSERT INTO provider_credentials (id, workspace_id, provider, name, key_id, ciphertext, last4)
		 SELECT $1, $2, $3, $4, $5, $6, $7
		  WHERE (SELECT count(*) FROM provider_credentials WHERE workspace_id = $2) < $8
		 RETURNING id, workspace_id, provider, name, last4, created_at`,
		id.String(), workspaceID, string(in.Provider), in.Name, string(sealed.KeyID), sealed.Ciphertext, suffix,
		maxCredentialsPerWorkspace,
	).Scan(&out.ID, &out.WorkspaceID, &out.Provider, &out.Name, &out.Last4, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLimitReached
	}
	if err != nil {
		return nil, fmt.Errorf("insert provider credential: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit provider credential: %w", err)
	}
	return &out, nil
}

// List returns the workspace's credentials, newest first.
func (c *Credentials) List(ctx context.Context, workspaceID string) ([]Credential, error) {
	rows, err := c.store.pool.Query(ctx,
		`SELECT id, workspace_id, provider, name, last4, created_at
		   FROM provider_credentials
		  WHERE workspace_id = $1
		  ORDER BY created_at DESC, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list provider credentials: %w", err)
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var cred Credential
		if err := rows.Scan(&cred.ID, &cred.WorkspaceID, &cred.Provider, &cred.Name, &cred.Last4, &cred.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan provider credential: %w", err)
		}
		out = append(out, cred)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list provider credentials: %w", err)
	}
	return out, nil
}

// Delete removes a credential, or returns ErrNotFound when the workspace has
// no such one.
func (c *Credentials) Delete(ctx context.Context, workspaceID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	tag, err := c.store.pool.Exec(ctx,
		`DELETE FROM provider_credentials WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete provider credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Secret opens a credential's key. The error never carries the plaintext.
func (c *Credentials) Secret(ctx context.Context, workspaceID, id string) (string, error) {
	ws, err := uuid.Parse(workspaceID)
	if err != nil {
		return "", ErrNotFound
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return "", ErrNotFound
	}
	var (
		vendor provider.Vendor
		keyID  string
		blob   []byte
	)
	err = c.store.pool.QueryRow(ctx,
		`SELECT provider, key_id, ciphertext FROM provider_credentials
		  WHERE workspace_id = $1 AND id = $2`, workspaceID, id).Scan(&vendor, &keyID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("select provider credential: %w", err)
	}
	plain, err := c.keys.Open(crypto.Sealed{KeyID: crypto.KeyID(keyID), Ciphertext: blob},
		credentialAAD(ws, rowID, vendor))
	if err != nil {
		return "", fmt.Errorf("open provider credential %s: %w", id, err)
	}
	return string(plain), nil
}
