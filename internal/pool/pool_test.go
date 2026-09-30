package pool

import (
	"fmt"
	"net/netip"
	"testing"
)

func mustPool(t testing.TB, prefixes ...string) *Pool {
	t.Helper()
	var ps []netip.Prefix
	for _, s := range prefixes {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	p, err := New(ps)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRandomStaysInPrefix(t *testing.T) {
	for _, s := range []string{
		"2001:db8::/32", "2001:db8:1::/48", "2001:db8:1:2::/64", "2001:db8::/63",
		"2001:db8::/100", "2001:db8::/120", "2001:db8::/126", "2001:db8::/127",
		"198.51.100.0/24", "10.0.0.0/8", "203.0.113.0/30",
	} {
		pr := netip.MustParsePrefix(s)
		p := mustPool(t, s)
		for range 2000 {
			a := p.Random()
			if !pr.Contains(a) {
				t.Fatalf("%s: %s outside", s, a)
			}
			if a == pr.Addr() {
				t.Fatalf("%s: picked the all-zero host address", s)
			}
		}
	}
}

func TestIPv4NeverBroadcast(t *testing.T) {
	p := mustPool(t, "203.0.113.0/30")
	for range 2000 {
		if a := p.Random(); a.String() == "203.0.113.3" || a.String() == "203.0.113.0" {
			t.Fatalf("picked %s", a)
		}
	}
}

func TestSingleAddress(t *testing.T) {
	for _, s := range []string{"2001:db8::7/128", "203.0.113.9/32"} {
		p := mustPool(t, s)
		want := netip.MustParsePrefix(s).Addr()
		if a := p.Random(); a != want {
			t.Errorf("%s: got %s", s, a)
		}
		if a := p.Sticky("x"); a != want {
			t.Errorf("%s: sticky got %s", s, a)
		}
	}
}

func TestRandomSpreads(t *testing.T) {
	p := mustPool(t, "2001:db8::/64")
	seen := map[netip.Addr]bool{}
	for range 1000 {
		seen[p.Random()] = true
	}
	if len(seen) != 1000 {
		t.Fatalf("%d repeats in 1000 picks from a /64", 1000-len(seen))
	}
}

func TestEveryPrefixUsed(t *testing.T) {
	a, b := netip.MustParsePrefix("2001:db8:a::/48"), netip.MustParsePrefix("2001:db8:b::/48")
	p := mustPool(t, a.String(), b.String())
	var na, nb int
	for range 1000 {
		switch x := p.Random(); {
		case a.Contains(x):
			na++
		case b.Contains(x):
			nb++
		default:
			t.Fatalf("%s outside both", x)
		}
	}
	if na < 400 || nb < 400 {
		t.Fatalf("uneven split %d/%d", na, nb)
	}
}

func TestSticky(t *testing.T) {
	p := mustPool(t, "2001:db8:a::/48", "2001:db8:b::/48")
	x := p.Sticky("session-42")
	for range 100 {
		if p.Sticky("session-42") != x {
			t.Fatal("sticky address changed")
		}
	}
	// a fresh pool with the same prefixes (a restart or reload) maps it the same way
	if mustPool(t, "2001:db8:a::/48", "2001:db8:b::/48").Sticky("session-42") != x {
		t.Fatal("sticky address not stable across pools")
	}
	seen := map[netip.Addr]bool{}
	for i := range 1000 {
		a := p.Sticky(fmt.Sprint("s", i))
		if !p.prefixes[0].Contains(a) && !p.prefixes[1].Contains(a) {
			t.Fatalf("%s outside the pool", a)
		}
		seen[a] = true
	}
	if len(seen) != 1000 {
		t.Fatalf("%d sessions collided", 1000-len(seen))
	}
}

func TestMixedFamiliesRejected(t *testing.T) {
	_, err := New([]netip.Prefix{netip.MustParsePrefix("2001:db8::/48"), netip.MustParsePrefix("203.0.113.0/24")})
	if err == nil {
		t.Fatal("mixed pool accepted")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("empty pool accepted")
	}
}

func BenchmarkRandom(b *testing.B) {
	p := mustPool(b, "2001:db8::/48")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Random()
		}
	})
}

func BenchmarkSticky(b *testing.B) {
	p := mustPool(b, "2001:db8::/48")
	b.ReportAllocs()
	for b.Loop() {
		p.Sticky("session-1234")
	}
}
