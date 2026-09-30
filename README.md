# Vanta

<div align="center">

[![CI](https://github.com/muxover/vanta/actions/workflows/ci.yml/badge.svg)](https://github.com/muxover/vanta/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muxover/vanta.svg)](https://pkg.go.dev/github.com/muxover/vanta)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/muxover/vanta)](https://github.com/muxover/vanta/releases/latest)

**IPv6 rotating proxy that sends every connection from a new address.**

</div>

---

Vanta turns an IPv6 prefix routed to your server into a pool of source addresses. Clients connect over HTTP, HTTPS CONNECT or SOCKS5 on one port, and every upstream connection leaves from a random address in the prefix — or from the same address every time, for a sticky session. It runs on Linux, stays small (one goroutine pair per tunnel, pooled buffers, no per-session state), and is meant to share a VPS with the monitors and claimers that use it.

---

## Features

- HTTP, HTTPS CONNECT and SOCKS5 on one port, detected from the first byte
- A random source address per connection from one or more IPv6 prefixes; every keep-alive request gets its own
- Sticky sessions: `user-session-<id>` always maps to the same address, with nothing stored per session
- IPv4-only sites are refused by default, so traffic never leaks out from the server's own address
- Username and password auth (constant-time), and an `allow_ips` client allowlist
- Private, loopback, link-local and metadata destinations blocked by default, and the bind prefixes and the proxy's own port always
- WebSocket upgrades, chunked bodies, `Expect: 100-continue` and pipelined requests over plain HTTP
- Inactivity timeout that resets on traffic, so long downloads and quiet tunnels behave
- Local routes for the prefixes on `lo`, added at start and removed at exit
- Live reload on SIGHUP for everything except the listen and metrics addresses
- Prometheus metrics and a health check on a separate port
- One `SO_REUSEPORT` listener per CPU and `IP_BIND_ADDRESS_NO_PORT` on outbound sockets
- Graceful shutdown that drains open connections

---

## Installation

Linux only. Requires Go 1.24+ to build.

```bash
go install github.com/muxover/vanta@latest
```

Or download a binary for `linux/amd64` or `linux/arm64` from [Releases](https://github.com/muxover/vanta/releases).

---

## Quick Start

You need a server with an IPv6 prefix routed to it (a /64 or larger from your provider, routed rather than on-link). Vanta adds the local route and enables `net.ipv6.ip_nonlocal_bind` itself, so it runs as root.

```bash
curl -LO https://raw.githubusercontent.com/muxover/vanta/main/config.example.yaml
mv config.example.yaml config.yaml
# set bind_prefixes to your prefix and change the password
vanta -config config.yaml -check
sudo vanta -config config.yaml
```

Then point a client at it:

```bash
curl -x http://user:change-me@SERVER:8080 https://api64.ipify.org
curl -x socks5h://user:change-me@SERVER:8080 https://api64.ipify.org
```

Each call prints a different address from your prefix.

To run it as a service:

```ini
# /etc/systemd/system/vanta.service
[Unit]
Description=Vanta IPv6 rotating proxy
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/vanta -config /etc/vanta/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

---

## Sticky sessions

Add `-session-<id>` to the username to keep one source address for as long as you use that id. The address is derived from the id, so it survives restarts and reloads as long as `bind_prefixes` stays the same.

```bash
curl -x http://user-session-acct1:change-me@SERVER:8080 https://api64.ipify.org   # same address every time
curl -x http://user-session-acct2:change-me@SERVER:8080 https://api64.ipify.org   # a different one
```

With auth off, use `session-<id>` as the username with any password. Sessions work the same over SOCKS5.

---

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `config.yaml` | Config file to load |
| `-check` | `false` | Validate the config, print every error, and exit |
| `-version` | `false` | Print the version and exit |

---

## Configuration

Unknown keys are an error, so a typo never silently falls back to a default.

| Key | Default | Description |
|-----|---------|-------------|
| `listen_address` | `::` | Address to listen on; `::` also accepts IPv4 clients |
| `listen_port` | — | Port for HTTP, CONNECT and SOCKS5; required |
| `metrics_address` | `127.0.0.1` | Address for `/metrics` and `/health` |
| `metrics_port` | `0` | Port for metrics; `0` turns them off |
| `debug` | `false` | Log every connection and failure |
| `max_connections` | `0` | Concurrent client connections; `0` is unlimited |
| `dial_timeout` | `10` | Seconds to reach the target, across all attempts |
| `idle_timeout` | `300` | Seconds without traffic before a connection is closed; also the limit for sending request headers |
| `dns_server` | `system` | `system` for the OS resolver, or an `ip:port` |
| `dns_cache_size` | `8192` | Cached hostnames; `0` turns the cache off |
| `block_private` | `true` | Refuse loopback, private, link-local, CGNAT and multicast destinations |
| `ipv4_targets` | `block` | `block` refuses sites without IPv6; `direct` reaches them from the server's own IPv4, without rotation |
| `auth.username` | — | Set both to require credentials |
| `auth.password` | — | Set both to require credentials |
| `allow_ips` | `[]` | Client addresses or prefixes allowed to connect; empty allows all |
| `bind_prefixes` | — | IPv6 prefixes to take source addresses from; required |
| `add_routes` | `true` | Add `local <prefix> dev lo` for each prefix, removed on exit |
| `tune_kernel` | `false` | Raise system-wide TCP limits (port range, backlogs, buffers) |
| `deleted_headers` | `[]` | Request headers to strip from plain HTTP requests |

See [config.example.yaml](config.example.yaml) for a full file. Send `SIGHUP` to reload it; open connections keep the settings they started with.

---

## Metrics

With `metrics_port` set, `GET /metrics` serves Prometheus text and `GET /health` returns `ok`.

| Metric | Type |
|--------|------|
| `vanta_connections_active` | gauge |
| `vanta_connections_total` | counter |
| `vanta_connections_rejected_total` | counter |
| `vanta_requests_total` | counter |
| `vanta_bytes_in_total` / `vanta_bytes_out_total` | counter |
| `vanta_auth_failures_total` | counter |
| `vanta_dns_failures_total` | counter |
| `vanta_dial_failures_total` | counter |
| `vanta_blocked_total` | counter |

---

## Limitations

- Linux only; the routes, `ip_nonlocal_bind` and socket options have no equivalent elsewhere.
- The prefix has to be routed to the server. With an on-link prefix, where the provider's router asks for each address, run an NDP proxy such as ndppd alongside. A plan that only allows a single IPv6 address can't rotate at all.
- IPv6 source pools only. A site with no IPv6 address is refused unless `ipv4_targets` is `direct`.
- SOCKS5 supports `CONNECT`; `BIND` and `UDP ASSOCIATE` are refused.
- Plain HTTP proxying covers `http://` URLs; `https://` goes through CONNECT, as every client does by default.

---

## Project Layout

```text
vanta/
├── main.go                  # Flags, startup, SIGHUP reload, metrics server
├── config.example.yaml      # Every setting with its default
├── internal/
│   ├── config/              # Config loading and validation
│   ├── pool/                # Random and sticky source addresses from the prefixes
│   ├── resolve/             # DNS lookups with a TTL cache
│   ├── proxy/               # Listeners, HTTP, CONNECT, SOCKS5 and the relay
│   ├── parse/               # HTTP/1.x request and response parsing
│   ├── metrics/             # Counters and the /metrics handler
│   └── sys/                 # Routes, sysctls and socket options (Linux)
├── release-notes/           # One file per release
└── docs/PROJECT.md          # Maintainer notes
```

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

Licensed under the [MIT](LICENSE) license.

---

## Links

- Repository: https://github.com/muxover/vanta
- Issues: https://github.com/muxover/vanta/issues
- Changelog: [CHANGELOG.md](CHANGELOG.md)
- Go Reference: https://pkg.go.dev/github.com/muxover/vanta

---

<p align="center">Made with ❤️ by Jax (@muxover)</p>
