package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
listen_port: 8080
bind_prefixes: ["2001:db8::/48"]
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr() != "[::]:8080" {
		t.Errorf("listen addr %q", c.ListenAddr())
	}
	if !c.BlockPrivate || !c.AddRoutes || c.TuneKernel {
		t.Error("unsafe defaults")
	}
	if c.IPv4Targets != IPv4Block || c.DNSServer != "system" || c.AuthEnabled() {
		t.Error("wrong defaults")
	}
	if len(c.Prefixes) != 1 || c.Prefixes[0].String() != "2001:db8::/48" {
		t.Errorf("prefixes %v", c.Prefixes)
	}
}

func TestIPv6AddressesJoinCorrectly(t *testing.T) {
	c, err := Parse([]byte(minimal + "metrics_port: 9090\nmetrics_address: \"::1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsAddr() != "[::1]:9090" {
		t.Errorf("metrics addr %q", c.MetricsAddr())
	}
}

func TestPrefixMasked(t *testing.T) {
	c, err := Parse([]byte("listen_port: 1\nbind_prefixes: [\"2001:db8::1234/64\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefixes[0].String() != "2001:db8::/64" {
		t.Errorf("got %s", c.Prefixes[0])
	}
}

func TestRejected(t *testing.T) {
	cases := map[string]string{
		"no prefixes":        "listen_port: 8080\n",
		"ipv4 prefix":        "listen_port: 8080\nbind_prefixes: [\"203.0.113.0/24\"]\n",
		"mapped v4 prefix":   "listen_port: 8080\nbind_prefixes: [\"::ffff:203.0.113.0/120\"]\n",
		"bad prefix":         "listen_port: 8080\nbind_prefixes: [\"2001:db8::/200\"]\n",
		"no port":            "bind_prefixes: [\"2001:db8::/48\"]\n",
		"port range":         "listen_port: 70000\nbind_prefixes: [\"2001:db8::/48\"]\n",
		"half auth":          minimal + "auth: {username: u}\n",
		"session in user":    minimal + "auth: {username: a-session-b, password: p}\n",
		"old auth type":      minimal + "auth: {type: credential, username: u, password: p}\n",
		"old fallback key":   minimal + "fallback_prefixes: [\"198.51.100.0/24\"]\n",
		"old network_type":   minimal + "network_type: tcp6\n",
		"ipv4_targets":       minimal + "ipv4_targets: sometimes\n",
		"dns_server":         minimal + "dns_server: 8.8.8.8\n",
		"dns hostname":       minimal + "dns_server: dns.google:53\n",
		"allow_ips":          minimal + "allow_ips: [\"not-an-ip\"]\n",
		"zero dial timeout":  minimal + "dial_timeout: 0\n",
		"same metrics port":  minimal + "metrics_port: 8080\n",
		"listen hostname":    minimal + "listen_address: localhost\n",
		"negative max conns": minimal + "max_connections: -1\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllErrorsReported(t *testing.T) {
	_, err := Parse([]byte("listen_port: 0\ndial_timeout: 0\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"listen_port", "dial_timeout", "bind_prefixes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %s: %v", want, err)
		}
	}
}

func TestAllowIPs(t *testing.T) {
	c, err := Parse([]byte(minimal + "allow_ips: [\"203.0.113.7\", \"2001:db8:1::/48\", \"::ffff:198.51.100.1\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7/32", "2001:db8:1::/48", "198.51.100.1/32"}
	for i, p := range c.Allow {
		if p.String() != want[i] {
			t.Errorf("allow[%d] = %s, want %s", i, p, want[i])
		}
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AuthEnabled() {
		t.Error("the example should ship with auth on")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); !os.IsNotExist(err) {
		t.Fatalf("got %v", err)
	}
}
