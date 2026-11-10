package auth

import (
	"net/http"
	"net/netip"
	"strings"
)

// defaultTrustedProxies are the ranges a request can only reach us from if
// something in front of us forwarded it: the private ranges, carrier-grade NAT
// and link-local. A client on the public internet cannot have a source address
// in any of them, so trusting them cannot be used to spoof anything — and it
// makes a platform deployment right with no configuration at all, because the
// platform's router always reaches the application from a private address.
//
// Loopback is deliberately *not* here, though a request from 127.0.0.1 is
// every bit as certainly local. Trusting it would mean X-Forwarded-For is
// believed in development and in tests but not against a direct connection in
// production, so the behaviour under test would be the opposite of the
// behaviour that matters, and the bypass this file exists to close could not be
// demonstrated locally. A reverse proxy on the same host is a real deployment,
// and it says so with TRUSTED_PROXIES=127.0.0.1.
//
// Public proxy ranges — a CDN in front of the platform — are not here either.
// They differ per deployment and come from TRUSTED_PROXIES.
var defaultTrustedProxies = mustPrefixes(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"169.254.0.0/16",
	"fc00::/7",
	"fe80::/10",
)

// maxForwardedHops bounds how much of X-Forwarded-For is parsed. The header is
// caller-supplied and may be as large as the server's header limit, so an
// unbounded walk is a cheap way to make us parse a megabyte per request. The
// cap keeps the rightmost hops, which are the only believable ones.
const maxForwardedHops = 32

// ipv6BucketBits groups IPv6 addresses by their routed prefix for rate-limit
// purposes. A household or a VPS is routinely handed a whole /64, so keying on
// the exact address would hand one client 2^64 buckets — the same bypass as a
// spoofed header, wearing a different hat.
const ipv6BucketBits = 64

func mustPrefixes(raw ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))
	for _, r := range raw {
		p, err := netip.ParsePrefix(r)
		if err != nil {
			panic("auth: malformed default trusted proxy prefix " + r + ": " + err.Error())
		}
		out = append(out, p)
	}
	return out
}

// ClientIP resolves the address a request should be attributed to, and reports
// whether one could be determined at all.
//
// X-Forwarded-For is read only when the peer is a trusted proxy, and then from
// the right. Every hop appends, so the entries real proxies added are the
// rightmost ones and anything a client prepended stays to the left of them.
// Reading the leftmost entry — the obvious reading of the header, and the one
// this code used to do — is what lets a client name its own address.
func (s *Service) ClientIP(r *http.Request) (netip.Addr, bool) {
	peer, ok := parseHost(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if !s.trusts(peer) {
		// Nothing entitled to speak for someone else handed us this request,
		// so the peer is the client whatever the headers claim.
		return peer, true
	}

	// Walk towards the client. The first hop that is not itself a trusted
	// proxy is as far as the chain can be believed; everything left of it was
	// written by someone we have no reason to trust.
	candidate := peer
	hops := forwardedHops(r)
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseHost(hops[i])
		if !ok {
			// A hop we cannot read is not evidence of anything, and stepping
			// over it would let a garbage entry hide the hop behind it.
			return candidate, true
		}
		if !s.trusts(addr) {
			return addr, true
		}
		candidate = addr
	}
	return candidate, true
}

// RateLimitKey is the bucket key for a request's client: the address itself for
// IPv4, and the containing /64 for IPv6.
//
// The key must be something the client cannot influence, because it is the
// whole of what separates one caller's allowance from another's.
func (s *Service) RateLimitKey(r *http.Request) string {
	addr, ok := s.ClientIP(r)
	if !ok {
		// One shared bucket rather than none: a request we cannot attribute
		// must not escape limiting altogether.
		return "unknown"
	}
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(ipv6BucketBits)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}

// trusts reports whether an address is a proxy allowed to assert who the
// client is.
func (s *Service) trusts(addr netip.Addr) bool {
	for _, p := range defaultTrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedHops returns every X-Forwarded-For entry in order, left to right,
// keeping only the rightmost maxForwardedHops of them. The header may appear
// more than once, and the values then concatenate in order.
func forwardedHops(r *http.Request) []string {
	var hops []string
	for _, value := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	if len(hops) > maxForwardedHops {
		hops = hops[len(hops)-maxForwardedHops:]
	}
	return hops
}

// parseHost reads an address from a RemoteAddr or a forwarded-for entry. It
// accepts a bare address, an address with a port, and a bracketed IPv6
// literal, because RemoteAddr always carries a port and proxies disagree about
// whether to add one.
//
// The result is unmapped and zone-stripped so that one client cannot occupy
// two buckets by arriving as ::ffff:1.2.3.4 instead of 1.2.3.4, and so that
// Prefix.Contains — which rejects a zoned address outright — works.
func parseHost(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return normalize(addr), true
	}
	if ap, err := netip.ParseAddrPort(raw); err == nil {
		return normalize(ap.Addr()), true
	}
	if trimmed := strings.Trim(raw, "[]"); trimmed != raw {
		if addr, err := netip.ParseAddr(trimmed); err == nil {
			return normalize(addr), true
		}
	}
	return netip.Addr{}, false
}

func normalize(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
