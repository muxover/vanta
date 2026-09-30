package pool

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"net/netip"
)

type Pool struct {
	prefixes []netip.Prefix
	is6      bool
}

func New(prefixes []netip.Prefix) (*Pool, error) {
	if len(prefixes) == 0 {
		return nil, errors.New("pool: no prefixes")
	}
	p := &Pool{is6: prefixes[0].Addr().Is6()}
	for _, pr := range prefixes {
		if pr.Addr().Is6() != p.is6 {
			return nil, errors.New("pool: prefixes mix IPv4 and IPv6")
		}
		p.prefixes = append(p.prefixes, pr.Masked())
	}
	return p, nil
}

func (p *Pool) Is6() bool { return p.is6 }

func (p *Pool) Contains(a netip.Addr) bool {
	for _, pr := range p.prefixes {
		if pr.Contains(a) {
			return true
		}
	}
	return false
}

func (p *Pool) Random() netip.Addr {
	pr := p.prefixes[0]
	if len(p.prefixes) > 1 {
		pr = p.prefixes[rand.IntN(len(p.prefixes))]
	}
	return addrIn(pr, rand.Uint64(), rand.Uint64())
}

// same key, same address, for as long as the prefix list doesn't change
func (p *Pool) Sticky(key string) netip.Addr {
	h := fnv.New64a()
	h.Write([]byte(key))
	seed := h.Sum64()
	pr := p.prefixes[seed%uint64(len(p.prefixes))]
	hi := splitmix(&seed)
	lo := splitmix(&seed)
	return addrIn(pr, hi, lo)
}

func splitmix(s *uint64) uint64 {
	*s += 0x9e3779b97f4a7c15
	z := *s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// all-zero host bits are the subnet-router anycast (or IPv4 network) address
// and all-ones is IPv4 broadcast, so neither is ever returned
func addrIn(pr netip.Prefix, hi, lo uint64) netip.Addr {
	bits := pr.Addr().BitLen()
	host := bits - pr.Bits()
	if host == 0 {
		return pr.Addr()
	}

	var b [16]byte
	if bits == 128 {
		b = pr.Addr().As16()
	} else {
		a := pr.Addr().As4()
		copy(b[12:], a[:])
	}

	var r [16]byte
	binary.BigEndian.PutUint64(r[:8], hi)
	binary.BigEndian.PutUint64(r[8:], lo)
	for i := range 16 {
		m := hostMask(i, host)
		b[i] = b[i]&^m | r[i]&m
	}

	zero, ones := true, true
	for i := range 16 {
		m := hostMask(i, host)
		if b[i]&m != 0 {
			zero = false
		}
		if b[i]&m != m {
			ones = false
		}
	}
	if zero {
		b[15] |= 1
	} else if ones && bits == 32 && host > 1 {
		b[15] &^= 1
	}

	if bits == 128 {
		return netip.AddrFrom16(b)
	}
	return netip.AddrFrom4([4]byte(b[12:]))
}

func hostMask(i, host int) byte {
	lowBit := (15 - i) * 8
	switch {
	case host >= lowBit+8:
		return 0xff
	case host <= lowBit:
		return 0
	default:
		return byte(1<<(host-lowBit)) - 1
	}
}
