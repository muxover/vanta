module github.com/muxover/vanta

go 1.24.0

require (
	github.com/goccy/go-yaml v1.19.2
	github.com/phuslu/lru v1.0.24
	github.com/rs/zerolog v1.35.1
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/sys v0.40.0
)

require (
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
)

retract [v1.0.0, v1.3.0] // broken releases, replaced by v1.4.0
