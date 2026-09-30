package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/parse"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var readerPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 4096) },
}

var copyBufPool = sync.Pool{
	New: func() any { b := make([]byte, 32<<10); return &b },
}

func getReader(r io.Reader) *bufio.Reader {
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

func putReader(br *bufio.Reader) {
	br.Reset(nil)
	readerPool.Put(br)
}

type client struct {
	st   *state
	c    *idleConn
	br   *bufio.Reader
	peer net.Addr
}

func (cl *client) debug() *zerolog.Event {
	return log.Debug().Stringer("peer", cl.peer)
}

func serveConn(st *state, conn net.Conn) {
	cl := &client{st: st, c: newIdleConn(conn, st.idle), peer: conn.RemoteAddr()}
	cl.br = getReader(cl.c)
	defer putReader(cl.br)

	// the first bytes get one idle window in total, however slowly they trickle in
	cl.c.hold()
	b, err := cl.br.Peek(1)
	if err != nil {
		return
	}
	switch b[0] {
	case socks5Version:
		cl.serveSOCKS5()
	case 0x04:
		cl.debug().Msg("socks4 is not supported")
	default:
		for {
			req, err := parse.ReadRequest(cl.br)
			if err != nil {
				if !errors.Is(err, io.EOF) && !isTimeout(err) {
					cl.debug().Err(err).Msg("bad request")
					if errors.Is(err, parse.ErrScheme) {
						cl.replyMsg(400, "vanta: "+err.Error())
					} else {
						cl.reply(errStatus(err))
					}
				}
				return
			}
			var keep bool
			if req.IsConnect() {
				cl.serveConnect(req)
			} else {
				keep = cl.serveHTTP(req)
			}
			parse.Put(req)
			if !keep {
				return
			}
			cl.c.hold()
		}
	}
}

// the deadline moves with traffic, so only a connection idle for a whole
// timeout is dropped
type idleConn struct {
	net.Conn
	timeout time.Duration
	last    atomic.Int64
	held    atomic.Bool
}

func newIdleConn(c net.Conn, timeout time.Duration) *idleConn {
	ic := &idleConn{Conn: c, timeout: timeout}
	ic.touch(time.Now().UnixNano())
	return ic
}

// fixed deadline for reading request headers; traffic doesn't extend it
func (c *idleConn) hold() {
	c.held.Store(true)
	c.Conn.SetDeadline(time.Now().Add(c.timeout))
}

func (c *idleConn) release() {
	c.held.Store(false)
	c.touch(time.Now().UnixNano())
}

func (c *idleConn) touch(now int64) {
	c.last.Store(now)
	c.Conn.SetDeadline(time.Unix(0, now).Add(c.timeout))
}

// refresh at most ~4x per timeout window; a SetDeadline per copy chunk is too costly
func (c *idleConn) bump() {
	if c.held.Load() {
		return
	}
	now := time.Now().UnixNano()
	last := c.last.Load()
	if now-last < int64(c.timeout)/4 {
		return
	}
	if c.last.CompareAndSwap(last, now) {
		c.Conn.SetDeadline(time.Unix(0, now).Add(c.timeout))
	}
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.bump()
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.bump()
	}
	return n, err
}

func (c *idleConn) closeWrite() {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Conn.Close()
	}
}

// anything already buffered in cbr or ubr goes first
func relay(cc *idleConn, cbr *bufio.Reader, uc *idleConn, ubr *bufio.Reader) (out, in int64) {
	cc.release()
	uc.release()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		in = pipe(cc, uc, ubr)
	}()
	out = pipe(uc, cc, cbr)
	wg.Wait()

	metrics.BytesOut.Add(out)
	metrics.BytesIn.Add(in)
	return out, in
}

// half-close on EOF so the other direction can finish; close both on error
func pipe(dst, src *idleConn, buffered *bufio.Reader) int64 {
	var n int64
	if buffered != nil && buffered.Buffered() > 0 {
		b, _ := buffered.Peek(buffered.Buffered())
		w, err := dst.Write(b)
		n += int64(w)
		if err != nil {
			dst.Close()
			src.Close()
			return n
		}
	}

	buf := copyBufPool.Get().(*[]byte)
	w, err := io.CopyBuffer(dst, src, *buf)
	copyBufPool.Put(buf)
	n += w
	if err != nil {
		dst.Close()
		src.Close()
		return n
	}
	dst.closeWrite()
	return n
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
