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

	if err := lockWorkspaceForCap(ctx, tx, workspaceID); err != nil {
		return nil, err
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

// Delete removes a credential. It returns ErrNotFound when the workspace has
// no such one, and ErrCredentialInUse while a route still names it.
func (c *Credentials) Delete(ctx context.Context, workspaceID, id string) error {
	id, err := parseID(id)
	if err != nil {
		return err
	}
	tx, err := c.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete provider credential: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockWorkspaceForCap(ctx, tx, workspaceID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM provider_credentials WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete provider credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	err = tx.Commit(ctx)
	if pgCode(err) == foreignKeyViolation {
		return ErrCredentialInUse
	}
	if err != nil {
		return fmt.Errorf("commit delete provider credential: %w", err)
	}
	return nil
}

type UnsealedCredential struct {
	Vendor provider.Vendor
	Secret string
}

// Unseal opens a credential's key. The error never carries the plaintext.
func (c *Credentials) Unseal(ctx context.Context, workspaceID, id string) (UnsealedCredential, error) {
	ws, err := uuid.Parse(workspaceID)
	if err != nil {
		return UnsealedCredential{}, ErrNotFound
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return UnsealedCredential{}, ErrNotFound
	}
	var sealed sealedCredential
	err = c.store.pool.QueryRow(ctx,
		`SELECT id, provider, key_id, ciphertext FROM provider_credentials
		  WHERE workspace_id = $1 AND id = $2`, workspaceID, rowID).
		Scan(&sealed.id, &sealed.vendor, &sealed.keyID, &sealed.blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnsealedCredential{}, ErrNotFound
	}
	if err != nil {
		return UnsealedCredential{}, fmt.Errorf("select provider credential: %w", err)
	}
	secret, err := c.open(ws, sealed)
	if err != nil {
		return UnsealedCredential{}, err
	}
	return UnsealedCredential{Vendor: sealed.vendor, Secret: secret}, nil
}

// ResolvedRoute is a route as /v1 calls it: its chain in order, each target
// carrying the opened key it authenticates upstream with.
type ResolvedRoute struct {
	ID        string
	MaxHops   int
	FreeFirst bool
	Cache     CacheMode
	// Version changes whenever the route is written. The exact cache keys on
	// it, so an answer from the chain as it was is not served for the chain
	// as it is.
	Version int64
	Targets []ResolvedTarget
}

type ResolvedTarget struct {
	CredentialID string
	Vendor       provider.Vendor
	Model        string
	Secret       string
}

// ResolveRoute finds the workspace's route for an alias and opens every
// target's key, or returns ErrNotFound when the workspace has no such alias.
func (c *Credentials) ResolveRoute(ctx context.Context, workspaceID, alias string) (*ResolvedRoute, error) {
	ws, err := uuid.Parse(workspaceID)
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := c.store.pool.Query(ctx,
		`SELECT r.id, r.max_hops, r.free_first, r.cache, r.updated_at, c.id, c.provider, t.model, c.key_id, c.ciphertext
		   FROM routes r
		   JOIN route_targets t ON t.route_id = r.id AND t.workspace_id = r.workspace_id
		   JOIN provider_credentials c ON c.id = t.credential_id AND c.workspace_id = t.workspace_id
		  WHERE r.workspace_id = $1 AND r.alias = $2
		  ORDER BY t.position`, workspaceID, alias)
	if err != nil {
		return nil, fmt.Errorf("resolve route: %w", err)
	}
	defer rows.Close()

	var route ResolvedRoute
	for rows.Next() {
		var (
			sealed  sealedCredential
			model   string
			updated time.Time
		)
		if err := rows.Scan(&route.ID, &route.MaxHops, &route.FreeFirst, &route.Cache, &updated, &sealed.id, &sealed.vendor, &model, &sealed.keyID, &sealed.blob); err != nil {
			return nil, fmt.Errorf("scan route target: %w", err)
		}
		route.Version = updated.UnixNano()
		secret, err := c.open(ws, sealed)
		if err != nil {
			return nil, err
		}
		route.Targets = append(route.Targets, ResolvedTarget{
			CredentialID: sealed.id.String(), Vendor: sealed.vendor, Model: model, Secret: secret,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve route: %w", err)
	}
	if len(route.Targets) == 0 {
		return nil, ErrNotFound
	}
	return &route, nil
}

type sealedCredential struct {
	id     uuid.UUID
	vendor provider.Vendor
	keyID  string
	blob   []byte
}

func (c *Credentials) open(ws uuid.UUID, s sealedCredential) (string, error) {
	plain, err := c.keys.Open(crypto.Sealed{KeyID: crypto.KeyID(s.keyID), Ciphertext: s.blob},
		credentialAAD(ws, s.id, s.vendor))
	if err != nil {
		return "", fmt.Errorf("open provider credential %s: %w", s.id, err)
	}
	return string(plain), nil
}
