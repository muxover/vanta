//go:build linux

package sys

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// lets any address in a routed prefix be a source, configured or not
func EnableNonlocalBind() error {
	return writeSysctl("net.ipv6.ip_nonlocal_bind", "1")
}

var tuning = [][2]string{
	{"net.ipv4.ip_local_port_range", "1024 65535"},
	{"net.ipv4.tcp_tw_reuse", "1"},
	{"net.ipv4.tcp_fin_timeout", "15"},
	{"net.core.somaxconn", "65535"},
	{"net.core.netdev_max_backlog", "65535"},
	{"net.ipv4.tcp_max_syn_backlog", "65535"},
	{"net.core.rmem_max", "16777216"},
	{"net.core.wmem_max", "16777216"},
	{"net.ipv4.tcp_rmem", "4096 87380 16777216"},
	{"net.ipv4.tcp_wmem", "4096 65536 16777216"},
}

// the net.ipv4.tcp_* keys apply to IPv6 sockets too
func Tune() []error {
	var errs []error
	for _, kv := range tuning {
		if err := writeSysctl(kv[0], kv[1]); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func writeSysctl(key, val string) error {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		return fmt.Errorf("sysctl %s: %w", key, err)
	}
	return nil
}

// Routes keeps "local <prefix> dev lo" for every bind prefix so the kernel
// accepts replies to any address in it. Only routes added here get removed.
type Routes struct {
	mu    sync.Mutex
	added map[netip.Prefix]bool
}

func NewRoutes() *Routes {
	return &Routes{added: make(map[netip.Prefix]bool)}
}

func (r *Routes) Sync(prefixes []netip.Prefix) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("route: lo: %w", err)
	}

	want := make(map[netip.Prefix]bool, len(prefixes))
	var errs []error
	gws := gateways()
	for _, p := range prefixes {
		want[p] = true
		if r.added[p] {
			continue
		}
		if gw, ok := containsAny(p, gws); ok {
			// routing the gateway to lo would cut this machine off the network
			errs = append(errs, fmt.Errorf("route: %s contains the gateway %s; use only the prefix routed to this server, usually its /64", p, gw))
			continue
		}
		err := netlink.RouteAdd(localRoute(lo.Attrs().Index, p))
		switch {
		case err == nil:
			r.added[p] = true
		case errors.Is(err, syscall.EEXIST):
		default:
			errs = append(errs, fmt.Errorf("route add local %s dev lo: %w", p, err))
		}
	}
	for p := range r.added {
		if want[p] {
			continue
		}
		if err := netlink.RouteDel(localRoute(lo.Attrs().Index, p)); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("route del local %s dev lo: %w", p, err))
			continue
		}
		delete(r.added, p)
	}
	return errors.Join(errs...)
}

func gateways() []netip.Addr {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, rt := range routes {
		if a, ok := netip.AddrFromSlice(rt.Gw); ok {
			out = append(out, a.Unmap())
		}
		for _, nh := range rt.MultiPath {
			if a, ok := netip.AddrFromSlice(nh.Gw); ok {
				out = append(out, a.Unmap())
			}
		}
	}
	return out
}

func containsAny(p netip.Prefix, addrs []netip.Addr) (netip.Addr, bool) {
	for _, a := range addrs {
		if p.Contains(a) {
			return a, true
		}
	}
	return netip.Addr{}, false
}

func (r *Routes) Close() error {
	return r.Sync(nil)
}

func localRoute(link int, p netip.Prefix) *netlink.Route {
	return &netlink.Route{
		LinkIndex: link,
		Dst:       &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())},
		Type:      unix.RTN_LOCAL,
		Table:     unix.RT_TABLE_LOCAL,
		Scope:     netlink.SCOPE_HOST,
	}
}

// IPv6 won't bind an address that isn't on an interface, local route or not,
// unless the socket is freebind. The port is picked at connect instead of bind
// so sticky sessions can open many sockets from one address.
func DialControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
		if serr == nil {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BIND_ADDRESS_NO_PORT, 1)
		}
	})
	if err != nil {
		return err
	}
	return serr
}

func ListenControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
