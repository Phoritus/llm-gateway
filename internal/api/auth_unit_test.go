package api

import "testing"

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  string
		valid bool
	}{
		{"plain", "user@example.com", "user@example.com", true},
		{"uppercase is normalised", "User@Example.COM", "user@example.com", true},
		{"surrounding space is trimmed", "  user@example.com  ", "user@example.com", true},
		{"subdomain", "user@mail.example.co.uk", "user@mail.example.co.uk", true},
		{"plus addressing", "user+tag@example.com", "user+tag@example.com", true},
		{"empty", "", "", false},
		{"no at sign", "userexample.com", "", false},
		{"no domain dot", "user@example", "", false},
		{"trailing dot", "user@example.com.", "", false},
		{"no local part", "@example.com", "", false},
		// A display-name form would store something other than what was
		// submitted, so it is rejected rather than silently rewritten.
		{"display name form", "User <user@example.com>", "", false},
		{"two addresses", "a@example.com, b@example.com", "", false},
		{"header injection attempt", "user@example.com\nBcc: evil@example.com", "", false},
		{"too long", string(make([]byte, maxEmailLen+1)), "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := validateEmail(tc.raw)
			if ok != tc.valid {
				t.Fatalf("validateEmail(%q) ok = %v, want %v", tc.raw, ok, tc.valid)
			}
			if got != tc.want {
				t.Fatalf("validateEmail(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// Rate-limit keys must not carry the address itself, so that Redis never
// holds a list of who uses the service.
func TestEmailKeyHidesTheAddress(t *testing.T) {
	const email = "user@example.com"

	key := emailKey(email)
	if key == "" {
		t.Fatal("emailKey returned an empty string")
	}
	if key == email {
		t.Fatal("emailKey returned the address unchanged")
	}
	if len(key) != 32 {
		t.Fatalf("emailKey length = %d, want 32 hex characters", len(key))
	}

	// Case differences must land in the same bucket, or an attacker could get
	// a fresh allowance per spelling.
	if emailKey("USER@EXAMPLE.COM") != key {
		t.Fatal("emailKey is case sensitive")
	}
	if emailKey("other@example.com") == key {
		t.Fatal("two different addresses share a bucket")
	}
}
