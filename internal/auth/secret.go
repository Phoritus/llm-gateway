package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// secretBytes is the length of every bearer-style secret this package mints:
// session tokens, verification tokens and reset tokens. 256 bits of entropy
// puts guessing out of reach, so these never need a rate limit of their own.
const secretBytes = 32

// secretEncodedLen is the length of a secret in unpadded base64.
const secretEncodedLen = (secretBytes*8 + 5) / 6

// newSecret returns a URL-safe secret and the SHA-256 of its exact string
// form. Only the hash is ever persisted, so a database leak yields nothing
// that can be replayed.
//
// SHA-256 with no salt or stretching is correct here and wrong for passwords:
// the input is already full-entropy random, so there is nothing to brute
// force, and a fast hash keeps per-request lookups cheap.
func newSecret() (token string, hash []byte, err error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate secret: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashSecret(token), nil
}

// hashSecret hashes a presented secret so it can be compared with a stored
// hash. Lookup by hash is itself constant-time with respect to the secret:
// the database index is searched for a value the attacker cannot steer.
func hashSecret(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
