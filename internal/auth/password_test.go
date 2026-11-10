package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("hash is not in the standard encoded form: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("hash contains the password")
	}
	if err := VerifyPassword(hash, password); err != nil {
		t.Fatalf("VerifyPassword with the right password: %v", err)
	}
	if err := VerifyPassword(hash, password+"x"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("VerifyPassword with a wrong password = %v, want ErrPasswordMismatch", err)
	}
}

// Two hashes of the same password must differ, or an attacker could tell from
// the database which accounts share a password.
func TestHashPasswordUsesAFreshSalt(t *testing.T) {
	const password = "the same password twice over"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Fatal("hashing the same password twice produced the same hash")
	}
	// Both must still verify.
	for i, hash := range []string{first, second} {
		if err := VerifyPassword(hash, password); err != nil {
			t.Errorf("hash %d does not verify: %v", i, err)
		}
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	good, err := HashPassword("a perfectly fine password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	cases := map[string]string{
		"empty":             "",
		"not a hash":        "hunter2",
		"wrong algorithm":   strings.Replace(good, "argon2id", "argon2i", 1),
		"wrong version":     strings.Replace(good, "v=19", "v=16", 1),
		"missing field":     strings.Join(strings.Split(good, "$")[:5], "$"),
		"bad base64 salt":   mustReplaceField(t, good, 4, "not!base64"),
		"bad base64 digest": mustReplaceField(t, good, 5, "not!base64"),
		"zero cost":         strings.Replace(good, "t=3", "t=0", 1),
	}

	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			if err := VerifyPassword(hash, "a perfectly fine password"); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("VerifyPassword = %v, want ErrInvalidHash", err)
			}
		})
	}
}

// A hash written under weaker settings must keep verifying, otherwise raising
// the cost later would lock every existing user out.
func TestVerifyPasswordHonoursTheParametersInTheHash(t *testing.T) {
	const password = "parameters live in the hash"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Rewriting the recorded cost makes the stored digest wrong for those
	// parameters, which is exactly how we can tell they were used.
	altered := strings.Replace(hash, "t=3", "t=2", 1)
	if err := VerifyPassword(altered, password); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("VerifyPassword with altered cost = %v, want ErrPasswordMismatch", err)
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     error
	}{
		{"long enough", "a reasonable passphrase", nil},
		{"exactly the minimum", "grapefruit12", nil},
		{"one short", "grapefruit1", ErrPasswordTooShort},
		{"empty", "", ErrPasswordTooShort},
		{"common", "password1234", ErrPasswordCommon},
		{"common in mixed case", "Password1234", ErrPasswordCommon},
		{"too long", strings.Repeat("x", MaxPasswordLen+1), ErrPasswordTooLong},
		// Counted in runes, so a short non-ASCII password is still short.
		{"short multibyte", strings.Repeat("ñ", 11), ErrPasswordTooShort},
		{"long multibyte", strings.Repeat("ñ", 12), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidatePassword = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidatePassword = %v, want %v", err, tc.want)
			}
		})
	}
}

// The dummy verification is what keeps a login for an unknown address from
// answering noticeably faster than one for a known address.
func TestVerifyPasswordDummyAlwaysFails(t *testing.T) {
	for _, candidate := range []string{"", "anything at all", strings.Repeat("x", 200)} {
		if err := VerifyPasswordDummy(candidate); !errors.Is(err, ErrPasswordMismatch) {
			t.Fatalf("VerifyPasswordDummy(%q) = %v, want ErrPasswordMismatch", candidate, err)
		}
	}
}

// An over-long password must be rejected before argon2 runs on it, so that a
// huge body cannot be used to burn server CPU.
func TestHashPasswordRejectsOverlongInput(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordLen+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("HashPassword = %v, want ErrPasswordTooLong", err)
	}
}

func mustReplaceField(t *testing.T, hash string, index int, value string) string {
	t.Helper()
	parts := strings.Split(hash, "$")
	if len(parts) <= index {
		t.Fatalf("hash %q has no field %d", hash, index)
	}
	parts[index] = value
	return strings.Join(parts, "$")
}

// Hashing is the only door to a stored password, so the rules are enforced
// there and no caller has to remember to apply them. Before this, a handler and
// the service each called ValidatePassword, and the two disagreed about when:
// which failure a client was told about depended on which layer happened to run
// first.
func TestHashPasswordEnforcesTheRules(t *testing.T) {
	cases := []struct {
		name     string
		password string
		want     error
	}{
		{"too short", "short", ErrPasswordTooShort},
		{"too long", strings.Repeat("x", MaxPasswordLen+1), ErrPasswordTooLong},
		{"too common", "password1234", ErrPasswordCommon},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hash, err := HashPassword(tc.password)
			if !errors.Is(err, tc.want) {
				t.Fatalf("HashPassword = %v, want %v", err, tc.want)
			}
			if hash != "" {
				t.Errorf("HashPassword returned a hash alongside the refusal: %q", hash)
			}
			if !IsPasswordRejected(err) {
				t.Error("IsPasswordRejected = false, want true")
			}
		})
	}
}

// A handler translates the whole class of rule failures without listing its
// members, and must not mistake an unrelated failure for one.
func TestIsPasswordRejectedIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{nil, ErrPasswordMismatch, ErrInvalidHash, ErrNoSession} {
		if IsPasswordRejected(err) {
			t.Errorf("IsPasswordRejected(%v) = true, want false", err)
		}
	}
}
