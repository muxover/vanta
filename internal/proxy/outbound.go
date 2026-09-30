package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"

	"github.com/muxover/vanta/internal/config"
	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/resolve"
	"github.com/muxover/vanta/internal/sys"
)

const dialAttempts = 3

var (
	errBlocked = errors.New("destination not allowed")
	errFamily  = errors.New("target has no address the pool can reach")
	errAuth    = errors.New("proxy authentication required")
)

// "alice-session-42" logs in as alice with session 42; "session-42" works
// with auth off
func (st *state) authorize(user, pass string) (string, error) {
	base, session := user, ""
	if i := strings.Index(user, "-session-"); i >= 0 {
		base, session = user[:i], user[i+len("-session-"):]
	} else if s, ok := strings.CutPrefix(user, "session-"); ok {
		base, session = "", s
	}
	if !st.cfg.AuthEnabled() {
		return session, nil
	}
	u := subtle.ConstantTimeCompare([]byte(base), []byte(st.cfg.Auth.Username))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(st.cfg.Auth.Password))
	if u&p != 1 {
		metrics.AuthFailures.Add(1)
		return "", errAuth
	}
	return session, nil
}

func basicCredentials(header []byte) (user, pass string) {
	scheme, enc, ok := strings.Cut(string(header), " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return "", ""
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", ""
	}
	user, pass, _ = strings.Cut(string(dec), ":")
	return user, pass
}

func (st *state) connect(ctx context.Context, host string, port uint16, session string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, st.dialTime)
	defer cancel()

	addr, err := st.dns.Resolve(ctx, host)
	if err != nil {
		metrics.DNSFailures.Add(1)
		if errors.Is(err, resolve.ErrNoAddress) {
			return nil, fmt.Errorf("%w: %v", errFamily, err)
		}
		return nil, err
	}
	if st.forbidden(addr, port) {
		metrics.Blocked.Add(1)
		return nil, fmt.Errorf("%w: %s", errBlocked, addr)
	}

	bind := addr.Is6() == st.pool.Is6()
	if !bind && st.cfg.IPv4Targets != config.IPv4Direct {
		metrics.Blocked.Add(1)
		return nil, fmt.Errorf("%w: %s", errFamily, addr)
	}

	target := netip.AddrPortFrom(addr, port).String()
	var last error
	for range dialAttempts {
		d := net.Dialer{KeepAlive: -1, Control: sys.DialControl}
		if bind {
			src := st.pool.Random()
			if session != "" {
				src = st.pool.Sticky(session)
			}
			d.LocalAddr = &net.TCPAddr{IP: src.AsSlice()}
		}
		c, err := d.DialContext(ctx, "tcp", target)
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil || errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
	}
	metrics.DialFailures.Add(1)
	return nil, last
}

// The bind prefixes are routed to lo, so an address in them is this machine:
// reaching it would expose local services, whatever block_private says. The
// proxy's own port on any local address would loop back into the proxy.
func (st *state) forbidden(a netip.Addr, port uint16) bool {
	a = a.Unmap()
	switch {
	case st.pool.Contains(a):
		return true
	case st.local[a] || a.IsUnspecified():
		if int(port) == st.cfg.ListenPort {
			return true
		}
	}
	return st.cfg.BlockPrivate && (isPrivate(a) || st.local[a])
}

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// link-local covers the cloud metadata endpoints
func isPrivate(a netip.Addr) bool {
	a = a.Unmap()
	if nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case a.IsLoopback(), a.IsPrivate(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(),
		a.IsInterfaceLocalMulticast(), a.IsMulticast(), a.IsUnspecified():
		return true
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fec0::/10"),
}
