# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.4.0] - 2026-09-30

### Added

- Sticky sessions: a username ending in `-session-<id>` (or `session-<id>` with auth off) always leaves from the same address
- `allow_ips` client allowlist
- `ipv4_targets` to refuse or directly reach sites that have no IPv6 address
- `add_routes` adds a local route on `lo` for each prefix and removes it on exit, and refuses a prefix that contains the server's gateway
- `tune_kernel` makes the system-wide TCP tuning opt-in
- `-check` flag to validate a config and print every error
- WebSocket and other `Upgrade` requests over plain HTTP
- `Expect: 100-continue` answered by the proxy
- Prometheus metrics: rejected connections, requests, DNS and dial failures, blocked destinations
- SOCKS5 replies carry the bound source address

### Changed

- IPv6 only: `bind_prefixes` must be IPv6 prefixes
- Unknown config keys and invalid values stop startup with a message for each
- `auth.type` is gone; setting `auth.username` and `auth.password` turns auth on
- `block_private` is on by default and also covers CGNAT, multicast, NAT64-mapped and reserved ranges
- `dns_server` defaults to the system resolver and falls back to TCP for large answers
- `metrics_address` defaults to `127.0.0.1`
- SIGHUP reloads every setting except the listen and metrics addresses, including `max_connections` and DNS options
- Plain HTTP keeps the client connection alive even when the upstream closes after each response
- Dial retries share one `dial_timeout` and stop on a refused connection
- Fewer dependencies: `fastrand`, `go-reuseport`, `go-sysctl` and `testify` replaced by the standard library

### Removed

- `fallback_prefixes`, `replace_ips` and `network_type`
- IPv4 bind prefixes
- v1.0.0 through v1.3.0 are retracted; they had the bugs below

### Fixed

- SOCKS5 refused every client when the config had no `auth` section
- An unknown `auth.type` turned the proxy open without a warning
- Plain HTTP responses were cut at `idle_timeout` even while data was flowing
- Bytes sent right after a CONNECT or SOCKS5 request (such as a TLS hello) were dropped
- WebSocket upgrades over plain HTTP reset the connection
- Route registration always failed; it now adds the local route the prefix needs
- Hostnames with both A and AAAA records always resolved to IPv6, whatever the pool
- An empty `bind_prefixes` sent traffic from the server's own address
- `metrics_address` set to an IPv6 address produced an invalid listen address
- A random address could be the all-zero subnet-router anycast address of the prefix
- `https://` URLs sent without CONNECT went to port 80 in plain text
- SOCKS5 returned the wrong reply codes for DNS failures, blocked destinations and unsupported commands
- Request and response headers had no size limit
- Targets inside the bind prefixes reached the server's own services, and the proxy could be pointed at itself

[Unreleased]: https://github.com/muxover/vanta/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/muxover/vanta/releases/tag/v1.4.0
