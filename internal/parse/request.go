package parse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"sync"
)

const (
	MaxHeaderBytes = 64 << 10
	maxHeaders     = 128
)

var (
	ErrMalformed = errors.New("malformed request")
	ErrTooLarge  = errors.New("request header too large")
	ErrScheme    = errors.New("only http:// URLs can be proxied without CONNECT")
)

type Header struct {
	Key, Val []byte
}

type Request struct {
	Method []byte
	Host   []byte
	Port   []byte
	Path   []byte

	Headers   []Header
	ProxyAuth []byte

	ContentLength int64 // -1 when absent
	Chunked       bool
	Close         bool   // client asked to close after this response
	Upgrade       []byte // Upgrade header value when Connection lists upgrade
	Expect100     bool

	hop [][]byte // header names listed in Connection
	buf []byte
}

var reqPool = sync.Pool{
	New: func() any {
		return &Request{Headers: make([]Header, 0, 24), buf: make([]byte, 0, 2048)}
	},
}

func Put(r *Request) {
	if cap(r.buf) > MaxHeaderBytes {
		return
	}
	reqPool.Put(r)
}

func (r *Request) reset() {
	*r = Request{Headers: r.Headers[:0], hop: r.hop[:0], buf: r.buf[:0], ContentLength: -1}
}

func (r *Request) IsConnect() bool { return string(r.Method) == "CONNECT" }

func (r *Request) IsHead() bool { return string(r.Method) == "HEAD" }

// slices in the result stay valid until the request goes back through Put
func ReadRequest(br *bufio.Reader) (*Request, error) {
	r := reqPool.Get().(*Request)
	r.reset()
	if err := r.read(br); err != nil {
		Put(r)
		return nil, err
	}
	return r, nil
}

func (r *Request) read(br *bufio.Reader) error {
	// the header block is copied into r.buf first; slices are taken after, since
	// appending may move it
	type span struct{ k0, k1, v0, v1 int }
	var spans [maxHeaders]span
	n := 0

	line, err := readLine(br, &r.buf)
	for err == nil && line.len() == 0 {
		// RFC 9112 2.2: ignore empty lines before the request line
		r.buf = r.buf[:0]
		line, err = readLine(br, &r.buf)
	}
	if err != nil {
		return err
	}
	reqLine := line

	for {
		l, err := readLine(br, &r.buf)
		if err != nil {
			return err
		}
		if l.len() == 0 {
			break
		}
		if n == maxHeaders {
			return ErrTooLarge
		}
		b := r.buf[l.start:l.end]
		i := bytes.IndexByte(b, ':')
		if i <= 0 || b[0] == ' ' || b[0] == '\t' || b[i-1] == ' ' {
			// no colon, obsolete line folding, or space before the colon
			return ErrMalformed
		}
		k0, k1 := l.start, l.start+i
		v0, v1 := trim(r.buf, l.start+i+1, l.end)
		spans[n] = span{k0, k1, v0, v1}
		n++
	}

	rl := r.buf[reqLine.start:reqLine.end]
	sp1 := bytes.IndexByte(rl, ' ')
	sp2 := bytes.LastIndexByte(rl, ' ')
	if sp1 <= 0 || sp2 <= sp1+1 {
		return ErrMalformed
	}
	r.Method = rl[:sp1]
	target := rl[sp1+1 : sp2]
	proto := rl[sp2+1:]
	switch string(proto) {
	case "HTTP/1.1":
	case "HTTP/1.0":
		r.Close = true
	default:
		return ErrMalformed
	}

	var host []byte
	for _, s := range spans[:n] {
		k := r.buf[s.k0:s.k1]
		v := r.buf[s.v0:s.v1]
		switch {
		case eq(k, "proxy-authorization"):
			r.ProxyAuth = v
		case eq(k, "connection"), eq(k, "proxy-connection"):
			for _, tok := range bytes.Split(v, []byte(",")) {
				tok = bytes.TrimSpace(tok)
				switch {
				case eq(tok, "close"):
					r.Close = true
				case eq(tok, "keep-alive"):
					if string(proto) == "HTTP/1.0" {
						r.Close = false
					}
				case len(tok) > 0:
					r.hop = append(r.hop, tok)
				}
			}
		case eq(k, "transfer-encoding"):
			// only chunked as the final coding is supported; anything else
			// can't be framed safely
			if !eq(lastToken(v), "chunked") {
				return ErrMalformed
			}
			r.Chunked = true
		case eq(k, "content-length"):
			cl, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil || cl < 0 || (r.ContentLength >= 0 && cl != r.ContentLength) {
				return ErrMalformed
			}
			r.ContentLength = cl
			r.Headers = append(r.Headers, Header{k, v})
		case eq(k, "expect"):
			if eq(v, "100-continue") {
				r.Expect100 = true
			}
		case eq(k, "host"):
			host = v
			r.Headers = append(r.Headers, Header{k, v})
		default:
			r.Headers = append(r.Headers, Header{k, v})
		}
	}

	for _, h := range r.hop {
		if eq(h, "upgrade") {
			for _, hd := range r.Headers {
				if eq(hd.Key, "upgrade") {
					r.Upgrade = hd.Val
				}
			}
		}
	}
	if r.Chunked && r.ContentLength >= 0 {
		// RFC 9112 6.3: chunked wins and Content-Length must go
		r.ContentLength = -1
		r.dropHeader("content-length")
	}

	return r.parseTarget(target, host)
}

func (r *Request) parseTarget(target, host []byte) error {
	if r.IsConnect() {
		h, p, ok := splitHostPort(target)
		if !ok || len(p) == 0 {
			return ErrMalformed
		}
		r.Host, r.Port, r.Path = h, p, target
		return validPort(p)
	}

	var authority []byte
	switch {
	case len(target) > 0 && target[0] == '/':
		// origin-form: the client talks to the proxy like a server
		if len(host) == 0 {
			return ErrMalformed
		}
		authority, r.Path = host, target
	default:
		i := bytes.Index(target, []byte("://"))
		if i < 0 {
			return ErrMalformed
		}
		if !eq(target[:i], "http") {
			return ErrScheme
		}
		rest := target[i+3:]
		j := bytes.IndexAny(rest, "/?#")
		if j < 0 {
			authority, r.Path = rest, []byte("/")
		} else {
			authority, r.Path = rest[:j], rest[j:]
			if r.Path[0] != '/' {
				r.Path = append([]byte("/"), r.Path...)
			}
		}
		if at := bytes.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		// RFC 9112 3.2.2: the absolute URI wins over the client's Host header
		r.dropHeader("host")
	}

	h, p, ok := splitHostPort(authority)
	if !ok || len(h) == 0 {
		return ErrMalformed
	}
	if len(p) == 0 {
		p = []byte("80")
	}
	r.Host, r.Port = h, p
	return validPort(p)
}

func (r *Request) dropHeader(name string) {
	out := r.Headers[:0]
	for _, h := range r.Headers {
		if !eq(h.Key, name) {
			out = append(out, h)
		}
	}
	r.Headers = out
}

func (r *Request) isHop(key []byte) bool {
	switch {
	case eq(key, "keep-alive"), eq(key, "te"), eq(key, "trailer"), eq(key, "upgrade"),
		eq(key, "proxy-authenticate"):
		return true
	}
	for _, h := range r.hop {
		if bytes.EqualFold(h, key) {
			return true
		}
	}
	return false
}

var writeBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, 4096)) },
}

// drop holds lowercase header names to strip
func (r *Request) WriteHeader(w io.Writer, drop []string) error {
	buf := writeBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer writeBufPool.Put(buf)

	buf.Write(r.Method)
	buf.WriteByte(' ')
	buf.Write(r.Path)
	buf.WriteString(" HTTP/1.1\r\n")

	hasHost := false
	for _, h := range r.Headers {
		if r.isHop(h.Key) || dropped(h.Key, drop) {
			continue
		}
		if eq(h.Key, "host") {
			hasHost = true
		}
		buf.Write(h.Key)
		buf.WriteString(": ")
		buf.Write(h.Val)
		buf.WriteString("\r\n")
	}
	if !hasHost {
		buf.WriteString("Host: ")
		if bytes.IndexByte(r.Host, ':') >= 0 {
			buf.WriteByte('[')
			buf.Write(r.Host)
			buf.WriteByte(']')
		} else {
			buf.Write(r.Host)
		}
		if string(r.Port) != "80" {
			buf.WriteByte(':')
			buf.Write(r.Port)
		}
		buf.WriteString("\r\n")
	}
	if r.Chunked {
		buf.WriteString("Transfer-Encoding: chunked\r\n")
	}
	if len(r.Upgrade) > 0 {
		buf.WriteString("Connection: Upgrade\r\nUpgrade: ")
		buf.Write(r.Upgrade)
		buf.WriteString("\r\n\r\n")
	} else {
		buf.WriteString("Connection: close\r\n\r\n")
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (r *Request) CopyBody(w io.Writer, br *bufio.Reader, buf []byte) (int64, error) {
	switch {
	case r.Chunked:
		return CopyChunked(w, br, buf)
	case r.ContentLength > 0:
		return io.CopyBuffer(w, io.LimitReader(br, r.ContentLength), buf)
	}
	return 0, nil
}

func CopyChunked(w io.Writer, br *bufio.Reader, buf []byte) (int64, error) {
	var total int64
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return total, chunkErr(err)
		}
		if _, err := w.Write(line); err != nil {
			return total, err
		}
		size := bytes.TrimSpace(line)
		if i := bytes.IndexByte(size, ';'); i >= 0 {
			size = size[:i]
		}
		n, err := strconv.ParseUint(string(bytes.TrimSpace(size)), 16, 62)
		if err != nil {
			return total, ErrMalformed
		}
		if n == 0 {
			// trailer section, ends with an empty line
			for {
				line, err := br.ReadSlice('\n')
				if err != nil {
					return total, chunkErr(err)
				}
				if _, err := w.Write(line); err != nil {
					return total, err
				}
				if len(bytes.TrimRight(line, "\r\n")) == 0 {
					return total, nil
				}
			}
		}
		c, err := io.CopyBuffer(w, io.LimitReader(br, int64(n)), buf)
		total += c
		if err != nil {
			return total, err
		}
		if c < int64(n) {
			return total, io.ErrUnexpectedEOF
		}
		crlf, err := br.ReadSlice('\n')
		if err != nil {
			return total, chunkErr(err)
		}
		if len(bytes.TrimRight(crlf, "\r\n")) != 0 {
			return total, ErrMalformed
		}
		if _, err := w.Write(crlf); err != nil {
			return total, err
		}
	}
}

func chunkErr(err error) error {
	if errors.Is(err, bufio.ErrBufferFull) {
		return ErrMalformed
	}
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

type lineSpan struct{ start, end int }

func (l lineSpan) len() int { return l.end - l.start }

// appends the line to *buf and returns where it sits, without the CRLF
func readLine(br *bufio.Reader, buf *[]byte) (lineSpan, error) {
	start := len(*buf)
	for {
		frag, err := br.ReadSlice('\n')
		if len(*buf)+len(frag) > MaxHeaderBytes {
			return lineSpan{}, ErrTooLarge
		}
		*buf = append(*buf, frag...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if err == io.EOF && len(*buf) > start {
				err = io.ErrUnexpectedEOF
			}
			return lineSpan{}, err
		}
	}
	end := len(*buf) - 1
	if end > start && (*buf)[end-1] == '\r' {
		end--
	}
	return lineSpan{start, end}, nil
}

func trim(b []byte, i, j int) (int, int) {
	for i < j && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t') {
		j--
	}
	return i, j
}

func lastToken(v []byte) []byte {
	if i := bytes.LastIndexByte(v, ','); i >= 0 {
		v = v[i+1:]
	}
	return bytes.TrimSpace(v)
}

func dropped(key []byte, drop []string) bool {
	for _, d := range drop {
		if eq(key, d) {
			return true
		}
	}
	return false
}

// lower must already be lowercase
func eq(b []byte, lower string) bool {
	if len(b) != len(lower) {
		return false
	}
	for i := range len(b) {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func splitHostPort(hp []byte) (host, port []byte, ok bool) {
	if len(hp) > 0 && hp[0] == '[' {
		end := bytes.IndexByte(hp, ']')
		if end < 0 {
			return nil, nil, false
		}
		host, rest := hp[1:end], hp[end+1:]
		switch {
		case len(rest) == 0:
			return host, nil, true
		case rest[0] == ':':
			return host, rest[1:], true
		}
		return nil, nil, false
	}
	i := bytes.LastIndexByte(hp, ':')
	if i < 0 {
		return hp, nil, true
	}
	if bytes.IndexByte(hp[:i], ':') >= 0 {
		// bare IPv6 without brackets
		return nil, nil, false
	}
	return hp[:i], hp[i+1:], true
}

func validPort(p []byte) error {
	n, err := strconv.Atoi(string(p))
	if err != nil || n < 1 || n > 65535 {
		return ErrMalformed
	}
	return nil
}
