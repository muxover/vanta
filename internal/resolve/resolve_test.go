package resolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
)

type fakeDNS struct {
	records map[string][]string
	calls   atomic.Int32
}

func (f *fakeDNS) lookup(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.calls.Add(1)
	var out []netip.Addr
	for _, s := range f.records[host] {
		a := netip.MustParseAddr(s)
		if (network == "ip6") == a.Is6() {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

var records = map[string][]string{
	"dual.test": {"203.0.113.1", "2001:db8::1"},
	"v4.test":   {"203.0.113.2"},
	"v6.test":   {"2001:db8::2"},
}

func TestPrefersPoolFamily(t *testing.T) {
	f := &fakeDNS{records: records}
	r6 := NewWithLookup(f.lookup, 16, true, false)
	if a, _ := r6.Resolve(context.Background(), "dual.test"); a.String() != "2001:db8::1" {
		t.Errorf("ipv6 pool got %s", a)
	}
	// the old resolver always took IPv6, which broke IPv4 pools on dual-stack hosts
	r4 := NewWithLookup(f.lookup, 16, false, false)
	if a, _ := r4.Resolve(context.Background(), "dual.test"); a.String() != "203.0.113.1" {
		t.Errorf("ipv4 pool got %s", a)
	}
}

func TestOtherFamily(t *testing.T) {
	f := &fakeDNS{records: records}
	strict := NewWithLookup(f.lookup, 16, true, false)
	if _, err := strict.Resolve(context.Background(), "v4.test"); !errors.Is(err, ErrNoAddress) {
		t.Errorf("ipv4-only host: %v, want ErrNoAddress", err)
	}
	loose := NewWithLookup(f.lookup, 16, true, true)
	if a, err := loose.Resolve(context.Background(), "v4.test"); err != nil || a.String() != "203.0.113.2" {
		t.Errorf("fallback got %s %v", a, err)
	}
}

func TestCache(t *testing.T) {
	f := &fakeDNS{records: records}
	r := NewWithLookup(f.lookup, 16, true, false)
	for range 5 {
		if _, err := r.Resolve(context.Background(), "V6.test."); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d lookups for one cached name", n)
	}

	f.calls.Store(0)
	for range 3 {
		r.Resolve(context.Background(), "missing.test")
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d lookups for a cached miss", n)
	}
}

func TestLiterals(t *testing.T) {
	f := &fakeDNS{}
	r := NewWithLookup(f.lookup, 16, true, false)
	for in, want := range map[string]string{
		"2001:db8::5":        "2001:db8::5",
		"[2001:db8::5]":      "2001:db8::5",
		"203.0.113.9":        "203.0.113.9",
		"::ffff:203.0.113.9": "203.0.113.9",
		"fe80::1%eth0":       "fe80::1",
	} {
		a, err := r.Resolve(context.Background(), in)
		if err != nil || a.String() != want {
			t.Errorf("%s: got %s %v", in, a, err)
		}
	}
	if f.calls.Load() != 0 {
		t.Error("literal went to DNS")
	}
}

func TestNoCache(t *testing.T) {
	f := &fakeDNS{records: records}
	r := NewWithLookup(f.lookup, 0, true, false)
	r.Resolve(context.Background(), "v6.test")
	r.Resolve(context.Background(), "v6.test")
	if f.calls.Load() != 2 {
		t.Error("dns_cache_size 0 still cached")
	}
}
