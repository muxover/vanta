package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muxover/vanta/internal/config"
	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/pool"
	"github.com/muxover/vanta/internal/resolve"
	"github.com/muxover/vanta/internal/sys"

	"github.com/rs/zerolog/log"
)

const drainTimeout = 30 * time.Second

// swapped whole on reload; open connections keep the one they started with
type state struct {
	cfg      *config.Config
	pool     *pool.Pool
	dns      *resolve.Resolver
	idle     time.Duration
	dialTime time.Duration
	drop     []string
	local    map[netip.Addr]bool
}

func newState(cfg *config.Config) (*state, error) {
	p, err := pool.New(cfg.Prefixes)
	if err != nil {
		return nil, err
	}
	drop := make([]string, len(cfg.DeletedHeaders))
	for i, h := range cfg.DeletedHeaders {
		drop[i] = strings.ToLower(h)
	}
	return &state{
		cfg:      cfg,
		pool:     p,
		dns:      resolve.New(cfg.DNSServer, cfg.DNSCacheSize, p.Is6(), cfg.IPv4Targets == config.IPv4Direct),
		idle:     cfg.IdleTimeoutDuration(),
		dialTime: cfg.DialTimeoutDuration(),
		drop:     drop,
		local:    localAddrs(),
	}, nil
}

func localAddrs() map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Warn().Err(err).Msg("interface addresses")
		return m
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				m[ip.Unmap()] = true
			}
		}
	}
	return m
}

type Server struct {
	st    atomic.Pointer[state]
	conns atomic.Int64
	wg    sync.WaitGroup

	mu    sync.Mutex
	open  map[net.Conn]struct{}
	lns   []net.Listener
	close bool
}

func New(cfg *config.Config) (*Server, error) {
	st, err := newState(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{open: make(map[net.Conn]struct{})}
	s.st.Store(st)
	return s, nil
}

func (s *Server) Reload(cfg *config.Config) error {
	st, err := newState(cfg)
	if err != nil {
		return err
	}
	s.st.Store(st)
	return nil
}

// one SO_REUSEPORT listener per CPU
func (s *Server) Run(ctx context.Context) error {
	addr := s.st.Load().cfg.ListenAddr()
	lc := net.ListenConfig{Control: sys.ListenControl}
	n := runtime.GOMAXPROCS(0)
	for range n {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			s.closeListeners()
			return err
		}
		s.addListener(ln)
	}
	log.Info().Str("addr", addr).Int("listeners", n).Msg("vanta listening")

	var wg sync.WaitGroup
	for _, ln := range s.listeners() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Serve(ln)
		}()
	}
	<-ctx.Done()
	s.Shutdown(drainTimeout)
	wg.Wait()
	return nil
}

func (s *Server) Serve(ln net.Listener) {
	s.addListener(ln)
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// back off on repeated errors (EMFILE and friends) instead of spinning
			backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
			log.Error().Err(err).Dur("retry_in", backoff).Msg("accept")
			time.Sleep(backoff)
			continue
		}
		backoff = 0

		st := s.st.Load()
		if !s.admit(st, conn) {
			metrics.Rejected.Add(1)
			conn.Close()
			continue
		}
		if !s.track(conn) {
			s.conns.Add(-1)
			conn.Close()
			return
		}
		go func() {
			defer s.untrack(conn)
			serveConn(st, conn)
		}()
	}
}

func (s *Server) admit(st *state, conn net.Conn) bool {
	if len(st.cfg.Allow) > 0 {
		ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
		if err != nil {
			return false
		}
		ip := ap.Addr().Unmap()
		ok := false
		for _, p := range st.cfg.Allow {
			if p.Contains(ip) {
				ok = true
				break
			}
		}
		if !ok {
			log.Debug().Stringer("peer", conn.RemoteAddr()).Msg("not in allow_ips")
			return false
		}
	}
	// reserve the slot here so parallel listeners can't overshoot the limit
	if n := s.conns.Add(1); st.cfg.MaxConnections > 0 && n > int64(st.cfg.MaxConnections) {
		s.conns.Add(-1)
		log.Debug().Stringer("peer", conn.RemoteAddr()).Msg("max_connections reached")
		return false
	}
	return true
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.close {
		return false
	}
	s.open[conn] = struct{}{}
	s.wg.Add(1)
	metrics.ConnsActive.Add(1)
	metrics.ConnsTotal.Add(1)
	return true
}

func (s *Server) untrack(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.open, conn)
	s.mu.Unlock()
	s.conns.Add(-1)
	metrics.ConnsActive.Add(-1)
	s.wg.Done()
}

// connections still open after timeout are closed
func (s *Server) Shutdown(timeout time.Duration) {
	s.mu.Lock()
	s.close = true
	s.mu.Unlock()
	s.closeListeners()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		log.Warn().Dur("timeout", timeout).Msg("drain timed out, closing connections")
	}
	s.mu.Lock()
	for c := range s.open {
		c.Close()
	}
	s.mu.Unlock()
	<-done
}

func (s *Server) addListener(ln net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lns {
		if l == ln {
			return
		}
	}
	s.lns = append(s.lns, ln)
}

func (s *Server) listeners() []net.Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]net.Listener(nil), s.lns...)
}

func (s *Server) closeListeners() {
	for _, ln := range s.listeners() {
		ln.Close()
	}
}
