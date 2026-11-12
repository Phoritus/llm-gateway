package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	keySize     = 32
	keyIDBytes  = 8
	keyIDDomain = "llm-gateway/crypto key id v1"
)

var (
	// ErrUnknownKey means the value was sealed under a key this keyring does not hold.
	ErrUnknownKey = errors.New("crypto: unknown key id")
	// ErrOpen means authentication failed or the ciphertext is malformed. It
	// says no more, so it cannot leak anything about why.
	ErrOpen = errors.New("crypto: cannot open sealed value")
)

// KeyID names the master key a value was sealed under. It is a truncated HMAC
// of the key, so it needs no config and reveals nothing usable about the key.
type KeyID string

// Sealed is what the store persists: two columns, so the rotation job can
// query rows WHERE key_id <> current.
type Sealed struct {
	KeyID      KeyID
	Ciphertext []byte // nonce (12 bytes) || GCM ciphertext+tag
}

// Keyring holds the current master key and any older ones still needed to open
// old values.
type Keyring struct {
	current KeyID
	aeads   map[KeyID]cipher.AEAD
}

// NewKeyring takes the current 32-byte master key and any older keys still
// needed to open old values. It rejects a key of the wrong length and the same
// key given twice.
func NewKeyring(current []byte, older ...[]byte) (*Keyring, error) {
	k := &Keyring{aeads: make(map[KeyID]cipher.AEAD, 1+len(older))}
	for i, key := range append([][]byte{current}, older...) {
		id, aead, err := newAEAD(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: key %d: %w", i, err)
		}
		if _, dup := k.aeads[id]; dup {
			return nil, fmt.Errorf("crypto: key %d duplicates an earlier key (id %s)", i, id)
		}
		k.aeads[id] = aead
		if i == 0 {
			k.current = id
		}
	}
	return k, nil
}

func newAEAD(key []byte) (KeyID, cipher.AEAD, error) {
	if len(key) != keySize {
		return "", nil, fmt.Errorf("master key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", nil, errors.New("invalid master key")
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return "", nil, errors.New("invalid master key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(keyIDDomain))
	return KeyID(hex.EncodeToString(mac.Sum(nil)[:keyIDBytes])), aead, nil
}

// Current reports the id new values are sealed under.
func (k *Keyring) Current() KeyID { return k.current }

// Seal encrypts under the current key with a fresh random nonce. aad binds the
// value to its context, so a ciphertext copied to another row will not open.
func (k *Keyring) Seal(plaintext, aad []byte) Sealed {
	return Sealed{KeyID: k.current, Ciphertext: k.aeads[k.current].Seal(nil, nil, plaintext, aad)}
}

// Open picks the key by s.KeyID. A rewritten id selects a different key, so
// GCM rejects the value without the id needing to be authenticated itself.
func (k *Keyring) Open(s Sealed, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[s.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	pt, err := aead.Open(nil, nil, s.Ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
