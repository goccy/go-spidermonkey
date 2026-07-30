package web

// fetchraw.go: sending a request whose headers net/http's Transport refuses.
//
// A header value may hold any byte but NUL, LF and CR as far as fetch is
// concerned. net/http is stricter: Transport.roundTrip runs validateHeaders,
// which rejects every control character except HTAB, and the request never
// leaves. That is RFC 9110's field-vchar rule, and enforcing it on send is a
// deliberate choice — but it is enforced by the TRANSPORT, not by the code that
// writes the request, and Go's own server does not enforce it on receive at all
// (net/textproto trims a field value and keeps whatever is left). So a Go
// client cannot send what a Go server will happily accept.
//
// The way around it is not a hand-written HTTP implementation. Request.Write is
// public, documented as writing "an HTTP/1.1 request, which is the header and
// body, in wire format", and validates only the field NAME; ReadResponse parses
// the reply. Between them the whole exchange is standard-library code, and this
// file is the dozen lines that connect them.
//
// The raw path is taken ONLY for a request the standard path would refuse, so
// nothing in ordinary use loses connection pooling or HTTP/2 — and HTTP/2 could
// not carry such a value anyway, since its own header check is the same one.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
)

// isCTLNotLWS mirrors the rule httpguts.ValidHeaderFieldValue applies: a
// control character other than HTAB or space is what makes the Transport refuse.
func isCTLNotLWS(b byte) bool {
	return (b < 0x20 || b == 0x7f) && b != '\t' && b != ' '
}

// needsRawWrite reports whether net/http would refuse to send these headers.
func needsRawWrite(h http.Header) bool {
	for _, vv := range h {
		for _, v := range vv {
			for i := 0; i < len(v); i++ {
				if isCTLNotLWS(v[i]) {
					return true
				}
			}
		}
	}
	return false
}

// permissiveTransport delegates to the standard Transport, and falls back to
// writing the request over a connection of its own for the requests the standard
// Transport will not send.
type permissiveTransport struct {
	std  *http.Transport
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (t *permissiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !needsRawWrite(req.Header) {
		return t.std.RoundTrip(req)
	}
	return t.roundTripRaw(req)
}

// closingBody ties the response body's lifetime to the connection's, since this
// path has no pool to return it to.
type closingBody struct {
	*bufio.Reader
	conn net.Conn
	body interface{ Close() error }
}

func (b *closingBody) Read(p []byte) (int, error) { return b.Reader.Read(p) }

func (b *closingBody) Close() error {
	if b.body != nil {
		_ = b.body.Close()
	}
	return b.conn.Close()
}

func (t *permissiveTransport) roundTripRaw(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := t.dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tc
	}
	// The context has no transport to cancel through here, so it cancels by
	// closing the connection — which is what makes an aborted fetch return
	// promptly rather than waiting on the peer.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	resp.Body = &closingBody{Reader: bufio.NewReader(resp.Body), conn: conn, body: resp.Body}
	return resp, nil
}
