//go:build linux

package sys

import (
	"net"
	"net/netip"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

// Needs root. Set VANTA_TEST_ROUTES=1 to run it; it adds and removes a local
// route for a documentation prefix.
func TestRoutes(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("VANTA_TEST_ROUTES") == "" {
		t.Skip("set VANTA_TEST_ROUTES=1 and run as root")
	}
	p := netip.MustParsePrefix("198.51.100.0/24")
	if s := os.Getenv("VANTA_TEST_ROUTES_PREFIX"); s != "" {
		p = netip.MustParsePrefix(s)
	}
	src := p.Addr().Next().Next()

	r := NewRoutes()
	if err := r.Sync([]netip.Prefix{p}); err != nil {
		t.Fatal(err)
	}
	if !r.added[p] {
		t.Fatal("route not recorded")
	}
	lo, _ := netlink.LinkByName("lo")
	found, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, localRoute(lo.Attrs().Index, p), netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE|netlink.RT_FILTER_OIF)
	if err != nil || len(found) != 1 {
		t.Fatalf("local route on lo not in the kernel table: %v %v", found, err)
	}
	// with the local route in place, any address in the prefix can be a source
	loop := "127.0.0.1"
	if p.Addr().Is6() {
		loop = "::1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(loop, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: src.AsSlice()}, Control: DialControl}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial from %s: %v", src, err)
	}
	c.Close()

	// a second Sync is a no-op, Close removes what was added
	if err := r.Sync([]netip.Prefix{p}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 0 {
		t.Fatal("route left behind")
	}
	found, _ = netlink.RouteListFiltered(netlink.FAMILY_ALL, localRoute(lo.Attrs().Index, p), netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE|netlink.RT_FILTER_OIF)
	if len(found) != 0 {
		t.Fatal("route still in the kernel table after Close")
	}
}

func TestGatewayPrefixRefused(t *testing.T) {
	gws := []netip.Addr{netip.MustParseAddr("2a02:4780:28::1")}
	if _, ok := containsAny(netip.MustParsePrefix("2a02:4780:28::/48"), gws); !ok {
		t.Fatal("a /48 holding the gateway was accepted")
	}
	if _, ok := containsAny(netip.MustParsePrefix("2a02:4780:28:3ff7::/64"), gws); ok {
		t.Fatal("the server's own /64 was refused")
	}
}
