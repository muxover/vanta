package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muxover/vanta/internal/config"
)

// The end-to-end tests run on loopback with an IPv4 pool inside 127.0.0.0/8,
// where every address is local, so they work on any Linux box without setup.
// ipv6_test.go runs the same paths over a routed IPv6 prefix when one is given.
var testPrefix = netip.MustParsePrefix("127.77.0.0/16")

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.ListenAddress = "127.0.0.1"
	cfg.ListenPort = 1
	cfg.BlockPrivate = false
	cfg.IdleTimeout = 5
	cfg.DialTimeout = 3
	cfg.Prefixes = []netip.Prefix{testPrefix}
	return cfg
}

func startProxy(t *testing.T, mutate func(*config.Config)) (string, *Server) {
	t.Helper()
	cfg := testConfig()
	if mutate != nil {
		mutate(cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(time.Second) })
	return ln.Addr().String(), srv
}

// peerServer answers every request with the address it came from.
func peerServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	}))
	t.Cleanup(s.Close)
	return s
}

func proxyClient(proxyAddr, user, pass string) *http.Client {
	u := &url.URL{Scheme: "http", Host: proxyAddr}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(u),
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			ExpectContinueTimeout: 5 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

func inPool(t *testing.T, ip string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(ip)
	if err != nil || !testPrefix.Contains(a) {
		t.Fatalf("source %q is not in %s", ip, testPrefix)
	}
	return a
}

func TestHTTPRotatesSourceAddress(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := peerServer(t)
	c := proxyClient(addr, "", "")

	seen := map[netip.Addr]bool{}
	for range 20 {
		code, body := get(t, c, origin.URL)
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		seen[inPool(t, body)] = true
	}
	if len(seen) < 15 {
		t.Fatalf("only %d distinct sources in 20 requests", len(seen))
	}
}

func TestHTTPSThroughConnect(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	}))
	defer origin.Close()

	code, body := get(t, proxyClient(addr, "", ""), origin.URL)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	inPool(t, body)
}

// Keep-alive with the client works even though every upstream connection is
// closed after one response, and each request still gets a new source.
func TestKeepAliveNewSourcePerRequest(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := peerServer(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)

	seen := map[netip.Addr]bool{}
	for range 5 {
		fmt.Fprintf(c, "GET %s/ HTTP/1.1\r\nHost: x\r\n\r\n", origin.URL)
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.Close {
			t.Fatal("proxy closed the client connection")
		}
		seen[inPool(t, string(b))] = true
	}
	if len(seen) < 4 {
		t.Fatalf("keep-alive requests reused sources: %d distinct of 5", len(seen))
	}
}

func TestPipelinedRequests(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := peerServer(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: x\r\n\r\n", origin.URL)
	io.WriteString(c, req+req+req)

	br := bufio.NewReader(c)
	for i := range 3 {
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}

// A download slower than idle_timeout in total must not be cut while data flows.
func TestSlowBodyOutlivesIdleTimeout(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) { c.IdleTimeout = 1 })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(200)
		for _, b := range []byte("abcdef") {
			w.Write([]byte{b})
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		}
	}))
	defer origin.Close()

	code, body := get(t, proxyClient(addr, "", ""), origin.URL)
	if code != 200 || body != "abcdef" {
		t.Fatalf("got %d %q, want the full body", code, body)
	}
}

func TestStalledUpstreamIsCut(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) { c.IdleTimeout = 1 })
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer origin.Close()
	defer close(release)

	start := time.Now()
	res, err := proxyClient(addr, "", "").Get(origin.URL)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("stalled body finished")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("stalled connection lived %s with idle_timeout 1s", d)
	}
}

func TestChunkedBothWays(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.(http.Flusher).Flush() // forces a chunked response
		w.Write(bytes.ToUpper(b))
	}))
	defer origin.Close()

	c := proxyClient(addr, "", "")
	req, _ := http.NewRequest("POST", origin.URL, io.NopCloser(strings.NewReader("hello chunks")))
	req.ContentLength = -1
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if string(b) != "HELLO CHUNKS" {
		t.Fatalf("got %q", b)
	}
	if len(res.TransferEncoding) == 0 {
		t.Fatal("response wasn't chunked")
	}
}

func TestExpectContinue(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	}))
	defer origin.Close()

	req, _ := http.NewRequest("PUT", origin.URL, strings.NewReader("payload"))
	req.Header.Set("Expect", "100-continue")
	start := time.Now()
	res, err := proxyClient(addr, "", "").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "payload" {
		t.Fatalf("got %q", b)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("client waited for the 100-continue timeout")
	}
}

func TestNoBodyResponsesDontHang(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/204":
			w.WriteHeader(204)
		case "/304":
			w.WriteHeader(304)
		default:
			w.Header().Set("Content-Length", "100")
		}
	}))
	defer origin.Close()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(c)
	for _, r := range []struct{ method, path string }{{"HEAD", "/"}, {"GET", "/204"}, {"GET", "/304"}, {"HEAD", "/"}} {
		fmt.Fprintf(c, "%s %s%s HTTP/1.1\r\nHost: x\r\n\r\n", r.method, origin.URL, r.path)
		res, err := http.ReadResponse(br, &http.Request{Method: r.method})
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		res.Body.Close()
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade header", 400)
			return
		}
		conn, brw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		buf := make([]byte, 4)
		io.ReadFull(brw, buf)
		conn.Write(append([]byte("echo:"), buf...))
	}))
	defer origin.Close()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "GET %s/ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", origin.URL)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	c.Write([]byte("PING"))
	got, _ := io.ReadAll(br)
	if string(got) != "echo:PING" {
		t.Fatalf("got %q", got)
	}
}

// echoServer writes back "<source>|<first bytes>" for raw tunnel tests.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
				fmt.Fprintf(c, "%s|%s", host, buf[:n])
			}()
		}
	}()
	return ln.Addr().String()
}

// Bytes a client sends right behind CONNECT (a TLS hello, usually) reach the target.
func TestConnectEarlyData(t *testing.T) {
	addr, _ := startProxy(t, nil)
	target := echoServer(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nEARLY", target, target)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("connect: %v %v", res, err)
	}
	got, _ := io.ReadAll(br)
	src, data, _ := strings.Cut(string(got), "|")
	inPool(t, src)
	if data != "EARLY" {
		t.Fatalf("target got %q, want the early bytes", data)
	}
}

type socksClient struct {
	c  net.Conn
	br *bufio.Reader
}

func dialSOCKS(t *testing.T, addr string, methods ...byte) *socksClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(8 * time.Second))
	c.Write(append([]byte{5, byte(len(methods))}, methods...))
	return &socksClient{c: c, br: bufio.NewReader(c)}
}

func (s *socksClient) method(t *testing.T) byte {
	t.Helper()
	var b [2]byte
	if _, err := io.ReadFull(s.br, b[:]); err != nil {
		t.Fatalf("method reply: %v", err)
	}
	return b[1]
}

func (s *socksClient) login(t *testing.T, user, pass string) byte {
	t.Helper()
	msg := []byte{1, byte(len(user))}
	msg = append(msg, user...)
	msg = append(msg, byte(len(pass)))
	msg = append(msg, pass...)
	s.c.Write(msg)
	var b [2]byte
	if _, err := io.ReadFull(s.br, b[:]); err != nil {
		t.Fatalf("auth reply: %v", err)
	}
	return b[1]
}

// connect sends a CONNECT for a domain or IP target plus extra bytes in the
// same write, and returns the reply code.
func (s *socksClient) connect(t *testing.T, target string, extra string) byte {
	t.Helper()
	host, port, _ := net.SplitHostPort(target)
	msg := []byte{5, 1, 0}
	if a, err := netip.ParseAddr(host); err == nil && a.Is4() {
		ip := a.As4()
		msg = append(append(msg, atypIPv4), ip[:]...)
	} else {
		msg = append(append(msg, atypDomain, byte(len(host))), host...)
	}
	var p uint16
	fmt.Sscan(port, &p)
	msg = binary.BigEndian.AppendUint16(msg, p)
	s.c.Write(append(msg, extra...))

	head := make([]byte, 4)
	if _, err := io.ReadFull(s.br, head); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	n := 4
	if head[3] == atypIPv6 {
		n = 16
	}
	io.ReadFull(s.br, make([]byte, n+2))
	return head[1]
}

func TestSOCKS5WithoutAuthSection(t *testing.T) {
	addr, _ := startProxy(t, nil)
	target := echoServer(t)

	s := dialSOCKS(t, addr, methodNone)
	if m := s.method(t); m != methodNone {
		t.Fatalf("method %#x, want no-auth", m)
	}
	if rep := s.connect(t, target, "EARLY"); rep != repSuccess {
		t.Fatalf("reply %#x", rep)
	}
	got, _ := io.ReadAll(s.br)
	src, data, _ := strings.Cut(string(got), "|")
	inPool(t, src)
	if data != "EARLY" {
		t.Fatalf("target got %q", data)
	}
}

func TestSOCKS5Auth(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.Auth.Username, c.Auth.Password = "u", "p"
	})
	target := echoServer(t)

	s := dialSOCKS(t, addr, methodNone)
	if m := s.method(t); m != methodNoAccept {
		t.Fatalf("no-auth accepted with auth on: %#x", m)
	}

	s = dialSOCKS(t, addr, methodNone, methodUserPass)
	s.method(t)
	if st := s.login(t, "u", "wrong"); st == 0 {
		t.Fatal("wrong password accepted")
	}

	s = dialSOCKS(t, addr, methodUserPass)
	s.method(t)
	if st := s.login(t, "u", "p"); st != 0 {
		t.Fatal("login failed")
	}
	if rep := s.connect(t, target, "x"); rep != repSuccess {
		t.Fatalf("reply %#x", rep)
	}
}

func TestSOCKS5ReplyCodes(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) { c.BlockPrivate = true })

	s := dialSOCKS(t, addr, methodNone)
	s.method(t)
	if rep := s.connect(t, "127.0.0.1:80", ""); rep != repNotAllowed {
		t.Fatalf("blocked destination: reply %#x, want %#x", rep, repNotAllowed)
	}

	s = dialSOCKS(t, addr, methodNone)
	s.method(t)
	if rep := s.connect(t, "does-not-exist.invalid:80", ""); rep != repHostUnreachable {
		t.Fatalf("unknown host: reply %#x, want %#x", rep, repHostUnreachable)
	}

	s = dialSOCKS(t, addr, methodNone)
	s.method(t)
	s.c.Write([]byte{5, 2, 0, atypIPv4, 1, 1, 1, 1, 0, 80}) // BIND
	head := make([]byte, 2)
	io.ReadFull(s.br, head)
	if head[1] != repCmdUnsupported {
		t.Fatalf("bind: reply %#x, want %#x", head[1], repCmdUnsupported)
	}
}

func TestStickySessions(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.Auth.Username, c.Auth.Password = "alice", "pw"
	})
	origin := peerServer(t)

	src := func(user string) string {
		_, body := get(t, proxyClient(addr, user, "pw"), origin.URL)
		inPool(t, body)
		return body
	}
	a := src("alice-session-one")
	for range 5 {
		if got := src("alice-session-one"); got != a {
			t.Fatalf("session moved from %s to %s", a, got)
		}
	}
	if src("alice-session-two") == a {
		t.Fatal("two sessions share an address")
	}

	// the same session over SOCKS5 lands on the same address
	s := dialSOCKS(t, addr, methodUserPass)
	s.method(t)
	s.login(t, "alice-session-one", "pw")
	s.connect(t, echoServer(t), "x")
	got, _ := io.ReadAll(s.br)
	if sa, _, _ := strings.Cut(string(got), "|"); sa != a {
		t.Fatalf("socks5 session used %s, http used %s", sa, a)
	}
}

func TestHTTPAuth(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.Auth.Username, c.Auth.Password = "u", "p"
	})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			http.Error(w, "credentials leaked upstream", 500)
		}
	}))
	defer origin.Close()

	res, err := proxyClient(addr, "", "").Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 407 || res.Header.Get("Proxy-Authenticate") == "" {
		t.Fatalf("no credentials: %d", res.StatusCode)
	}
	if code, _ := get(t, proxyClient(addr, "u", "x"), origin.URL); code != 407 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, body := get(t, proxyClient(addr, "u", "p"), origin.URL); code != 200 {
		t.Fatalf("right password: %d %s", code, body)
	}
}

func TestBlockPrivate(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) { c.BlockPrivate = true })
	origin := peerServer(t)
	if code, _ := get(t, proxyClient(addr, "", ""), origin.URL); code != 403 {
		t.Fatalf("loopback target: %d, want 403", code)
	}
	mapped := strings.Replace(origin.URL, "127.0.0.1", "[::ffff:127.0.0.1]", 1)
	if code, _ := get(t, proxyClient(addr, "", ""), mapped); code != 403 {
		t.Fatalf("mapped loopback target: %d, want 403", code)
	}
}

// With an IPv4 test pool, an IPv6 target is the "other family" and is refused
// unless ipv4_targets is direct; production is the same with the families swapped.
func TestOtherFamilyRefused(t *testing.T) {
	addr, _ := startProxy(t, nil)
	if code, _ := get(t, proxyClient(addr, "", ""), "http://[2001:db8::1]:80/"); code != 502 {
		t.Fatalf("other-family target: %d, want 502", code)
	}
}

func TestSchemeAndHeaderLimits(t *testing.T) {
	addr, _ := startProxy(t, nil)
	send := func(raw string) int {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(c, raw)
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	if code := send("GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"); code != 400 {
		t.Fatalf("https:// without CONNECT: %d", code)
	}
	big := "GET http://example.com/ HTTP/1.1\r\nX-Big: " + strings.Repeat("a", 70<<10) + "\r\n\r\n"
	if code := send(big); code != 431 {
		t.Fatalf("huge header: %d", code)
	}
	if code := send("GET http://example.com/ HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n"); code != 400 {
		t.Fatalf("conflicting content-length: %d", code)
	}
}

func TestDeletedHeaders(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.DeletedHeaders = []string{"X-Forwarded-For"}
	})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Keep"))
	}))
	defer origin.Close()

	req, _ := http.NewRequest("GET", origin.URL, nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Keep", "yes")
	res, err := proxyClient(addr, "", "").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "|yes" {
		t.Fatalf("upstream saw %q", b)
	}
}

func TestAllowIPs(t *testing.T) {
	addr, _ := startProxy(t, func(c *config.Config) {
		c.Allow = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	origin := peerServer(t)
	if _, err := proxyClient(addr, "", "").Get(origin.URL); err == nil {
		t.Fatal("client outside allow_ips was served")
	}

	addr, _ = startProxy(t, func(c *config.Config) {
		c.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	})
	if code, _ := get(t, proxyClient(addr, "", ""), origin.URL); code != 200 {
		t.Fatalf("allowed client: %d", code)
	}
}

func TestMaxConnections(t *testing.T) {
	addr, srv := startProxy(t, func(c *config.Config) { c.MaxConnections = 1 })
	target := echoServer(t)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	fmt.Fprintf(first, "CONNECT %s HTTP/1.1\r\n\r\n", target)
	http.ReadResponse(bufio.NewReader(first), &http.Request{Method: "CONNECT"})

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetDeadline(time.Now().Add(2 * time.Second))
	if n, _ := second.Read(make([]byte, 1)); n != 0 {
		t.Fatal("second connection was served")
	}
	if got := srv.conns.Load(); got != 1 {
		t.Fatalf("conns = %d, want 1", got)
	}
}

func TestReload(t *testing.T) {
	addr, srv := startProxy(t, nil)
	origin := peerServer(t)

	next := testConfig()
	next.Prefixes = []netip.Prefix{netip.MustParsePrefix("127.88.0.0/16")}
	next.Auth.Username, next.Auth.Password = "u", "p"
	if err := srv.Reload(next); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, proxyClient(addr, "", ""), origin.URL); code != 407 {
		t.Fatalf("auth added on reload not enforced: %d", code)
	}
	_, body := get(t, proxyClient(addr, "u", "p"), origin.URL)
	if a, _ := netip.ParseAddr(body); !next.Prefixes[0].Contains(a) {
		t.Fatalf("source %s not from the reloaded prefix", body)
	}
}

func TestShutdownClosesLongTunnels(t *testing.T) {
	addr, srv := startProxy(t, nil)
	target := echoServer(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n\r\n", target)
	http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})

	done := make(chan struct{})
	go func() {
		srv.Shutdown(200 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung on an open tunnel")
	}
}

func TestBasicCredentials(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("a-session-x:p:q"))
	u, p := basicCredentials([]byte("basic " + enc))
	if u != "a-session-x" || p != "p:q" {
		t.Fatalf("got %q %q", u, p)
	}
	if u, _ := basicCredentials([]byte("Bearer abc")); u != "" {
		t.Fatal("non-basic scheme parsed")
	}
}

func TestIsPrivate(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"fe80::1", "fc00::1", "fd00:ec2::254", "0.0.0.0", "::", "100.64.0.1", "::ffff:10.0.0.1",
		"64:ff9b::a9fe:a9fe", "ff02::1", "224.0.0.1",
	} {
		if !isPrivate(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700::1111", "2a00:1450:4001:80b::200e", "64:ff9b::808:808"} {
		if isPrivate(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestConcurrentLoad(t *testing.T) {
	addr, _ := startProxy(t, nil)
	origin := peerServer(t)
	c := proxyClient(addr, "", "")

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Get(origin.URL)
			if err != nil {
				errs <- err
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func BenchmarkHTTPRequest(b *testing.B) {
	cfg := testConfig()
	srv, _ := New(cfg)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Shutdown(time.Second)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer origin.Close()

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	br := bufio.NewReader(c)
	req := []byte(fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: x\r\n\r\n", origin.URL))
	b.ReportAllocs()
	for b.Loop() {
		c.Write(req)
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}

func BenchmarkTunnelThroughput(b *testing.B) {
	cfg := testConfig()
	srv, _ := New(cfg)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Shutdown(time.Second)

	sink, _ := net.Listen("tcp", "127.0.0.1:0")
	defer sink.Close()
	go func() {
		c, err := sink.Accept()
		if err == nil {
			io.Copy(io.Discard, c)
			c.Close()
		}
	}()

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n\r\n", sink.Addr())
	http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})

	chunk := make([]byte, 64<<10)
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
}

// Addresses in the bind prefixes are routed to lo, so they'd reach this
// machine's own services; the proxy's own port would loop.
func TestSelfTargetsRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ListenPort = ln.Addr().(*net.TCPAddr).Port
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(time.Second)
	addr := ln.Addr().String()

	origin := peerServer(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	c := proxyClient(addr, "", "")
	if code, _ := get(t, c, "http://127.77.1.1:"+port+"/"); code != 403 {
		t.Fatalf("target inside the pool: %d, want 403", code)
	}
	if code, _ := get(t, c, "http://"+addr+"/"); code != 403 {
		t.Fatalf("proxy's own port: %d, want 403", code)
	}
	if code, _ := get(t, c, origin.URL); code != 200 {
		t.Fatalf("normal target: %d", code)
	}
}
