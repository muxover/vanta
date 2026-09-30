package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"

	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/resolve"
)

// RFC 1928 and RFC 1929
const (
	socks5Version = 0x05

	methodNone     = 0x00
	methodUserPass = 0x02
	methodNoAccept = 0xff

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess         = 0x00
	repFailure         = 0x01
	repNotAllowed      = 0x02
	repNetUnreachable  = 0x03
	repHostUnreachable = 0x04
	repRefused         = 0x05
	repCmdUnsupported  = 0x07
	repAtypUnsupported = 0x08
)

func (cl *client) serveSOCKS5() {
	br := cl.br
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}

	// without auth, username/password is still accepted so clients can pass a session
	chosen := byte(methodNoAccept)
	for _, m := range methods {
		if m == methodUserPass {
			chosen = methodUserPass
			break
		}
		if m == methodNone && !cl.st.cfg.AuthEnabled() {
			chosen = methodNone
		}
	}
	if _, err := cl.c.Write([]byte{socks5Version, chosen}); err != nil || chosen == methodNoAccept {
		cl.debug().Msg("socks5: no acceptable auth method")
		return
	}

	var session string
	if chosen == methodUserPass {
		user, pass, err := readUserPass(br)
		if err != nil {
			return
		}
		session, err = cl.st.authorize(user, pass)
		if err != nil {
			cl.c.Write([]byte{0x01, 0x01})
			cl.debug().Msg("socks5: auth failed")
			return
		}
		if _, err := cl.c.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}

	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil {
		return
	}
	if req[0] != socks5Version {
		return
	}
	host, port, err := readAddr(req[3], br)
	if err != nil {
		cl.socksReply(repAtypUnsupported, nil)
		return
	}
	if req[1] != cmdConnect {
		cl.socksReply(repCmdUnsupported, nil)
		return
	}
	metrics.Requests.Add(1)

	up, err := cl.st.connect(context.Background(), host, port, session)
	if err != nil {
		cl.debug().Err(err).Str("host", host).Msg("socks5: connect failed")
		cl.socksReply(socksCode(err), nil)
		return
	}
	uc := newIdleConn(up, cl.st.idle)
	defer uc.Close()

	if err := cl.socksReply(repSuccess, up.LocalAddr()); err != nil {
		return
	}
	out, in := relay(cl.c, br, uc, nil)
	cl.debug().Str("host", host).Int64("out", out).Int64("in", in).Msg("socks5")
}

func readUserPass(br io.Reader) (user, pass string, err error) {
	var b [2]byte
	if _, err = io.ReadFull(br, b[:]); err != nil {
		return
	}
	if b[0] != 0x01 {
		return "", "", errors.New("socks5: bad auth version")
	}
	u := make([]byte, b[1])
	if _, err = io.ReadFull(br, u); err != nil {
		return
	}
	if _, err = io.ReadFull(br, b[:1]); err != nil {
		return
	}
	p := make([]byte, b[0])
	if _, err = io.ReadFull(br, p); err != nil {
		return
	}
	return string(u), string(p), nil
}

func readAddr(atyp byte, br io.Reader) (host string, port uint16, err error) {
	var b [258]byte
	switch atyp {
	case atypIPv4:
		if _, err = io.ReadFull(br, b[:6]); err != nil {
			return
		}
		host = netip.AddrFrom4([4]byte(b[:4])).String()
		port = binary.BigEndian.Uint16(b[4:6])
	case atypIPv6:
		if _, err = io.ReadFull(br, b[:18]); err != nil {
			return
		}
		host = netip.AddrFrom16([16]byte(b[:16])).String()
		port = binary.BigEndian.Uint16(b[16:18])
	case atypDomain:
		if _, err = io.ReadFull(br, b[:1]); err != nil {
			return
		}
		n := int(b[0])
		if _, err = io.ReadFull(br, b[:n+2]); err != nil {
			return
		}
		host = string(b[:n])
		port = binary.BigEndian.Uint16(b[n : n+2])
	default:
		err = errors.New("socks5: unsupported address type")
	}
	return
}

func (cl *client) socksReply(rep byte, bound net.Addr) error {
	b := make([]byte, 0, 22)
	b = append(b, socks5Version, rep, 0x00)
	var ap netip.AddrPort
	if ta, ok := bound.(*net.TCPAddr); ok {
		ap = ta.AddrPort()
	}
	a := ap.Addr().Unmap()
	switch {
	case a.Is6():
		ip := a.As16()
		b = append(b, atypIPv6)
		b = append(b, ip[:]...)
	case a.Is4():
		ip := a.As4()
		b = append(b, atypIPv4)
		b = append(b, ip[:]...)
	default:
		b = append(b, atypIPv4, 0, 0, 0, 0)
	}
	b = binary.BigEndian.AppendUint16(b, ap.Port())
	_, err := cl.c.Write(b)
	return err
}

func socksCode(err error) byte {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errBlocked):
		return repNotAllowed
	case errors.Is(err, errFamily), errors.Is(err, resolve.ErrNoAddress), errors.As(err, &dnsErr):
		return repHostUnreachable
	case errors.Is(err, syscall.ECONNREFUSED):
		return repRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return repNetUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return repHostUnreachable
	case isTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return repHostUnreachable
	}
	return repFailure
}
