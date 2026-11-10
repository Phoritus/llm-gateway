package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the cost of one login, so they are a
// deliberate trade: 64 MiB and three passes take tens of milliseconds on the
// free tiers this runs on while making offline cracking of a leaked hash
// expensive. They are recorded in every hash, so raising them later keeps old
// hashes verifiable.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Password length bounds. The minimum follows current guidance that length
// beats composition rules. The maximum exists because the hash input is
// attacker-controlled and argon2 cost grows with it, so an unbounded password
// is a cheap way to burn server CPU.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 1024
)

// Errors returned when a candidate password is unusable. Handlers surface
// these to the client, since the user needs to know what to fix.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLen)
	ErrPasswordCommon   = errors.New("password is too common; choose something less guessable")
	ErrPasswordMismatch = errors.New("password does not match")
	ErrInvalidHash      = errors.New("stored password hash is malformed")
)

// commonPasswords holds the handful of passwords that dominate credential
// stuffing lists and still satisfy a 12-character minimum. This is a cheap
// floor, not a substitute for a full breach-corpus check.
var commonPasswords = map[string]struct{}{
	"password":        {},
	"password1":       {},
	"password123":     {},
	"password1234":    {},
	"passw0rd123":     {},
	"123456789012":    {},
	"1234567890":      {},
	"qwertyuiop":      {},
	"qwerty123456":    {},
	"letmein123":      {},
	"iloveyou123":     {},
	"administrator":   {},
	"welcome123456":   {},
	"changeme123":     {},
	"correcthorse":    {},
	"football123":     {},
	"baseball123":     {},
	"dragon123456":    {},
	"monkey123456":    {},
	"abc123456789":    {},
	"trustno1234":     {},
	"iloveyouuu":      {},
	"sunshine1234":    {},
	"princess1234":    {},
	"qazwsxedcrfv":    {},
	"asdfghjkl123":    {},
	"zxcvbnm12345":    {},
	"111111111111":    {},
	"000000000000":    {},
	"loginpassword":   {},
	"gatewaypassword": {},
}

// IsPasswordRejected reports whether err is one of the password-rule failures
// above. Those messages are written for the user and can be shown as they are,
// which is what lets a handler translate the whole class without listing its
// members or deciding the rules itself.
func IsPasswordRejected(err error) bool {
	return errors.Is(err, ErrPasswordTooShort) ||
		errors.Is(err, ErrPasswordTooLong) ||
		errors.Is(err, ErrPasswordCommon)
}

// ValidatePassword checks a candidate against the rules. HashPassword calls it,
// so no caller has to, and the exported form exists for a client that wants to
// check a candidate without hashing it.
func ValidatePassword(plain string) error {
	// Count runes, not bytes: a short non-ASCII password should not pass a
	// byte-length check.
	if utf8.RuneCountInString(plain) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if len(plain) > MaxPasswordLen {
		return ErrPasswordTooLong
	}
	if _, bad := commonPasswords[strings.ToLower(strings.TrimSpace(plain))]; bad {
		return ErrPasswordCommon
	}
	return nil
}

// HashPassword validates the candidate and derives an argon2id hash with a
// fresh random salt, returned in the standard encoded form, which carries the
// parameters and salt alongside the digest.
//
// The rules are enforced here rather than by each caller because hashing is the
// only door to a stored password: signup, reset and change all pass through it,
// so a path that forgets to validate is not a path that can exist. It also
// settles the ordering question -- a caller that validates separately decides,
// by where it puts the call, which failure a client is told about first.
func HashPassword(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	digest := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword checks a candidate against an encoded hash. It re-derives
// with the parameters stored in the hash, so hashes written under older
// settings keep working, and compares in constant time.
func VerifyPassword(encodedHash, plain string) error {
	if len(plain) > MaxPasswordLen {
		return ErrPasswordMismatch
	}
	params, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(plain), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// dummyHash is verified against when no user matched the submitted address.
// Without it a missing account would answer far faster than a wrong password,
// which turns the login endpoint into an account-existence oracle.
var dummyHash = func() string {
	h, err := HashPassword("this value is never a real password, it only burns the same CPU")
	if err != nil {
		// rand.Read failing at init means the process has no usable entropy
		// source, and nothing security-related can proceed.
		panic("auth: cannot build dummy password hash: " + err.Error())
	}
	return h
}()

// VerifyPasswordDummy spends the same work VerifyPassword would, for the case
// where no account exists. It always reports a mismatch.
func VerifyPasswordDummy(plain string) error {
	_ = VerifyPassword(dummyHash, plain)
	return ErrPasswordMismatch
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, digest
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, ErrInvalidHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	return p, salt, digest, nil
}
