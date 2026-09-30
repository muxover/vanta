package parse

import (
	"bufio"
	"bytes"
	"strconv"
)

type Response struct {
	Code          int
	ContentLength int64 // -1 when absent
	Chunked       bool
	CloseDelim    bool // body ends when the server closes the connection
	Upgrade       []byte
}

// out gets the head minus connection headers and the blank line; the caller
// adds its own Connection header
func ReadResponseHead(br *bufio.Reader, out *bytes.Buffer, scratch *[]byte) (Response, error) {
	res := Response{ContentLength: -1}
	*scratch = (*scratch)[:0]

	sl, err := readLine(br, scratch)
	if err != nil {
		return res, err
	}
	status := (*scratch)[sl.start:sl.end]
	if len(status) < 12 || !bytes.HasPrefix(status, []byte("HTTP/1.")) || status[8] != ' ' {
		return res, ErrMalformed
	}
	code, err := strconv.Atoi(string(status[9:12]))
	if err != nil || code < 100 || code > 999 {
		return res, ErrMalformed
	}
	res.Code = code
	http10 := status[7] == '0'

	type span struct{ start, end, colon int }
	var spans [maxHeaders]span
	n := 0
	var hop [][]byte
	hasTE := false
	for {
		l, err := readLine(br, scratch)
		if err != nil {
			return res, err
		}
		if l.len() == 0 {
			break
		}
		if n == maxHeaders {
			return res, ErrTooLarge
		}
		b := (*scratch)[l.start:l.end]
		i := bytes.IndexByte(b, ':')
		if i <= 0 {
			return res, ErrMalformed
		}
		spans[n] = span{l.start, l.end, l.start + i}
		n++
	}
	// scratch is stable from here on
	status = (*scratch)[sl.start:sl.end]
	if http10 {
		// answer as 1.1: the proxy speaks 1.1 to the client either way
		out.WriteString("HTTP/1.1")
		out.Write(status[8:])
	} else {
		out.Write(status)
	}
	out.WriteString("\r\n")

	for _, s := range spans[:n] {
		k := (*scratch)[s.start:s.colon]
		v0, v1 := trim(*scratch, s.colon+1, s.end)
		v := (*scratch)[v0:v1]
		if eq(k, "transfer-encoding") {
			hasTE = true
		}
		if eq(k, "connection") || eq(k, "proxy-connection") {
			for _, tok := range bytes.Split(v, []byte(",")) {
				if tok = bytes.TrimSpace(tok); len(tok) > 0 && !eq(tok, "close") && !eq(tok, "keep-alive") {
					hop = append(hop, tok)
				}
			}
		}
	}

	for _, s := range spans[:n] {
		k := (*scratch)[s.start:s.colon]
		v0, v1 := trim(*scratch, s.colon+1, s.end)
		v := (*scratch)[v0:v1]
		switch {
		case eq(k, "connection"), eq(k, "proxy-connection"), eq(k, "keep-alive"):
			continue
		case eq(k, "transfer-encoding"):
			if eq(lastToken(v), "chunked") {
				res.Chunked = true
			} else {
				res.CloseDelim = true
			}
		case eq(k, "content-length"):
			if hasTE {
				// RFC 9112 6.3: Transfer-Encoding overrides Content-Length
				continue
			}
			cl, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil || cl < 0 || (res.ContentLength >= 0 && cl != res.ContentLength) {
				return res, ErrMalformed
			}
			res.ContentLength = cl
		case eq(k, "upgrade"):
			res.Upgrade = v
			if code != 101 {
				continue
			}
		}
		if code != 101 && listed(hop, k) {
			continue
		}
		out.Write((*scratch)[s.start:s.end])
		out.WriteString("\r\n")
	}

	if res.Chunked || res.CloseDelim {
		res.ContentLength = -1
	} else if res.ContentLength < 0 {
		res.CloseDelim = true
	}
	return res, nil
}

// RFC 9112 6.3
func (r Response) NoBody(head bool) bool {
	return head || r.Code < 200 || r.Code == 204 || r.Code == 304
}

func listed(hop [][]byte, k []byte) bool {
	for _, h := range hop {
		if bytes.EqualFold(h, k) {
			return true
		}
	}
	return false
}
