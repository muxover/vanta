package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/muxover/vanta/internal/config"
)

// ipv6Prefix returns the prefix from VANTA_TEST_PREFIX6. It must be routed
// locally, e.g. `ip -6 route add local fd00:5::/64 dev lo`, which CI does.
func ipv6Prefix(t *testing.T) netip.Prefix {
	s := os.Getenv("VANTA_TEST_PREFIX6")
	if s == "" {
		t.Skip("set VANTA_TEST_PREFIX6 to a prefix routed locally on lo")
	}
	return netip.MustParsePrefix(s)
}

func ipv6Proxy(t *testing.T, p netip.Prefix, mutate func(*config.Config)) string {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.Prefixes = []netip.Prefix{p}
		if mutate != nil {
			mutate(c)
		}
	})
	return addr
}

func ipv6Origin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func checkIPv6Source(t *testing.T, p netip.Prefix, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil || !p.Contains(a) {
		t.Fatalf("source %q is not in %s", s, p)
	}
	return a
}

func TestIPv6Rotation(t *testing.T) {
	p := ipv6Prefix(t)
	addr := ipv6Proxy(t, p, nil)
	origin := ipv6Origin(t)

	c := proxyClient(addr, "", "")
	seen := map[netip.Addr]bool{}
	for range 20 {
		_, body := get(t, c, origin)
		seen[checkIPv6Source(t, p, body)] = true
	}
	if len(seen) < 18 {
		t.Fatalf("only %d distinct IPv6 sources in 20 requests", len(seen))
	}
}

func TestIPv6ConnectAndSOCKS5(t *testing.T) {
	p := ipv6Prefix(t)
	addr := ipv6Proxy(t, p, nil)
	origin := ipv6Origin(t)
	target := strings.TrimPrefix(origin, "http://")

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n\r\nGET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", target)
	got, _ := io.ReadAll(c)
	i := strings.LastIndex(string(got), "\r\n\r\n")
	checkIPv6Source(t, p, string(got[i+4:]))

	s := dialSOCKS(t, addr, methodNone)
	s.method(t)
	if rep := s.connect(t, target, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); rep != repSuccess {
		t.Fatalf("reply %#x", rep)
	}
	got, _ = io.ReadAll(s.br)
	i = strings.LastIndex(string(got), "\r\n\r\n")
	checkIPv6Source(t, p, string(got[i+4:]))
}

func TestIPv6StickyAndIPv4Blocked(t *testing.T) {
	p := ipv6Prefix(t)
	addr := ipv6Proxy(t, p, nil)
	origin := ipv6Origin(t)

	_, a := get(t, proxyClient(addr, "session-abc", "x"), origin)
	_, b := get(t, proxyClient(addr, "session-abc", "x"), origin)
	if a != b {
		t.Fatalf("session moved: %s then %s", a, b)
	}
	checkIPv6Source(t, p, a)

	// an IPv4-only target can't be reached from an IPv6 pool and must not
	// leak out from the server's own IPv4 address
	v4 := peerServer(t)
	if code, _ := get(t, proxyClient(addr, "", ""), v4.URL); code != 502 {
		t.Fatalf("ipv4 target: %d, want 502", code)
	}
	direct := ipv6Proxy(t, p, func(c *config.Config) { c.IPv4Targets = config.IPv4Direct })
	if code, body := get(t, proxyClient(direct, "", ""), v4.URL); code != 200 || body != "127.0.0.1" {
		t.Fatalf("ipv4_targets direct: %d %s", code, body)
	}
}
