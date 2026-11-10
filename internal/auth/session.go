package auth

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"llm-gateway/internal/store"
)

// Errors a caller must distinguish. Everything else is an internal failure.
var (
	// ErrNoSession means no usable session was presented: absent, unknown,
	// revoked, or past either expiry. Handlers answer all of these with one
	// 401 so a probe learns nothing about which it was.
	ErrNoSession = errors.New("auth: no valid session")
)

// touchInterval bounds how often an active session writes to Postgres to
// refresh its idle clock. Without it every authenticated request would cost a
// write.
const touchInterval = time.Minute

// Config is everything the auth service needs from the environment.
//
// Every field is required and none is defaulted here. internal/config is the
// one place a lifetime or a policy value is decided, and it validates all of
// them before this struct is built; a second layer of defaults in this package
// would be dead in production and would silently disagree with the loader
// wherever it was not.
type Config struct {
	// SessionTTL is the absolute lifetime of a session, counted from login.
	SessionTTL time.Duration
	// SessionIdleTTL ends a session that has gone unused for this long, even
	// while the absolute lifetime has not run out.
	SessionIdleTTL time.Duration
	// Secure sets the cookie Secure attribute. Production only, because a
	// Secure cookie is not sent over plain http://localhost.
	Secure bool
	// CookieDomain scopes the cookies to a parent domain. Left empty, the
	// cookies are host-only, which is the stricter choice.
	CookieDomain string
	// SessionSecret keys the CSRF token HMAC.
	SessionSecret []byte
	// TrustedProxies are the public proxy ranges whose X-Forwarded-For header
	// may be believed, on top of the loopback and private ranges that are
	// always treated as proxies. Empty means the TCP peer is the client and
	// the header is ignored, which is the right answer for a service reached
	// directly.
	TrustedProxies []netip.Prefix
	// VerificationTTL and ResetTTL bound the two email-delivered tokens. A
	// reset token is short-lived because it is a password bypass; a
	// verification token is only a proof of address.
	VerificationTTL time.Duration
	ResetTTL        time.Duration
}

// Service owns sessions, password verification and the email-delivered
// tokens. It is safe for concurrent use.
type Service struct {
	store *store.Store
	cfg   Config
}

// NewService builds the auth service from an already-validated Config.
func NewService(st *store.Store, cfg Config) *Service {
	return &Service{store: st, cfg: cfg}
}

// CreateSession mints a session for a user in one workspace and returns the
// raw token, which is the only time it exists outside the client.
func (s *Service) CreateSession(ctx context.Context, userID, workspaceID string, r *http.Request) (string, *store.Session, error) {
	token, hash, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	// The exact address, not the rate-limit bucket: this is an audit field the
	// user will read back in their own session list.
	var ip *netip.Addr
	if addr, ok := s.ClientIP(r); ok {
		ip = &addr
	}

	sess, err := s.store.CreateSession(ctx, store.SessionInput{
		TokenHash:   hash,
		UserID:      userID,
		WorkspaceID: workspaceID,
		ExpiresAt:   time.Now().Add(s.cfg.SessionTTL),
		UserAgent:   truncate(r.UserAgent(), 512),
		IP:          ip,
	})
	if err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// ResolveSession authenticates a presented token. It applies the idle timeout
// here rather than in SQL because the timeout is policy, and refreshes
// last_seen_at at most once per touchInterval.
func (s *Service) ResolveSession(ctx context.Context, token string) (*store.Identity, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	identity, err := s.store.IdentityByTokenHash(ctx, hashSecret(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}

	now := time.Now()
	if now.Sub(identity.Session.LastSeenAt) > s.cfg.SessionIdleTTL {
		// Abandoned session: revoke it rather than leaving a token that would
		// start working again if the clock or the policy moved.
		if err := s.store.RevokeSession(ctx, identity.Session.ID); err != nil {
			return nil, err
		}
		return nil, ErrNoSession
	}

	if now.Sub(identity.Session.LastSeenAt) > touchInterval {
		if err := s.store.TouchSession(ctx, identity.Session.ID); err != nil {
			return nil, err
		}
	}
	return identity, nil
}

// RevokeSession ends the session behind a presented token. An unknown token is
// not an error: logout is idempotent, and reporting "no such session" would
// leak whether the token was ever valid.
func (s *Service) RevokeSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	identity, err := s.store.IdentityByTokenHash(ctx, hashSecret(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.store.RevokeSession(ctx, identity.Session.ID)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
