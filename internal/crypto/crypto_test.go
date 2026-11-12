package crypto

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func mustKeyring(t *testing.T, current []byte, older ...[]byte) *Keyring {
	t.Helper()
	k, err := NewKeyring(current, older...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustSeal(t *testing.T, k *Keyring, pt, aad []byte) Sealed {
	t.Helper()
	s, err := k.Seal(pt, aad)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	k := mustKeyring(t, key(1))
	for _, tc := range []struct {
		name    string
		pt, aad []byte
	}{
		{"plain", []byte("sk-secret"), []byte("ws1/cred1")},
		{"empty plaintext", []byte{}, []byte("ctx")},
		{"nil plaintext", nil, []byte("ctx")},
		{"nil aad", []byte("sk-secret"), nil},
		{"binary", []byte{0, 255, 1, 2}, []byte{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := k.Open(mustSeal(t, k, tc.pt, tc.aad), tc.aad)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.pt) {
				t.Fatalf("got %q, want %q", got, tc.pt)
			}
		})
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	k := mustKeyring(t, key(1))
	aad := []byte("ctx")
	base := mustSeal(t, k, []byte("sk-secret"), aad)
	last := len(base.Ciphertext) - 1

	for _, tc := range []struct {
		name   string
		mutate func(ct []byte) []byte
	}{
		{"nonce byte", func(ct []byte) []byte { ct[0] ^= 1; return ct }},
		{"body byte", func(ct []byte) []byte { ct[12] ^= 1; return ct }},
		{"tag byte", func(ct []byte) []byte { ct[last] ^= 1; return ct }},
		{"appended byte", func(ct []byte) []byte { return append(ct, 0) }},
		{"shorter than nonce", func(ct []byte) []byte { return ct[:5] }},
		{"shorter than nonce+tag", func(ct []byte) []byte { return ct[:12+15] }},
		{"empty", func(ct []byte) []byte { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Sealed{KeyID: base.KeyID, Ciphertext: tc.mutate(bytes.Clone(base.Ciphertext))}
			if _, err := k.Open(s, aad); !errors.Is(err, ErrOpen) {
				t.Fatalf("err = %v, want ErrOpen", err)
			}
		})
	}
}

func TestWrongKey(t *testing.T) {
	a := mustKeyring(t, key(1))
	b := mustKeyring(t, key(2))
	s := mustSeal(t, a, []byte("sk-secret"), nil)

	if _, err := b.Open(s, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("different keyring: err = %v, want ErrUnknownKey", err)
	}
	s.KeyID = b.Current()
	if _, err := b.Open(s, nil); !errors.Is(err, ErrOpen) {
		t.Fatalf("rewritten key id: err = %v, want ErrOpen", err)
	}
}

func TestRewrittenKeyIDWithinKeyringFails(t *testing.T) {
	k := mustKeyring(t, key(1), key(2))
	s := mustSeal(t, k, []byte("sk-secret"), nil)
	s.KeyID = mustKeyring(t, key(2)).Current()
	if _, err := k.Open(s, nil); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestWrongAAD(t *testing.T) {
	k := mustKeyring(t, key(1))
	s := mustSeal(t, k, []byte("sk-secret"), []byte("ws1/cred1"))
	for name, aad := range map[string][]byte{
		"other context": []byte("ws2/cred1"),
		"nil":           nil,
		"prefix":        []byte("ws1/cred"),
	} {
		if _, err := k.Open(s, aad); !errors.Is(err, ErrOpen) {
			t.Fatalf("%s: err = %v, want ErrOpen", name, err)
		}
	}
}

func TestRotation(t *testing.T) {
	oldRing := mustKeyring(t, key(1))
	aad := []byte("ctx")
	sealedOld := mustSeal(t, oldRing, []byte("sk-secret"), aad)

	ring := mustKeyring(t, key(2), key(1))
	if ring.Current() == oldRing.Current() {
		t.Fatal("current id did not change")
	}
	got, err := ring.Open(sealedOld, aad)
	if err != nil || string(got) != "sk-secret" {
		t.Fatalf("open old value: %q, %v", got, err)
	}
	if s := mustSeal(t, ring, []byte("x"), aad); s.KeyID != ring.Current() {
		t.Fatalf("new seal under %s, want %s", s.KeyID, ring.Current())
	}
}

func TestNonceUniqueness(t *testing.T) {
	k := mustKeyring(t, key(1))
	nonces := map[string]bool{}
	cts := map[string]bool{}
	const n = 1000
	for range n {
		s := mustSeal(t, k, []byte("same"), nil)
		nonces[string(s.Ciphertext[:12])] = true
		cts[string(s.Ciphertext)] = true
	}
	if len(nonces) != n || len(cts) != n {
		t.Fatalf("distinct nonces %d, ciphertexts %d, want %d", len(nonces), len(cts), n)
	}
}

func TestKeyID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{16}$`)
	a1 := mustKeyring(t, key(1)).Current()
	a2 := mustKeyring(t, key(1)).Current()
	b := mustKeyring(t, key(2)).Current()
	if !re.MatchString(string(a1)) {
		t.Fatalf("id %q is not 16 lowercase hex chars", a1)
	}
	if a1 != a2 {
		t.Fatal("same key gave different ids")
	}
	if a1 == b {
		t.Fatal("different keys gave the same id")
	}
}

func TestNewKeyringRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current []byte
		older   [][]byte
	}{
		{"31-byte current", key(1)[:31], nil},
		{"33-byte current", append(key(1), 0), nil},
		{"nil current", nil, nil},
		{"short older", key(1), [][]byte{key(2)[:31]}},
		{"older equals current", key(1), [][]byte{key(1)}},
		{"duplicate older", key(1), [][]byte{key(2), key(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewKeyring(tc.current, tc.older...); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
