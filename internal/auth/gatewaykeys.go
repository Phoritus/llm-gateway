package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"llm-gateway/internal/store"
)

const (
	GatewayKeyPrefix = "gwk_"

	gatewayKeyLen = len(GatewayKeyPrefix) + secretEncodedLen

	gatewayKeyShownLen = len(GatewayKeyPrefix) + 8

	gatewayKeyCacheTTL = time.Second

	lastUsedFlushInterval = time.Minute
)

// ErrInvalidGatewayKey means the presented key is malformed, unknown or
// revoked.
var ErrInvalidGatewayKey = errors.New("auth: gateway key is invalid or revoked")

type cachedGatewayKey struct {
	key     store.ActiveGatewayKey
	expires time.Time
}

// GatewayKeys mints gateway keys and authenticates them for /v1 through a
// short-lived in-process cache. It is safe for concurrent use.
type GatewayKeys struct {
	store *store.Store

	mu              sync.Mutex
	byHash          map[string]cachedGatewayKey
	lastUsedByID    map[string]time.Time
	revocationEpoch uint64
}

func NewGatewayKeys(st *store.Store) *GatewayKeys {
	return &GatewayKeys{
		store:        st,
		byHash:       make(map[string]cachedGatewayKey),
		lastUsedByID: make(map[string]time.Time),
	}
}

// Mint creates a key, with a monthly budget or nil for none, and returns it
// in full.
func (g *GatewayKeys) Mint(ctx context.Context, workspaceID, name string, budget *store.Budget) (string, *store.GatewayKey, error) {
	secret, _, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	token := GatewayKeyPrefix + secret
	key, err := g.store.CreateGatewayKey(ctx, workspaceID, store.NewGatewayKey{
		Name:   name,
		Prefix: token[:gatewayKeyShownLen],
		Hash:   hashSecret(token),
		Budget: budget,
	})
	if err != nil {
		return "", nil, err
	}
	return token, key, nil
}

// Revoke revokes a key and evicts it from this instance's cache, so it stops
// working here at once and on other instances within gatewayKeyCacheTTL.
func (g *GatewayKeys) Revoke(ctx context.Context, workspaceID, id string) error {
	hash, err := g.store.RevokeGatewayKey(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	g.mu.Lock()
	delete(g.byHash, string(hash))
	g.revocationEpoch++
	g.mu.Unlock()
	return nil
}

// Authenticate resolves a presented bearer key to its key and workspace.
func (g *GatewayKeys) Authenticate(ctx context.Context, token string) (store.ActiveGatewayKey, error) {
	if len(token) != gatewayKeyLen || !strings.HasPrefix(token, GatewayKeyPrefix) {
		return store.ActiveGatewayKey{}, ErrInvalidGatewayKey
	}
	hash := string(hashSecret(token))
	now := time.Now()

	g.mu.Lock()
	entry, ok := g.byHash[hash]
	if ok && !now.After(entry.expires) {
		g.lastUsedByID[entry.key.ID] = now
		g.mu.Unlock()
		return entry.key, nil
	}
	epoch := g.revocationEpoch
	g.mu.Unlock()

	key, err := g.store.ActiveGatewayKeyByHash(ctx, []byte(hash))
	if errors.Is(err, store.ErrNotFound) {
		return store.ActiveGatewayKey{}, ErrInvalidGatewayKey
	}
	if err != nil {
		return store.ActiveGatewayKey{}, err
	}

	g.mu.Lock()
	if g.revocationEpoch == epoch {
		g.byHash[hash] = cachedGatewayKey{key: *key, expires: now.Add(gatewayKeyCacheTTL)}
	}
	g.lastUsedByID[key.ID] = now
	g.mu.Unlock()
	return *key, nil
}

// RunLastUsedFlusher writes recorded uses to Postgres until ctx ends, then
// flushes once more so a clean shutdown loses none.
func (g *GatewayKeys) RunLastUsedFlusher(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(lastUsedFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			g.flushLogged(context.Background(), logger)
			return
		case <-ticker.C:
			g.flushLogged(ctx, logger)
		}
	}
}

func (g *GatewayKeys) flushLogged(ctx context.Context, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := g.Flush(ctx); err != nil {
		logger.ErrorContext(ctx, "flush gateway key last_used", slog.String("error", err.Error()))
	}
}

// Flush writes the uses recorded since the last flush and drops expired cache
// entries. A failed write puts its uses back for the next attempt.
func (g *GatewayKeys) Flush(ctx context.Context) error {
	now := time.Now()
	g.mu.Lock()
	pending := g.lastUsedByID
	g.lastUsedByID = make(map[string]time.Time)
	for hash, entry := range g.byHash {
		if now.After(entry.expires) {
			delete(g.byHash, hash)
		}
	}
	g.mu.Unlock()

	err := g.store.TouchGatewayKeys(ctx, pending)
	if err != nil {
		g.mu.Lock()
		for id, at := range pending {
			if at.After(g.lastUsedByID[id]) {
				g.lastUsedByID[id] = at
			}
		}
		g.mu.Unlock()
	}
	return err
}
