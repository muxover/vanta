package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/parse"
	"github.com/muxover/vanta/internal/resolve"
)

type headBuf struct {
	out     bytes.Buffer
	scratch []byte
}

var headPool = sync.Pool{
	New: func() any { return &headBuf{scratch: make([]byte, 0, 2048)} },
}

var continue100 = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// reports whether the client connection can take another request
func (cl *client) serveHTTP(req *parse.Request) bool {
	user, pass := basicCredentials(req.ProxyAuth)
	session, err := cl.st.authorize(user, pass)
	if err != nil {
		cl.debug().Msg("auth failed")
		cl.reply(407)
		return false
	}
	metrics.Requests.Add(1)

	port, _ := strconv.ParseUint(string(req.Port), 10, 16)
	up, err := cl.st.connect(context.Background(), string(req.Host), uint16(port), session)
	if err != nil {
		cl.debug().Err(err).Bytes("host", req.Host).Msg("connect failed")
		cl.reply(errStatus(err))
		return false
	}
	uc := newIdleConn(up, cl.st.idle)
	defer uc.Close()

	cl.c.release()
	if req.Expect100 {
		// the Expect header isn't forwarded, so answer it here
		if _, err := cl.c.Write(continue100); err != nil {
			return false
		}
	}

	buf := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(buf)

	if err := req.WriteHeader(uc, cl.st.drop); err != nil {
		cl.reply(502)
		return false
	}
	sent, err := req.CopyBody(uc, cl.br, *buf)
	metrics.BytesOut.Add(sent)
	if err != nil {
		cl.debug().Err(err).Msg("request body")
		if errors.Is(err, parse.ErrMalformed) {
			cl.reply(400)
		}
		return false
	}

	ubr := getReader(uc)
	defer putReader(ubr)
	hb := headPool.Get().(*headBuf)
	defer headPool.Put(hb)

	for {
		hb.out.Reset()
		res, err := parse.ReadResponseHead(ubr, &hb.out, &hb.scratch)
		if err != nil {
			cl.debug().Err(err).Msg("upstream response")
			cl.reply(502)
			return false
		}

		if res.Code == 101 {
			if len(req.Upgrade) == 0 {
				cl.reply(502)
				return false
			}
			hb.out.WriteString("Connection: Upgrade\r\n\r\n")
			if _, err := cl.c.Write(hb.out.Bytes()); err != nil {
				return false
			}
			relay(cl.c, cl.br, uc, ubr)
			return false
		}
		if res.Code < 200 {
			hb.out.WriteString("\r\n")
			if _, err := cl.c.Write(hb.out.Bytes()); err != nil {
				return false
			}
			continue
		}

		noBody := res.NoBody(req.IsHead())
		keep := !req.Close && (noBody || !res.CloseDelim)
		if keep {
			hb.out.WriteString("Connection: keep-alive\r\n\r\n")
		} else {
			hb.out.WriteString("Connection: close\r\n\r\n")
		}
		if _, err := cl.c.Write(hb.out.Bytes()); err != nil {
			return false
		}
		if noBody {
			return keep
		}

		var n int64
		switch {
		case res.Chunked:
			n, err = parse.CopyChunked(cl.c, ubr, *buf)
		case res.ContentLength >= 0:
			n, err = io.CopyBuffer(cl.c, io.LimitReader(ubr, res.ContentLength), *buf)
			if err == nil && n < res.ContentLength {
				err = io.ErrUnexpectedEOF
			}
		default:
			n, err = io.CopyBuffer(cl.c, ubr, *buf)
		}
		metrics.BytesIn.Add(n)
		if err != nil {
			cl.debug().Err(err).Msg("response body")
			return false
		}
		return keep
	}
}

var statusText = map[int]string{
	400: "Bad Request",
	403: "Forbidden",
	407: "Proxy Authentication Required",
	431: "Request Header Fields Too Large",
	502: "Bad Gateway",
	504: "Gateway Timeout",
}

var reasons = map[int]string{
	400: "vanta: malformed request",
	403: "vanta: destination not allowed",
	407: "vanta: proxy authentication required",
	431: "vanta: request header too large",
	502: "vanta: could not reach the destination",
	504: "vanta: destination timed out",
}

func (cl *client) reply(code int) {
	cl.replyMsg(code, reasons[code])
}

func (cl *client) replyMsg(code int, body string) {
	extra := ""
	if code == 407 {
		extra = "Proxy-Authenticate: Basic realm=\"vanta\"\r\n"
	}
	fmt.Fprintf(cl.c, "HTTP/1.1 %d %s\r\n%sContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, statusText[code], extra, len(body)+1, body+"\n")
}

func errStatus(err error) int {
	switch {
	case errors.Is(err, parse.ErrTooLarge):
		return 431
	case errors.Is(err, parse.ErrMalformed), errors.Is(err, parse.ErrScheme):
		return 400
	case errors.Is(err, errBlocked):
		return 403
	case errors.Is(err, errFamily), errors.Is(err, resolve.ErrNoAddress):
		return 502
	case isTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return 504
	}
	return 502
}
