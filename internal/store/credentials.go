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
	BaseURL     string
	CreatedAt   time.Time
	LastCheck   *KeyCheck
}

type KeyCheck struct {
	Result KeyCheckResult
	At     time.Time
}

type KeyCheckResult string

const (
	KeyCheckOK                  KeyCheckResult = "ok"
	KeyCheckRejected            KeyCheckResult = "key_rejected"
	KeyCheckQuotaExhausted      KeyCheckResult = "quota_exhausted"
	KeyCheckRateLimited         KeyCheckResult = "rate_limited"
	KeyCheckProviderTimeout     KeyCheckResult = "provider_timeout"
	KeyCheckProviderError       KeyCheckResult = "provider_error"
	KeyCheckProviderUnavailable KeyCheckResult = "provider_unavailable"
)

type NewCredential struct {
	Provider provider.Vendor
	Name     string
	Secret   string
	BaseURL  string
}

func credentialAAD(workspaceID, id uuid.UUID, vendor provider.Vendor, baseURL string) []byte {
	if vendor == provider.VendorCustom {
		return customCredentialAAD(workspaceID, id, baseURL)
	}
	return vendorCredentialAAD(workspaceID, id, vendor)
}

func vendorCredentialAAD(workspaceID, id uuid.UUID, vendor provider.Vendor) []byte {
	aad := append([]byte("llm-gateway/provider-credential v1\x00"), workspaceID[:]...)
	aad = append(aad, id[:]...)
	return append(aad, vendor...)
}

func customCredentialAAD(workspaceID, id uuid.UUID, baseURL string) []byte {
	return append(append(vendorCredentialAAD(workspaceID, id, provider.VendorCustom), 0), baseURL...)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
	sealed := c.keys.Seal([]byte(in.Secret), credentialAAD(ws, id, in.Provider, in.BaseURL))
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
		`INSERT INTO provider_credentials (id, workspace_id, provider, name, key_id, ciphertext, last4, base_url)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $9
		  WHERE (SELECT count(*) FROM provider_credentials WHERE workspace_id = $2) < $8
		 RETURNING id, workspace_id, provider, name, last4, coalesce(base_url, ''), created_at`,
		id.String(), workspaceID, string(in.Provider), in.Name, string(sealed.KeyID), sealed.Ciphertext, suffix,
		maxCredentialsPerWorkspace, nullable(in.BaseURL),
	).Scan(&out.ID, &out.WorkspaceID, &out.Provider, &out.Name, &out.Last4, &out.BaseURL, &out.CreatedAt)
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
		`SELECT id, workspace_id, provider, name, last4, coalesce(base_url, ''), created_at, check_result, checked_at
		   FROM provider_credentials
		  WHERE workspace_id = $1
		  ORDER BY created_at DESC, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list provider credentials: %w", err)
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var (
			cred      Credential
			result    *string
			checkedAt *time.Time
		)
		if err := rows.Scan(&cred.ID, &cred.WorkspaceID, &cred.Provider, &cred.Name, &cred.Last4, &cred.BaseURL, &cred.CreatedAt,
			&result, &checkedAt); err != nil {
			return nil, fmt.Errorf("scan provider credential: %w", err)
		}
		if result != nil && checkedAt != nil {
			cred.LastCheck = &KeyCheck{Result: KeyCheckResult(*result), At: *checkedAt}
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
	Vendor  provider.Vendor
	Secret  string
	BaseURL string
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
		`SELECT id, provider, coalesce(base_url, ''), key_id, ciphertext FROM provider_credentials
		  WHERE workspace_id = $1 AND id = $2`, workspaceID, rowID).
		Scan(&sealed.id, &sealed.vendor, &sealed.baseURL, &sealed.keyID, &sealed.blob)
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
	return UnsealedCredential{Vendor: sealed.vendor, Secret: secret, BaseURL: sealed.baseURL}, nil
}

// ResolvedRoute is a route as /v1 calls it: its chain in order, each target
// carrying the opened key it authenticates upstream with.
type ResolvedRoute struct {
	ID             string
	MaxHops        int
	FreeFirst      bool
	Cache          CacheMode
	ContextHandoff HandoffMode
	// Version changes whenever the route is written. The exact cache keys on
	// it, so an answer from the chain as it was is not served for the chain
	// as it is.
	Version  int64
	Settings WorkspaceSettings
	Targets  []ResolvedTarget
}

type ResolvedTarget struct {
	CredentialID string
	Vendor       provider.Vendor
	Model        string
	Secret       string
	BaseURL      string
	// FirstByteTimeout is how long the target may take to its first chunk.
	// Zero is router.DefaultFirstByteTimeout.
	FirstByteTimeout time.Duration
}

// ResolveRoute finds the workspace's route for an alias and opens every
// target's key, or returns ErrNotFound when the workspace has no such alias.
func (c *Credentials) ResolveRoute(ctx context.Context, workspaceID, alias string) (*ResolvedRoute, error) {
	ws, err := uuid.Parse(workspaceID)
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := c.store.pool.Query(ctx,
		`SELECT r.id, r.max_hops, r.free_first, r.cache, r.context_handoff, r.updated_at, w.settings, c.id, c.provider, coalesce(c.base_url, ''), t.model, t.first_byte_timeout_seconds, c.key_id, c.ciphertext
		   FROM routes r
		   JOIN workspaces w ON w.id = r.workspace_id
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
			wait    *int32
			updated time.Time
			raw     []byte
		)
		if err := rows.Scan(&route.ID, &route.MaxHops, &route.FreeFirst, &route.Cache, &route.ContextHandoff, &updated, &raw, &sealed.id, &sealed.vendor, &sealed.baseURL, &model, &wait, &sealed.keyID, &sealed.blob); err != nil {
			return nil, fmt.Errorf("scan route target: %w", err)
		}
		route.Version = updated.UnixNano()
		if route.Settings, err = parseSettings(raw); err != nil {
			return nil, err
		}
		secret, err := c.open(ws, sealed)
		if err != nil {
			return nil, err
		}
		route.Targets = append(route.Targets, ResolvedTarget{
			CredentialID: sealed.id.String(), Vendor: sealed.vendor, BaseURL: sealed.baseURL, Model: model, Secret: secret,
			FirstByteTimeout: seconds(wait),
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
	id      uuid.UUID
	vendor  provider.Vendor
	baseURL string
	keyID   string
	blob    []byte
}

func (c *Credentials) open(ws uuid.UUID, s sealedCredential) (string, error) {
	plain, err := c.keys.Open(crypto.Sealed{KeyID: crypto.KeyID(s.keyID), Ciphertext: s.blob},
		credentialAAD(ws, s.id, s.vendor, s.baseURL))
	if err != nil {
		return "", fmt.Errorf("open provider credential %s: %w", s.id, err)
	}
	return string(plain), nil
}

// Reseal re-seals, under the keyring's current key, every credential sealed
// under an older one, batch rows at a time, each row in a transaction of its
// own. Stopped part-way, it is run again: it picks up what is left. It returns
// how many rows it re-sealed. A row it cannot open stops it with the error, so
// that a key missing from the keyring is noticed rather than skipped.
func (c *Credentials) Reseal(ctx context.Context, batch int) (int, error) {
	current := string(c.keys.Current())
	resealed := 0
	for {
		rows, err := c.store.pool.Query(ctx,
			`SELECT id FROM provider_credentials WHERE key_id <> $1 ORDER BY id LIMIT $2`, current, batch)
		if err != nil {
			return resealed, fmt.Errorf("list credentials to re-seal: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return resealed, fmt.Errorf("list credentials to re-seal: %w", err)
		}
		if len(ids) == 0 {
			return resealed, nil
		}
		for _, id := range ids {
			done, err := c.resealOne(ctx, id, current)
			if err != nil {
				return resealed, err
			}
			if done {
				resealed++
			}
		}
	}
}

func (c *Credentials) resealOne(ctx context.Context, id uuid.UUID, current string) (resealedHere bool, err error) {
	tx, err := c.store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin re-seal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		ws     uuid.UUID
		sealed = sealedCredential{id: id}
	)
	err = tx.QueryRow(ctx,
		`SELECT workspace_id, provider, coalesce(base_url, ''), key_id, ciphertext FROM provider_credentials
		  WHERE id = $1 AND key_id <> $2 FOR UPDATE`, id, current).
		Scan(&ws, &sealed.vendor, &sealed.baseURL, &sealed.keyID, &sealed.blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select credential %s to re-seal: %w", id, err)
	}
	secret, err := c.open(ws, sealed)
	if err != nil {
		return false, err
	}
	resealed := c.keys.Seal([]byte(secret), credentialAAD(ws, id, sealed.vendor, sealed.baseURL))
	if _, err := tx.Exec(ctx,
		`UPDATE provider_credentials SET key_id = $2, ciphertext = $3 WHERE id = $1`,
		id, string(resealed.KeyID), resealed.Ciphertext); err != nil {
		return false, fmt.Errorf("re-seal credential %s: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit re-seal of %s: %w", id, err)
	}
	return true, nil
}

func (c *Credentials) KeyUsage(ctx context.Context) (map[crypto.KeyID]int, error) {
	rows, err := c.store.pool.Query(ctx, `SELECT key_id, count(*) FROM provider_credentials GROUP BY key_id`)
	if err != nil {
		return nil, fmt.Errorf("count credentials by key: %w", err)
	}
	defer rows.Close()
	usage := map[crypto.KeyID]int{}
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("count credentials by key: %w", err)
		}
		usage[crypto.KeyID(id)] = n
	}
	return usage, rows.Err()
}

type DueCheck struct {
	WorkspaceID string
	ID          string
}

func (c *Credentials) ClaimDueChecks(ctx context.Context, interval time.Duration, limit int) ([]DueCheck, error) {
	rows, err := c.store.pool.Query(ctx,
		`UPDATE provider_credentials SET check_due_at = now() + $1 * interval '1 second'
		  WHERE id IN (SELECT id FROM provider_credentials WHERE check_due_at <= now()
		                ORDER BY check_due_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		 RETURNING workspace_id, id`, int64(interval/time.Second), limit)
	if err != nil {
		return nil, fmt.Errorf("claim due key checks: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DueCheck])
}

func (c *Credentials) RecordCheck(ctx context.Context, workspaceID, id string, check KeyCheck) error {
	id, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := c.store.pool.Exec(ctx,
		`UPDATE provider_credentials SET check_result = $3, checked_at = $4 WHERE workspace_id = $1 AND id = $2`,
		workspaceID, id, string(check.Result), check.At)
	if err != nil {
		return fmt.Errorf("record key check: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
