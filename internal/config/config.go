package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

const (
	IPv4Block  = "block"
	IPv4Direct = "direct"
)

type Config struct {
	ListenAddress  string `yaml:"listen_address"`
	ListenPort     int    `yaml:"listen_port"`
	MetricsAddress string `yaml:"metrics_address"`
	MetricsPort    int    `yaml:"metrics_port"`
	Debug          bool   `yaml:"debug"`

	MaxConnections int    `yaml:"max_connections"`
	DialTimeout    int    `yaml:"dial_timeout"`
	IdleTimeout    int    `yaml:"idle_timeout"`
	DNSServer      string `yaml:"dns_server"`
	DNSCacheSize   int    `yaml:"dns_cache_size"`
	BlockPrivate   bool   `yaml:"block_private"`
	IPv4Targets    string `yaml:"ipv4_targets"`

	Auth struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"auth"`
	AllowIPs []string `yaml:"allow_ips"`

	BindPrefixes   []string `yaml:"bind_prefixes"`
	AddRoutes      bool     `yaml:"add_routes"`
	TuneKernel     bool     `yaml:"tune_kernel"`
	DeletedHeaders []string `yaml:"deleted_headers"`

	Prefixes []netip.Prefix `yaml:"-"`
	Allow    []netip.Prefix `yaml:"-"`
}

func Default() *Config {
	return &Config{
		ListenAddress:  "::",
		MetricsAddress: "127.0.0.1",
		DialTimeout:    10,
		IdleTimeout:    300,
		DNSServer:      "system",
		DNSCacheSize:   8192,
		BlockPrivate:   true,
		IPv4Targets:    IPv4Block,
		AddRoutes:      true,
	}
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	c := Default()
	if len(bytes.TrimSpace(b)) > 0 {
		if err := yaml.UnmarshalWithOptions(b, c, yaml.Strict()); err != nil {
			return nil, fmt.Errorf("config: %s", yaml.FormatError(err, false, true))
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.ListenAddress, fmt.Sprint(c.ListenPort))
}

func (c *Config) MetricsAddr() string {
	return net.JoinHostPort(c.MetricsAddress, fmt.Sprint(c.MetricsPort))
}

func (c *Config) DialTimeoutDuration() time.Duration {
	return time.Duration(c.DialTimeout) * time.Second
}

func (c *Config) IdleTimeoutDuration() time.Duration {
	return time.Duration(c.IdleTimeout) * time.Second
}

func (c *Config) AuthEnabled() bool {
	return c.Auth.Username != "" || c.Auth.Password != ""
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}

	if c.ListenPort < 1 || c.ListenPort > 65535 {
		bad("listen_port: must be 1-65535")
	}
	if c.ListenAddress != "" {
		if _, err := netip.ParseAddr(c.ListenAddress); err != nil {
			bad("listen_address: %q is not an IP address", c.ListenAddress)
		}
	}
	if c.MetricsPort < 0 || c.MetricsPort > 65535 {
		bad("metrics_port: must be 0-65535")
	}
	if c.MetricsPort > 0 && c.MetricsPort == c.ListenPort {
		bad("metrics_port: must differ from listen_port")
	}
	if c.MetricsAddress != "" {
		if _, err := netip.ParseAddr(c.MetricsAddress); err != nil {
			bad("metrics_address: %q is not an IP address", c.MetricsAddress)
		}
	}
	if c.MaxConnections < 0 {
		bad("max_connections: must be 0 or more")
	}
	if c.DialTimeout < 1 {
		bad("dial_timeout: must be at least 1 second")
	}
	if c.IdleTimeout < 1 {
		bad("idle_timeout: must be at least 1 second")
	}
	if c.DNSCacheSize < 0 {
		bad("dns_cache_size: must be 0 or more")
	}
	if c.DNSServer != "system" {
		host, port, err := net.SplitHostPort(c.DNSServer)
		if err == nil {
			_, err = netip.ParseAddr(host)
		}
		if err != nil || port == "" {
			bad(`dns_server: %q must be "system" or ip:port`, c.DNSServer)
		}
	}
	if c.IPv4Targets != IPv4Block && c.IPv4Targets != IPv4Direct {
		bad("ipv4_targets: must be %q or %q", IPv4Block, IPv4Direct)
	}

	if c.AuthEnabled() {
		if c.Auth.Username == "" || c.Auth.Password == "" {
			bad("auth: username and password are both required")
		}
		if strings.Contains(c.Auth.Username, ":") || strings.Contains(c.Auth.Username, "-session-") {
			bad(`auth.username: must not contain ":" or "-session-"`)
		}
	}

	c.Allow = c.Allow[:0]
	for _, s := range c.AllowIPs {
		p, err := parsePrefix(s)
		if err != nil {
			bad("allow_ips: %v", err)
			continue
		}
		c.Allow = append(c.Allow, p)
	}

	c.Prefixes = c.Prefixes[:0]
	if len(c.BindPrefixes) == 0 {
		bad("bind_prefixes: at least one IPv6 prefix is required")
	}
	for _, s := range c.BindPrefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			bad("bind_prefixes: %q is not a CIDR prefix", s)
			continue
		}
		if !p.Addr().Is6() || p.Addr().Is4In6() {
			bad("bind_prefixes: %s is not an IPv6 prefix", s)
			continue
		}
		c.Prefixes = append(c.Prefixes, p.Masked())
	}

	return errors.Join(errs...)
}

// a bare address in allow_ips means that one host
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a CIDR prefix", s)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an IP address", s)
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}
