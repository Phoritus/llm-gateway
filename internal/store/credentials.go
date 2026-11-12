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

// maxCredentialsPerWorkspace bounds what one tenant can make the table hold.
const maxCredentialsPerWorkspace = 50

// Credentials stores provider API keys sealed under the master key. It is the
// only code that sees a secret: a Credential carries none, and Secret is for
// the router to call when it needs to make an upstream request.
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

// credentialAAD binds a ciphertext to its tenant, row and provider, so one
// copied onto another row, or kept after its provider column is edited, fails
// to open. NUL cannot appear in a uuid or a Vendor, which keeps the fields
// from running into each other.
func credentialAAD(workspaceID, id string, vendor provider.Vendor) []byte {
	return []byte("llm-gateway/provider-credential v1\x00" + workspaceID + "\x00" + id + "\x00" + string(vendor))
}

// canonicalID parses a caller-supplied id so a malformed one is a miss rather
// than a driver error, and so the AAD is built from the form Create used.
func canonicalID(id string) (string, bool) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

func last4(secret string) string {
	r := []rune(secret)
	if len(r) > 4 {
		r = r[len(r)-4:]
	}
	return string(r)
}

// Create seals the secret and stores it, or returns ErrLimitReached when the
// workspace is full. The count and the insert are one statement.
func (c *Credentials) Create(ctx context.Context, workspaceID string, in NewCredential) (*Credential, error) {
	id := uuid.NewString()
	sealed := c.keys.Seal([]byte(in.Secret), credentialAAD(workspaceID, id, in.Provider))
	suffix := last4(in.Secret)

	var out Credential
	err := c.store.pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (id, workspace_id, provider, name, key_id, ciphertext, last4)
		 SELECT $1, $2, $3, $4, $5, $6, $7
		  WHERE (SELECT count(*) FROM provider_credentials WHERE workspace_id = $2) < $8
		 RETURNING id, workspace_id, provider, name, last4, created_at`,
		id, workspaceID, string(in.Provider), in.Name, string(sealed.KeyID), sealed.Ciphertext, suffix,
		maxCredentialsPerWorkspace,
	).Scan(&out.ID, &out.WorkspaceID, &out.Provider, &out.Name, &out.Last4, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLimitReached
	}
	if err != nil {
		return nil, fmt.Errorf("insert provider credential: %w", err)
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
	id, ok := canonicalID(id)
	if !ok {
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
	id, ok := canonicalID(id)
	if !ok {
		return "", ErrNotFound
	}
	var (
		vendor provider.Vendor
		keyID  string
		blob   []byte
	)
	err := c.store.pool.QueryRow(ctx,
		`SELECT provider, key_id, ciphertext FROM provider_credentials
		  WHERE workspace_id = $1 AND id = $2`, workspaceID, id).Scan(&vendor, &keyID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("select provider credential: %w", err)
	}
	plain, err := c.keys.Open(crypto.Sealed{KeyID: crypto.KeyID(keyID), Ciphertext: blob},
		credentialAAD(workspaceID, id, vendor))
	if err != nil {
		return "", fmt.Errorf("open provider credential %s: %w", id, err)
	}
	return string(plain), nil
}
