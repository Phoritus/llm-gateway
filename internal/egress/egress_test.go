package egress

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCheckRefusesEveryNonPublicAddressClass(t *testing.T) {
	for _, tc := range []struct {
		class, addr string
		refused     bool
	}{
		{"public IPv4", "8.8.8.8", false},
		{"public IPv4 next to CGNAT", "100.128.0.1", false},
		{"public IPv6", "2606:4700:4700::1111", false},
		{"public IPv4 mapped into IPv6", "::ffff:8.8.8.8", false},

		{"loopback", "127.0.0.1", true},
		{"loopback, far end", "127.255.255.254", true},
		{"IPv6 loopback", "::1", true},
		{"loopback mapped into IPv6", "::ffff:127.0.0.1", true},
		{"private 10/8", "10.1.2.3", true},
		{"private 172.16/12", "172.31.255.255", true},
		{"private 192.168/16", "192.168.1.1", true},
		{"private mapped into IPv6", "::ffff:10.0.0.1", true},
		{"cloud metadata", "169.254.169.254", true},
		{"IPv4 link-local", "169.254.1.1", true},
		{"IPv6 link-local", "fe80::1", true},
		{"IPv6 link-local with a zone", "fe80::1%eth0", true},
		{"CGNAT", "100.64.0.1", true},
		{"CGNAT, far end", "100.127.255.255", true},
		{"IPv4 multicast", "224.0.0.1", true},
		{"IPv6 multicast", "ff02::1", true},
		{"IPv4 unspecified", "0.0.0.0", true},
		{"IPv6 unspecified", "::", true},
		{"broadcast", "255.255.255.255", true},
		{"unique local IPv6", "fd00::1", true},
		{"NAT64 of a private address", "64:ff9b::a00:1", true},
		{"6to4", "2002:a00:1::1", true},
		{"Teredo", "2001:0:4136:e378::1", true},
		{"documentation", "192.0.2.10", true},
		{"benchmarking", "198.18.0.1", true},
		{"IPv6 documentation", "3fff::1", true},
		{"IPv6 dummy prefix", "100:0:0:1::1", true},
		{"segment routing identifier", "5f00::1", true},
	} {
		t.Run(tc.class, func(t *testing.T) {
			err := Check(netip.MustParseAddr(tc.addr))
			if got := errors.Is(err, ErrForbiddenAddress); got != tc.refused {
				t.Errorf("Check(%s) = %v, refused %v, want %v", tc.addr, err, got, tc.refused)
			}
		})
	}
}

// trusting is tr with srv's certificate trusted under the name it is issued
// to, so a test can tell a refused dial from a failed handshake.
func trusting(tr *http.Transport, srv *httptest.Server) *http.Transport {
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	return tr
}

func onLocalhost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	return u.String()
}

func TestAHostnameResolvingToLoopbackIsRefusedAtDialTime(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	target := onLocalhost(t, srv)

	open := Client(trusting(newTransport(5*time.Second, func(netip.Addr) error { return nil }), srv))
	resp, err := open.Get(target)
	if err != nil {
		t.Fatalf("without the guard the server answers: %v", err)
	}
	resp.Body.Close()

	guarded := Client(trusting(Transport(5*time.Second), srv))
	_, err = guarded.Get(target)
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("err = %v, want ErrForbiddenAddress from the dial", err)
	}
}

func TestOnlyHTTPSIsCalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a plain http request reached the server")
	}))
	defer srv.Close()
	c := Client(newTransport(5*time.Second, func(netip.Addr) error { return nil }))
	if _, err := c.Get(srv.URL); !errors.Is(err, ErrNotHTTPS) {
		t.Fatalf("err = %v, want ErrNotHTTPS", err)
	}
}

func TestARedirectIsReturnedNotFollowed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			t.Error("the redirect was followed")
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	c := Client(trusting(newTransport(5*time.Second, func(netip.Addr) error { return nil }), srv))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 itself", resp.StatusCode)
	}
}

func TestNoProxyIsTakenFromTheEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if tr := Transport(time.Second); tr.Proxy != nil {
		if u, err := tr.Proxy(req); err != nil || u != nil {
			t.Fatalf("proxy = %v, %v; want none", u, err)
		}
	}
}

func TestTheRefusalNamesTheRange(t *testing.T) {
	err := Check(netip.MustParseAddr("169.254.169.254"))
	if err == nil || !strings.Contains(err.Error(), "169.254.0.0/16") {
		t.Errorf("err = %v, want it to name the range", err)
	}
}
