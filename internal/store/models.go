package store

import (
	"errors"
	"net/netip"
	"time"
)

// Sentinel errors the handlers translate into HTTP status codes. Callers
// compare with errors.Is rather than inspecting driver errors.
var (
	// ErrNotFound means no row matched, including the case where a row exists
	// but belongs to another workspace. Keeping those two indistinguishable is
	// deliberate: a cross-tenant probe must not be able to tell them apart.
	ErrNotFound = errors.New("store: not found")

	// ErrEmailTaken is returned when the unique index on users.email rejects
	// an insert.
	ErrEmailTaken = errors.New("store: email already registered")
)

// User is a sign-in identity. PasswordHash is the full argon2id encoded
// string and is never serialised to a client.
type User struct {
	ID              string
	Email           string
	PasswordHash    string
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EmailVerified reports whether the address has been confirmed.
func (u *User) EmailVerified() bool { return u.EmailVerifiedAt != nil }

// Workspace is the tenant boundary. Every tenant row carries its id.
type Workspace struct {
	ID        string
	Name      string
	OwnerID   string
	CreatedAt time.Time
}

// Session is one signed-in browser session. The token itself is never stored,
// only its SHA-256.
type Session struct {
	ID          string
	UserID      string
	WorkspaceID string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// Identity is everything a request needs about its caller, resolved in one
// query so authenticating a request costs a single round trip.
type Identity struct {
	Session         Session
	Email           string
	EmailVerifiedAt *time.Time
	WorkspaceName   string
}

// EmailVerified reports whether the caller has confirmed their address.
func (i *Identity) EmailVerified() bool { return i.EmailVerifiedAt != nil }

// SessionInput carries the fields recorded when a session is created. The
// address is optional because a request may arrive without a usable one.
type SessionInput struct {
	TokenHash   []byte
	UserID      string
	WorkspaceID string
	ExpiresAt   time.Time
	UserAgent   string
	IP          *netip.Addr
}

// ErrLimitReached means the workspace already holds the maximum number of
// provider credentials.
var ErrLimitReached = errors.New("store: credential limit reached")
