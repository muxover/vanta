package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/phuslu/lru"
)

const (
	cacheTTL    = 5 * time.Minute
	negativeTTL = 10 * time.Second
	lookupLimit = 5 * time.Second
)

// ErrNoAddress means the host has no address of a usable family.
var ErrNoAddress = errors.New("no usable address")

type LookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

type Resolver struct {
	lookup LookupFunc
	cache  *lru.TTLCache[string, netip.Addr]
	want6  bool
	other  bool
}

// server is "system" or ip:port; allowOther falls back to the other family
// when the preferred one has no records
func New(server string, cacheSize int, want6, allowOther bool) *Resolver {
	r := &net.Resolver{}
	if server != "system" {
		r.PreferGo = true
		r.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			// keep the network: the Go resolver retries over tcp on truncated answers
			return d.DialContext(ctx, network, server)
		}
	}
	return NewWithLookup(r.LookupNetIP, cacheSize, want6, allowOther)
}

func NewWithLookup(lookup LookupFunc, cacheSize int, want6, allowOther bool) *Resolver {
	res := &Resolver{lookup: lookup, want6: want6, other: allowOther}
	if cacheSize > 0 {
		res.cache = lru.NewTTLCache[string, netip.Addr](cacheSize)
	}
	return res
}

func (r *Resolver) Resolve(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		return a.Unmap().WithZone(""), nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return netip.Addr{}, errors.New("empty host")
	}

	if r.cache != nil {
		if a, ok := r.cache.Get(host); ok {
			if !a.IsValid() {
				return netip.Addr{}, fmt.Errorf("resolve %s: %w (cached)", host, ErrNoAddress)
			}
			return a, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, lookupLimit)
	defer cancel()

	first, second := "ip4", "ip6"
	if r.want6 {
		first, second = "ip6", "ip4"
	}
	a, err := r.pick(ctx, first, host)
	if !a.IsValid() && r.other && ctx.Err() == nil {
		a, err = r.pick(ctx, second, host)
	}
	if !a.IsValid() {
		if r.cache != nil && (err == nil || isNotFound(err)) {
			r.cache.Set(host, netip.Addr{}, negativeTTL)
		}
		if err == nil {
			err = ErrNoAddress
		}
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if r.cache != nil {
		r.cache.Set(host, a, cacheTTL)
	}
	return a, nil
}

func (r *Resolver) pick(ctx context.Context, network, host string) (netip.Addr, error) {
	addrs, err := r.lookup(ctx, network, host)
	for _, a := range addrs {
		a = a.Unmap().WithZone("")
		if a.Is6() == (network == "ip6") {
			return a, nil
		}
	}
	if err != nil && isNotFound(err) {
		err = ErrNoAddress
	}
	return netip.Addr{}, err
}

func isNotFound(err error) bool {
	if errors.Is(err, ErrNoAddress) {
		return true
	}
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}
