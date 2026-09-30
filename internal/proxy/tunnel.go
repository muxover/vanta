package proxy

import (
	"context"
	"strconv"

	"github.com/muxover/vanta/internal/metrics"
	"github.com/muxover/vanta/internal/parse"
)

var established = []byte("HTTP/1.1 200 Connection established\r\n\r\n")

func (cl *client) serveConnect(req *parse.Request) {
	user, pass := basicCredentials(req.ProxyAuth)
	session, err := cl.st.authorize(user, pass)
	if err != nil {
		cl.debug().Msg("auth failed")
		cl.reply(407)
		return
	}
	metrics.Requests.Add(1)

	port, _ := strconv.ParseUint(string(req.Port), 10, 16)
	up, err := cl.st.connect(context.Background(), string(req.Host), uint16(port), session)
	if err != nil {
		cl.debug().Err(err).Bytes("host", req.Host).Msg("connect failed")
		cl.reply(errStatus(err))
		return
	}
	uc := newIdleConn(up, cl.st.idle)
	defer uc.Close()

	if _, err := cl.c.Write(established); err != nil {
		return
	}
	// clients may send the TLS hello right behind the CONNECT; it's in cl.br
	out, in := relay(cl.c, cl.br, uc, nil)
	cl.debug().Bytes("host", req.Host).Int64("out", out).Int64("in", in).Msg("tunnel")
}
