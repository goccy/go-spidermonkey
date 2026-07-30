package wpt

// permissive.go: accepting the requests net/http's server will not.
//
// A header value may hold any byte but NUL, LF and CR as far as fetch is
// concerned, and the suite checks that such a value reaches the server and is
// echoed back. net/http's server rejects it on arrival — readRequest runs the
// same field-vchar check the client transport does, and answers 400 before any
// handler sees the request. The suite's own Python server accepts it.
//
// So the harness has to as well, or it measures its own strictness rather than
// the runtime's. The listener below reads each request's head, and hands the
// connection to net/http unless net/http would refuse it; in that case it
// answers the request itself, through the same handler table, writing the
// response bytes directly so the echoed value survives the response path too.
//
// This is harness code, and it exists only because the harness stands in for a
// server that is more permissive than Go's. The runtime's own behaviour is
// pinned separately, in compat/web.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// permissiveListener wraps a listener so each connection is inspected before
// net/http sees it.
type permissiveListener struct {
	net.Listener
	serve func(conn net.Conn, req *http.Request, head []byte) bool
}

func (l *permissiveListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &peekConn{Conn: conn, serve: l.serve}, nil
}

// peekConn reads the request head on the first Read, decides who should answer
// it, and then either replays the head to net/http or takes the connection over.
type peekConn struct {
	net.Conn
	serve  func(conn net.Conn, req *http.Request, head []byte) bool
	once   sync.Once
	prefix io.Reader
	taken  bool
}

func (c *peekConn) Read(p []byte) (int, error) {
	c.once.Do(c.inspect)
	if c.taken {
		// This connection has been answered already; net/http must see it end.
		return 0, io.EOF
	}
	if c.prefix != nil {
		n, err := c.prefix.Read(p)
		if err == io.EOF {
			c.prefix = nil
			if n > 0 {
				return n, nil
			}
			return c.Conn.Read(p)
		}
		return n, err
	}
	return c.Conn.Read(p)
}

// inspect reads up to the end of the request head. Only the head is read: the
// body stays on the connection for whoever answers.
func (c *peekConn) inspect() {
	// A TLS ClientHello starts with 0x16, and anything that is not an HTTP method
	// is not a request head to look for. Reading on regardless meant a TLS
	// connection blocked here waiting for a blank line that would never come.
	first, err := c.peekByte()
	if err != nil {
		return
	}
	if first < 'A' || first > 'Z' {
		// Not a request head. The byte has been consumed, so it is replayed:
		// net/http must see the connection exactly as it arrived.
		c.prefix = bytes.NewReader([]byte{first})
		return
	}
	head, err := readHead(c.Conn)
	head = append([]byte{first}, head...)
	if err != nil {
		c.prefix = bytes.NewReader(head)
		return
	}
	req, perr := parseHead(head)
	if perr != nil || !headerNeedsPermissiveServer(req.Header) {
		c.prefix = bytes.NewReader(head)
		return
	}
	if c.serve(c.Conn, req, head) {
		c.taken = true
		return
	}
	c.prefix = bytes.NewReader(head)
}

// peekByte reads the first byte of the connection, holding it so the head can be
// reassembled. A short deadline keeps a connection that sends nothing from
// stalling here; on timeout the connection simply goes to net/http untouched.
func (c *peekConn) peekByte() (byte, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.Conn.SetReadDeadline(time.Time{}) }()
	var b [1]byte
	if _, err := io.ReadFull(c.Conn, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// readHead reads bytes up to and including the blank line that ends the head.
func readHead(r io.Reader) ([]byte, error) {
	var head []byte
	buf := make([]byte, 1)
	for len(head) < 64<<10 {
		n, err := r.Read(buf)
		if n > 0 {
			head = append(head, buf[0])
			if bytes.HasSuffix(head, []byte("\r\n\r\n")) || bytes.HasSuffix(head, []byte("\n\n")) {
				return head, nil
			}
		}
		if err != nil {
			return head, err
		}
	}
	return head, fmt.Errorf("request head too long")
}

// parseHead reads the request line and headers by hand. net/textproto cannot be
// used for this: ReadMIMEHeader validates field values too, and rejects the very
// bytes this path exists to accept.
func parseHead(head []byte) (*http.Request, error) {
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty request")
	}
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("bad request line %q", lines[0])
	}
	h := http.Header{}
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("bad header line %q", line)
		}
		// Only the optional whitespace around the value is removed; whatever else
		// the value holds is the value.
		h.Add(strings.TrimSpace(name), strings.Trim(value, " \t"))
	}
	req, err := http.NewRequest(parts[0], "http://"+h.Get("Host")+parts[1], nil)
	if err != nil {
		return nil, err
	}
	req.Header = h
	req.Host = h.Get("Host")
	return req, nil
}

// headerNeedsPermissiveServer reports whether net/http would answer 400 rather
// than let a handler see this request.
func headerNeedsPermissiveServer(h http.Header) bool {
	for _, vv := range h {
		for _, v := range vv {
			for i := 0; i < len(v); i++ {
				if b := v[i]; (b < 0x20 || b == 0x7f) && b != '\t' {
					return true
				}
			}
		}
	}
	return false
}

// permissiveWriter collects a handler's response so it can be written without
// net/http's own field-value filtering — the echoed value has to survive the way
// out as well as the way in.
type permissiveWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *permissiveWriter) Header() http.Header { return w.header }
func (w *permissiveWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}
func (w *permissiveWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

// writeTo writes the response in wire format. Header values go out verbatim
// apart from CR and LF, which cannot be represented in a field at all.
func (w *permissiveWriter) writeTo(conn net.Conn) error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", w.status, http.StatusText(w.status))
	names := make([]string, 0, len(w.header))
	for k := range w.header {
		names = append(names, k)
	}
	sort.Strings(names)
	sanitize := strings.NewReplacer("\r", " ", "\n", " ")
	for _, k := range names {
		for _, v := range w.header[k] {
			fmt.Fprintf(&b, "%s: %s\r\n", k, sanitize.Replace(v))
		}
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\nConnection: close\r\n\r\n", w.body.Len())
	b.Write(w.body.Bytes())
	_, err := conn.Write(b.Bytes())
	return err
}
