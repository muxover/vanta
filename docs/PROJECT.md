# Vanta — maintainer notes

## What this is

An IPv6 rotating proxy for Linux. It takes one or more IPv6 prefixes routed to the server and sends every upstream connection from a random address in them, or from a fixed address per sticky session. Clients use HTTP, HTTPS CONNECT or SOCKS5 on one port. It's built for monitors and claimers running on the same VPS, so it has to stay fast and small.

## Stack & layout

- Go 1.24+, Linux. Dependencies: goccy/go-yaml (strict config parsing), phuslu/lru (DNS cache), rs/zerolog (logs), vishvananda/netlink (routes), golang.org/x/sys (socket options).
- `main.go`: flags, startup, SIGHUP reload, metrics server.
- `internal/config`: YAML loading, defaults, validation of every field.
- `internal/pool`: random and sticky address generation with `net/netip`, no allocations.
- `internal/resolve`: DNS with a TTL cache, preferring the pool's address family.
- `internal/proxy`: listeners, per-connection state snapshot, HTTP, CONNECT, SOCKS5, the relay and idle deadlines.
- `internal/parse`: HTTP/1.x request and response heads with size limits and pooled buffers.
- `internal/sys`: local routes on `lo`, sysctls, `SO_REUSEPORT` and `IP_BIND_ADDRESS_NO_PORT`.

## Decisions

- 2026-09-29: IPv6 only. Nobody uses rotating pools for IPv4, and dropping it removed `fallback_prefixes`, `replace_ips` and the mixed-family code.
- 2026-09-29: IPv4-only targets are refused by default (`ipv4_targets: block`) so traffic never leaves from the server's own address.
- 2026-09-29: v1.4.0 is the first version meant to be used; v1.0.0–v1.3.0 are retracted in `go.mod`. The GitHub repository was recreated with one commit and one release.
- 2026-09-29: Sticky sessions hash the session id into an address (FNV-1a plus splitmix64) instead of keeping a table, so they cost no memory and survive restarts.
- 2026-09-29: Config parsing is strict; removed and misspelled keys stop startup rather than falling back to defaults.
- 2026-09-29: Each plain HTTP request gets a new upstream connection (and address), sent with `Connection: close`; the client side stays keep-alive.
- 2026-09-29: The relay copies through pooled 32 KB buffers instead of splice so the idle deadline can move on traffic.
- 2026-09-29: Routes are `local <prefix> dev lo` (AnyIP). Only routes Vanta added are removed on reload or exit.
- 2026-09-29: System-wide TCP tuning is opt-in (`tune_kernel`); only `net.ipv6.ip_nonlocal_bind` is always set.
- 2026-09-29: Per-connection failures log at debug level and are counted in metrics, so a busy proxy doesn't flood the journal.

## Status

- Unit and end-to-end tests cover HTTP, CONNECT, SOCKS5, sessions, auth, limits, reload and shutdown over loopback, plus IPv6 end-to-end tests that CI runs against a prefix routed on `lo`.
- Not yet run on a real routed IPv6 prefix from a hosting provider.

## Next

- Run it on a VPS with a routed /48 or /64 and check rotation against an outside IP echo service.

## Open questions

None.
