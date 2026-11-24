package egress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var (
	// ErrForbiddenAddress is a connection to an address outside the public
	// internet.
	ErrForbiddenAddress = errors.New("egress: address is not on the public internet")
	// ErrNotHTTPS is a request for a URL whose scheme is not https.
	ErrNotHTTPS = errors.New("egress: only https URLs may be called")
)

var denied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // this network, unspecified
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // carrier-grade NAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, cloud metadata (169.254.169.254)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, broadcast
		"::/96",           // unspecified, loopback, IPv4-compatible
		"64:ff9b::/96",    // NAT64
		"64:ff9b:1::/48",  // local NAT64
		"100::/64",        // discard
		"100:0:0:1::/64",  // dummy prefix
		"2001::/23",       // IETF protocol assignments, Teredo
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4
		"3fff::/20",       // documentation
		"5f00::/16",       // segment routing identifiers
		"fc00::/7",        // unique local
		"fe80::/10",       // link-local
		"fec0::/10",       // site-local
		"ff00::/8",        // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Check refuses an address outside the public internet. An IPv4 address
// written as IPv6 (::ffff:a.b.c.d) is judged as the IPv4 address it is.
func Check(addr netip.Addr) error {
	addr = addr.Unmap().WithZone("")
	for _, p := range denied {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s is in %s", ErrForbiddenAddress, addr, p)
		}
	}
	return nil
}

// Transport is an http.Transport that dials only addresses Check allows and
// takes no proxy from the environment. It is for Client, or for an adapter
// that sets its own redirect policy around it.
func Transport(dialTimeout time.Duration) *http.Transport {
	return newTransport(dialTimeout, Check)
}

func newTransport(dialTimeout time.Duration, check func(netip.Addr) error) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		// Control sees the address being connected to, after resolution, for
		// every address a hostname resolves to.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %q is not an address", ErrForbiddenAddress, address)
			}
			return check(ap.Addr())
		},
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: dialTimeout,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
}

// Client calls https URLs through t and never follows a redirect: the
// redirect response is returned as it came. It has no Timeout, which would
// cut off a long stream.
func Client(t *http.Transport) *http.Client {
	return &http.Client{
		Transport:     httpsOnly{t},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s", ErrNotHTTPS, req.URL.Scheme)
	}
	return h.next.RoundTrip(req)
}
