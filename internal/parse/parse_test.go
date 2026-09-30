package parse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func read(t *testing.T, raw string) (*Request, error) {
	t.Helper()
	return ReadRequest(bufio.NewReaderSize(strings.NewReader(raw), 64))
}

func written(t *testing.T, r *Request, drop ...string) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.WriteHeader(&b, drop); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAbsoluteForm(t *testing.T) {
	r, err := read(t, "GET http://example.com:8080/a/b?q=1 HTTP/1.1\r\nHost: evil.test\r\nUser-Agent: x\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Host) != "example.com" || string(r.Port) != "8080" || string(r.Path) != "/a/b?q=1" {
		t.Fatalf("host %q port %q path %q", r.Host, r.Port, r.Path)
	}
	out := written(t, r)
	want := "GET /a/b?q=1 HTTP/1.1\r\nUser-Agent: x\r\nHost: example.com:8080\r\nConnection: close\r\n\r\n"
	if out != want {
		t.Fatalf("wrote\n%q\nwant\n%q", out, want)
	}
}

func TestTargets(t *testing.T) {
	cases := []struct{ raw, host, port, path string }{
		{"GET http://example.com HTTP/1.1\r\n\r\n", "example.com", "80", "/"},
		{"GET http://example.com?x HTTP/1.1\r\n\r\n", "example.com", "80", "/?x"},
		{"GET HTTP://user:pw@example.com/ HTTP/1.1\r\n\r\n", "example.com", "80", "/"},
		{"GET http://[2001:db8::1]:8080/x HTTP/1.1\r\n\r\n", "2001:db8::1", "8080", "/x"},
		{"GET /origin HTTP/1.1\r\nHost: example.com:81\r\n\r\n", "example.com", "81", "/origin"},
		{"CONNECT example.com:443 HTTP/1.1\r\n\r\n", "example.com", "443", "example.com:443"},
		{"CONNECT [2001:db8::1]:443 HTTP/1.1\r\n\r\n", "2001:db8::1", "443", "[2001:db8::1]:443"},
	}
	for _, c := range cases {
		r, err := read(t, c.raw)
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if string(r.Host) != c.host || string(r.Port) != c.port || string(r.Path) != c.path {
			t.Errorf("%q: got %q %q %q", c.raw, r.Host, r.Port, r.Path)
		}
	}
}

func TestIPv6HostHeader(t *testing.T) {
	r, err := read(t, "GET http://[2001:db8::1]/ HTTP/1.1\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if out := written(t, r); !strings.Contains(out, "Host: [2001:db8::1]\r\n") {
		t.Fatalf("wrote %q", out)
	}
}

func TestMalformed(t *testing.T) {
	cases := map[string]error{
		"GET\r\n\r\n":                                                                      ErrMalformed,
		"GET http://x/ HTTP/2.0\r\n\r\n":                                                   ErrMalformed,
		"GET https://x/ HTTP/1.1\r\n\r\n":                                                  ErrScheme,
		"GET /x HTTP/1.1\r\n\r\n":                                                          ErrMalformed,
		"CONNECT example.com HTTP/1.1\r\n\r\n":                                             ErrMalformed,
		"CONNECT example.com:99999 HTTP/1.1\r\n\r\n":                                       ErrMalformed,
		"CONNECT 2001:db8::1:443 HTTP/1.1\r\n\r\n":                                         ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nNoColon\r\n\r\n":                                        ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nKey : v\r\n\r\n":                                        ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nA: b\r\n folded\r\n\r\n":                                ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nContent-Length: -1\r\n\r\n":                             ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n":         ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nTransfer-Encoding: gzip\r\n\r\n":                        ErrMalformed,
		"GET http://x/ HTTP/1.1\r\nX: " + strings.Repeat("a", MaxHeaderBytes) + "\r\n\r\n": ErrTooLarge,
		"GET http://x/ HTTP/1.1\r\n" + strings.Repeat("A: b\r\n", maxHeaders+1) + "\r\n":   ErrTooLarge,
		"GET http://x/ HTTP/1.1\r\nHost: x":                                                io.ErrUnexpectedEOF,
	}
	for raw, want := range cases {
		if _, err := read(t, raw); !errors.Is(err, want) {
			name := raw
			if len(name) > 60 {
				name = name[:60]
			}
			t.Errorf("%q: got %v, want %v", name, err, want)
		}
	}
}

func TestHopByHopAndDrop(t *testing.T) {
	r, err := read(t, "GET http://x/ HTTP/1.1\r\n"+
		"Connection: keep-alive, X-Secret\r\n"+
		"Proxy-Connection: keep-alive\r\n"+
		"Proxy-Authorization: Basic abc\r\n"+
		"Keep-Alive: 5\r\nTE: trailers\r\nX-Secret: 1\r\nX-Forwarded-For: 1.2.3.4\r\nX-Keep: yes\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.ProxyAuth) != "Basic abc" || r.Close {
		t.Fatalf("auth %q close %v", r.ProxyAuth, r.Close)
	}
	out := written(t, r, "x-forwarded-for")
	for _, bad := range []string{"Proxy-", "Keep-Alive", "TE:", "X-Secret", "X-Forwarded-For", "keep-alive"} {
		if strings.Contains(out, bad) {
			t.Errorf("forwarded %s:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "X-Keep: yes") {
		t.Errorf("dropped X-Keep:\n%s", out)
	}
}

func TestCloseAndHTTP10(t *testing.T) {
	for raw, want := range map[string]bool{
		"GET http://x/ HTTP/1.1\r\n\r\n":                                 false,
		"GET http://x/ HTTP/1.1\r\nConnection: close\r\n\r\n":            true,
		"GET http://x/ HTTP/1.0\r\n\r\n":                                 true,
		"GET http://x/ HTTP/1.0\r\nConnection: Keep-Alive\r\n\r\n":       false,
		"GET http://x/ HTTP/1.0\r\nProxy-Connection: keep-alive\r\n\r\n": false,
	} {
		r, err := read(t, raw)
		if err != nil {
			t.Fatal(err)
		}
		if r.Close != want {
			t.Errorf("%q: close %v", raw, r.Close)
		}
	}
}

func TestUpgrade(t *testing.T) {
	r, err := read(t, "GET http://x/ws HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: k\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	out := written(t, r)
	if !strings.HasSuffix(out, "Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n") || strings.Count(out, "Upgrade: websocket") != 1 {
		t.Fatalf("wrote %q", out)
	}
	if !strings.Contains(out, "Sec-WebSocket-Key: k") {
		t.Fatal("dropped the websocket key")
	}
}

func TestChunkedBody(t *testing.T) {
	raw := "POST http://x/ HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Length: 99\r\n\r\n" +
		"5;ext=1\r\nhello\r\n0\r\nTrailer: v\r\n\r\nNEXT"
	br := bufio.NewReader(strings.NewReader(raw))
	r, err := ReadRequest(br)
	if err != nil {
		t.Fatal(err)
	}
	out := written(t, r)
	if strings.Contains(out, "Content-Length") || !strings.Contains(out, "Transfer-Encoding: chunked") {
		t.Fatalf("smuggling-prone headers: %q", out)
	}
	var body bytes.Buffer
	n, err := r.CopyBody(&body, br, make([]byte, 8))
	if err != nil || n != 5 {
		t.Fatalf("n %d err %v", n, err)
	}
	if body.String() != "5;ext=1\r\nhello\r\n0\r\nTrailer: v\r\n\r\n" {
		t.Fatalf("body %q", body.String())
	}
	if rest, _ := io.ReadAll(br); string(rest) != "NEXT" {
		t.Fatalf("over-read, left %q", rest)
	}
}

func TestChunkedBad(t *testing.T) {
	for _, body := range []string{"zz\r\n", "5\r\nhel", "5\r\nhelloXX\r\n0\r\n\r\n", "5\r\nhello\r\n"} {
		_, err := CopyChunked(io.Discard, bufio.NewReader(strings.NewReader(body)), make([]byte, 8))
		if err == nil {
			t.Errorf("%q accepted", body)
		}
	}
}

func TestContentLengthBody(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("PUT http://x/ HTTP/1.1\r\nContent-Length: 3\r\n\r\nabcNEXT"))
	r, err := ReadRequest(br)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	r.CopyBody(&body, br, make([]byte, 8))
	if body.String() != "abc" {
		t.Fatalf("body %q", body.String())
	}
}

func TestLeadingEmptyLines(t *testing.T) {
	if _, err := read(t, "\r\n\r\nGET http://x/ HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestPooledRequestIsClean(t *testing.T) {
	r, _ := read(t, "POST http://x/ HTTP/1.1\r\nContent-Length: 5\r\nExpect: 100-continue\r\nProxy-Authorization: Basic a\r\n\r\n")
	Put(r)
	r2, _ := read(t, "GET http://y/ HTTP/1.1\r\n\r\n")
	if r2.ContentLength != -1 || r2.Expect100 || r2.ProxyAuth != nil || len(r2.Headers) != 0 || string(r2.Host) != "y" {
		t.Fatalf("state leaked between requests: %+v", r2)
	}
}

func TestResponseHead(t *testing.T) {
	cases := []struct {
		raw     string
		code    int
		cl      int64
		chunked bool
		delim   bool
		out     string
	}{
		{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\nX-A: 1\r\n\r\n", 200, 5, false, false,
			"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: 1\r\n"},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n", 200, -1, true, false,
			"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n"},
		{"HTTP/1.0 200 OK\r\nKeep-Alive: 5\r\n\r\n", 200, -1, false, true,
			"HTTP/1.1 200 OK\r\n"},
		{"HTTP/1.1 200 OK\r\nConnection: X-Hop\r\nX-Hop: 1\r\nX-B: 2\r\n\r\n", 200, -1, false, true,
			"HTTP/1.1 200 OK\r\nX-B: 2\r\n"},
		{"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 101, -1, false, true,
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"},
		{"HTTP/1.1 204 No Content\r\n\r\n", 204, -1, false, true, "HTTP/1.1 204 No Content\r\n"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		var scratch []byte
		res, err := ReadResponseHead(bufio.NewReaderSize(strings.NewReader(c.raw), 16), &out, &scratch)
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if res.Code != c.code || res.ContentLength != c.cl || res.Chunked != c.chunked || res.CloseDelim != c.delim {
			t.Errorf("%q: got %+v", c.raw, res)
		}
		if out.String() != c.out {
			t.Errorf("%q: wrote %q, want %q", c.raw, out.String(), c.out)
		}
	}

	for _, bad := range []string{"HTTP/1.1 2x0 OK\r\n\r\n", "SSH-2.0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n"} {
		var out bytes.Buffer
		var scratch []byte
		if _, err := ReadResponseHead(bufio.NewReader(strings.NewReader(bad)), &out, &scratch); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func BenchmarkReadRequest(b *testing.B) {
	raw := "GET http://example.com/path?q=1 HTTP/1.1\r\nHost: example.com\r\nUser-Agent: bench\r\n" +
		"Accept: */*\r\nAccept-Encoding: gzip\r\nProxy-Authorization: Basic dTpw\r\n\r\n"
	sr := strings.NewReader(raw)
	br := bufio.NewReader(sr)
	b.ReportAllocs()
	for b.Loop() {
		sr.Reset(raw)
		br.Reset(sr)
		r, err := ReadRequest(br)
		if err != nil {
			b.Fatal(err)
		}
		r.WriteHeader(io.Discard, nil)
		Put(r)
	}
}
