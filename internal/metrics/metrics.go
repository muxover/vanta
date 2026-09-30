package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var (
	ConnsActive  atomic.Int64
	ConnsTotal   atomic.Int64
	Rejected     atomic.Int64
	Requests     atomic.Int64
	BytesIn      atomic.Int64
	BytesOut     atomic.Int64
	AuthFailures atomic.Int64
	DNSFailures  atomic.Int64
	DialFailures atomic.Int64
	Blocked      atomic.Int64
)

type metric struct {
	name, kind, help string
	v                *atomic.Int64
}

var all = []metric{
	{"vanta_connections_active", "gauge", "Client connections open now.", &ConnsActive},
	{"vanta_connections_total", "counter", "Client connections accepted.", &ConnsTotal},
	{"vanta_connections_rejected_total", "counter", "Client connections refused by allow_ips or max_connections.", &Rejected},
	{"vanta_requests_total", "counter", "Proxied HTTP requests, CONNECT tunnels and SOCKS5 connections.", &Requests},
	{"vanta_bytes_in_total", "counter", "Bytes received from upstream servers.", &BytesIn},
	{"vanta_bytes_out_total", "counter", "Bytes sent to upstream servers.", &BytesOut},
	{"vanta_auth_failures_total", "counter", "Failed proxy authentications.", &AuthFailures},
	{"vanta_dns_failures_total", "counter", "Target hostnames that did not resolve to a usable address.", &DNSFailures},
	{"vanta_dial_failures_total", "counter", "Upstream connections that failed after all attempts.", &DialFailures},
	{"vanta_blocked_total", "counter", "Requests refused by block_private or ipv4_targets.", &Blocked},
}

func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, m := range all {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", m.name, m.help, m.name, m.kind, m.name, m.v.Load())
	}
}
