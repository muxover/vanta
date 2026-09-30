# Contributing

## Getting started

```bash
git clone https://github.com/muxover/vanta.git
cd vanta
go build ./...
```

Requires Go 1.24+ on Linux.

## Running tests

```bash
go test -race ./...
```

The IPv6 end-to-end tests need a prefix routed locally on `lo`, and the route test needs root:

```bash
sudo ip -6 route add local fd00:5::/64 dev lo
VANTA_TEST_PREFIX6=fd00:5::/64 go test -race ./internal/proxy/
sudo VANTA_TEST_ROUTES=1 VANTA_TEST_ROUTES_PREFIX=fd00:6::/64 go test ./internal/sys/
```

## Code style

```bash
gofmt -l .
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

## Submitting changes

1. Open an issue first to discuss the change.
2. Branch from `main`, one change per pull request.
3. Add a test that fails without your change.
4. Open a pull request with the template filled in.

## Reporting bugs

Include the Vanta version (`vanta -version`), your Linux distribution and kernel, the config with secrets removed, and the log output with `debug: true`.
