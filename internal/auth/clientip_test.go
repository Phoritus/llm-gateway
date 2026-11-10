package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// proxyService builds a service trusting the given public proxy ranges, on top
// of the private ranges that are always trusted.
func proxyService(t *testing.T, trusted ...string) *Service {
	t.Helper()
	prefixes := make([]netip.Prefix, 0, len(trusted))
	for _, raw := range trusted {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			t.Fatalf("parse test prefix %q: %v", raw, err)
		}
		prefixes = append(prefixes, p)
	}
	return NewService(nil, Config{
		SessionSecret:  []byte("a test session secret of ample length"),
		TrustedProxies: prefixes,
	})
}

func request(peer string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r.RemoteAddr = peer
	for _, value := range forwarded {
		r.Header.Add("X-Forwarded-For", value)
	}
	return r
}

// The whole point of the key: a client must not be able to choose it. Each
// case below is a way a client might try to.
func TestClientIPIgnoresForwardedForFromAnUntrustedPeer(t *testing.T) {
	svc := proxyService(t)

	cases := []struct {
		name      string
		forwarded []string
	}{
		{"absent", nil},
		{"a well formed spoof", []string{"203.0.113.9"}},
		{"a chain of spoofs", []string{"203.0.113.9, 198.51.100.4, 192.0.2.1"}},
		{"a private address, which would look like a proxy", []string{"10.1.2.3"}},
		{"the header repeated", []string{"203.0.113.9", "198.51.100.4"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request("198.51.100.77:41234", tc.forwarded...)
			addr, ok := svc.ClientIP(r)
			if !ok {
				t.Fatal("ClientIP reported no address for a valid peer")
			}
			if addr.String() != "198.51.100.77" {
				t.Fatalf("ClientIP = %s, want the peer 198.51.100.77", addr)
			}
		})
	}
}

func TestClientIPWalksTheChainFromTheRight(t *testing.T) {
	// 203.0.113.0/24 stands in for the CDN's published ranges; the platform
	// router reaches us from a private address and so is trusted by default.
	svc := proxyService(t, "203.0.113.0/24")

	cases := []struct {
		name      string
		peer      string
		forwarded string
		want      string
		why       string
	}{
		{
			name:      "cdn behind a platform router",
			peer:      "10.0.0.5:8080",
			forwarded: "198.51.100.4, 203.0.113.7",
			want:      "198.51.100.4",
			why:       "the rightmost untrusted entry is the client",
		},
		{
			name:      "a spoof prepended to a real chain",
			peer:      "10.0.0.5:8080",
			forwarded: "1.2.3.4, 198.51.100.4, 203.0.113.7",
			want:      "198.51.100.4",
			why:       "entries left of the real hops are the client's own invention",
		},
		{
			name:      "many spoofs prepended",
			peer:      "10.0.0.5:8080",
			forwarded: "1.2.3.4, 5.6.7.8, 9.10.11.12, 198.51.100.4, 203.0.113.7",
			want:      "198.51.100.4",
			why:       "the walk stops at the first untrusted hop and never reaches them",
		},
		{
			name:      "platform router only, no cdn",
			peer:      "10.0.0.5:8080",
			forwarded: "198.51.100.4",
			want:      "198.51.100.4",
			why:       "one trusted hop, one client entry",
		},
		{
			name:      "an entirely trusted chain",
			peer:      "10.0.0.5:8080",
			forwarded: "203.0.113.1, 203.0.113.7",
			want:      "203.0.113.1",
			why:       "nothing untrusted appears, so the leftmost hop is as close as we get",
		},
		{
			name:      "no header at all from a trusted peer",
			peer:      "10.0.0.5:8080",
			forwarded: "",
			want:      "10.0.0.5",
			why:       "a trusted peer that forwarded nothing is the only address there is",
		},
		{
			name:      "an unreadable entry stops the walk",
			peer:      "10.0.0.5:8080",
			forwarded: "198.51.100.4, not-an-address, 203.0.113.7",
			want:      "203.0.113.7",
			why:       "stepping over garbage would let it hide the hop behind it",
		},
		{
			name:      "a port on a forwarded entry",
			peer:      "10.0.0.5:8080",
			forwarded: "198.51.100.4:1234, 203.0.113.7",
			want:      "198.51.100.4",
			why:       "proxies disagree about whether to add one",
		},
		{
			name:      "an ipv6 client through the cdn",
			peer:      "10.0.0.5:8080",
			forwarded: "2001:db8:1:2::9, 203.0.113.7",
			want:      "2001:db8:1:2::9",
			why:       "the exact address is what the audit column records",
		},
		{
			name:      "a bracketed ipv6 entry",
			peer:      "10.0.0.5:8080",
			forwarded: "[2001:db8:1:2::9], 203.0.113.7",
			want:      "2001:db8:1:2::9",
			why:       "some proxies bracket even without a port",
		},
		{
			name:      "an ipv4 mapped ipv6 entry",
			peer:      "10.0.0.5:8080",
			forwarded: "::ffff:198.51.100.4, 203.0.113.7",
			want:      "198.51.100.4",
			why:       "one client must not get two buckets by changing notation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r *http.Request
			if tc.forwarded == "" {
				r = request(tc.peer)
			} else {
				r = request(tc.peer, tc.forwarded)
			}
			addr, ok := svc.ClientIP(r)
			if !ok {
				t.Fatalf("ClientIP reported no address (%s)", tc.why)
			}
			if addr.String() != tc.want {
				t.Fatalf("ClientIP = %s, want %s: %s", addr, tc.want, tc.why)
			}
		})
	}
}

// A header long enough to be expensive must not be walked in full. The cap
// keeps the rightmost hops, so the answer is still the nearest believable one.
func TestClientIPCapsTheChainLength(t *testing.T) {
	svc := proxyService(t)

	hops := make([]string, 0, maxForwardedHops*4)
	for range maxForwardedHops * 4 {
		hops = append(hops, "203.0.113.9")
	}
	r := request("10.0.0.5:8080", strings.Join(hops, ", "))

	addr, ok := svc.ClientIP(r)
	if !ok {
		t.Fatal("ClientIP reported no address")
	}
	// Every entry is untrusted here, so the walk must stop at the rightmost
	// one immediately rather than reading the whole header.
	if addr.String() != "203.0.113.9" {
		t.Fatalf("ClientIP = %s, want 203.0.113.9", addr)
	}
	if got := len(forwardedHops(r)); got != maxForwardedHops {
		t.Fatalf("forwardedHops parsed %d entries, want the cap of %d", got, maxForwardedHops)
	}
}

func TestRateLimitKey(t *testing.T) {
	svc := proxyService(t)

	t.Run("ipv4 keys on the address", func(t *testing.T) {
		key := svc.RateLimitKey(request("198.51.100.77:41234"))
		if key != "198.51.100.77" {
			t.Fatalf("key = %q, want 198.51.100.77", key)
		}
	})

	// A single household or VPS is handed a whole /64, so keying on the exact
	// address would hand one client 2^64 buckets.
	t.Run("ipv6 keys on the /64", func(t *testing.T) {
		first := svc.RateLimitKey(request("[2001:db8:1:2::1]:443"))
		second := svc.RateLimitKey(request("[2001:db8:1:2:ffff:ffff:ffff:ffff]:443"))
		if first != second {
			t.Fatalf("two addresses in one /64 produced %q and %q", first, second)
		}
		if first != "2001:db8:1:2::/64" {
			t.Fatalf("key = %q, want 2001:db8:1:2::/64", first)
		}

		other := svc.RateLimitKey(request("[2001:db8:1:3::1]:443"))
		if other == first {
			t.Fatal("two different /64s share a bucket")
		}
	})

	t.Run("an ipv4 mapped peer keys as ipv4", func(t *testing.T) {
		key := svc.RateLimitKey(request("[::ffff:198.51.100.77]:41234"))
		if key != "198.51.100.77" {
			t.Fatalf("key = %q, want 198.51.100.77", key)
		}
	})

	// An unattributable request shares one bucket rather than escaping limits.
	t.Run("an unusable peer falls back to one shared bucket", func(t *testing.T) {
		for _, peer := range []string{"", "@", "not-an-address", "garbage:port"} {
			if key := svc.RateLimitKey(request(peer)); key != "unknown" {
				t.Fatalf("peer %q gave key %q, want unknown", peer, key)
			}
		}
	})

	// The regression this fix exists for: a rotating header must not mint
	// fresh buckets.
	t.Run("a spoofed header does not change the key", func(t *testing.T) {
		want := svc.RateLimitKey(request("198.51.100.77:41234"))
		for _, spoof := range []string{"203.0.113.1", "203.0.113.2", "10.0.0.1", "2001:db8::1"} {
			got := svc.RateLimitKey(request("198.51.100.77:41234", spoof))
			if got != want {
				t.Fatalf("X-Forwarded-For %q changed the key from %q to %q", spoof, want, got)
			}
		}
	})
}
