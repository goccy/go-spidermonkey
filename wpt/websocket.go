package wpt

// websocket.go: the suite's own WebSocket fixtures, ported from
// websockets/handlers/*_wsh.py.
//
// Those are pywebsocket handlers: a `web_socket_do_extra_handshake` that may
// select a subprotocol or refuse the request, and a `web_socket_transfer_data`
// that reads and writes messages. Neither is more than a few lines, and they are
// checked out alongside this file, so a port can be read against its original.
//
// The endpoints are addressed by the path alone — the suite's WebSocket URLs
// have no query string — and every path with no handler here is served as a
// file, so an unported fixture fails as a handshake that did not upgrade rather
// than as a connection to something that answers wrongly.
//
// The close handshake is not written out by hand: the library echoes a peer's
// close code and reason automatically, including the empty frame that a close
// with no code must be answered with. That is exactly what echo_wsh.py's
// web_socket_passive_closing_handshake does.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// goodbye ends the echo handler's loop, as _GOODBYE_MESSAGE does in the
// original.
const goodbye = "Goodbye"

// wsHandler serves one already-upgraded connection.
type wsHandler func(ctx context.Context, r *http.Request, c *websocket.Conn)

// wsEndpoint is one fixture: what it negotiates during the handshake, and what
// it does afterwards.
type wsEndpoint struct {
	// subprotocols the handler will select from, in preference order.
	subprotocols []string
	// beforeAccept runs before the upgrade. It returns false to refuse the
	// request, having written the response itself.
	beforeAccept func(w http.ResponseWriter, r *http.Request) bool
	serve        wsHandler
}

var wsEndpoints = map[string]wsEndpoint{
	// echo_wsh.py: echo every message back with its own type, and stop at
	// "Goodbye".
	"echo": {subprotocols: []string{"echo"}, serve: echoWS},
	// echo_exit_wsh.py: read but never reply, and stop at "Goodbye".
	"echo_exit": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && string(data) == goodbye {
				return
			}
		}
	}},
	// handshake_no_extensions_wsh.py: accept, negotiate no extension, and hold
	// the connection open without reading. Extensions are never offered by this
	// server, so accepting is the whole of it.
	"handshake_no_extensions": {serve: holdWS},
	// handshake_sleep_2_wsh.py: accept only after two seconds.
	"handshake_sleep_2": {
		beforeAccept: func(w http.ResponseWriter, r *http.Request) bool {
			time.Sleep(2 * time.Second)
			return true
		},
		serve: holdWS,
	},
	// origin_wsh.py: report the Origin the client sent.
	"origin": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageText, []byte(r.Header.Get("Origin")))
		holdWS(ctx, r, c)
	}},
	// referrer_wsh.py: report the Referer, or that there was none.
	"referrer": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		referrer := r.Header.Get("Referer")
		if referrer == "" {
			referrer = "MISSING AS PER FETCH"
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(referrer))
		holdWS(ctx, r, c)
	}},
	// basic_auth_wsh.py: username "foo", password "bar", and a 401 otherwise.
	"basic_auth": {
		beforeAccept: func(w http.ResponseWriter, r *http.Request) bool {
			if r.Header.Get("Authorization") != "Basic Zm9vOmJhcg==" {
				w.Header().Set("WWW-Authenticate", `Basic realm="camelot"`)
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusUnauthorized)
				return false
			}
			return true
		},
		serve: holdWS,
	},
	// receive-many-with-backpressure_wsh.py: sleep before each read so the
	// client's send queue builds up, then acknowledge with the size received.
	"receive-many-with-backpressure": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		for {
			time.Sleep(100 * time.Millisecond)
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, websocket.MessageText, []byte(fmt.Sprint(len(data)))); err != nil {
				return
			}
		}
	}},
	// delayed-passive-close_wsh.py: answer the client's close frame only after a
	// delay, so the closing state is observable.
	"delayed-passive-close": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		_, _, err := c.Read(ctx)
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			time.Sleep(500 * time.Millisecond)
		}
	}},
	// remote-close_wsh.py: close from this end as soon as the connection is up.
	"remote-close": {serve: func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}},
}

// echoWS is echo_wsh.py's transfer loop.
func echoWS(ctx context.Context, r *http.Request, c *websocket.Conn) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
		if typ == websocket.MessageText && string(data) == goodbye {
			return
		}
	}
}

// holdWS keeps the connection open until the peer ends it. A handler that
// returns would close the connection, and a test that expected to stay
// connected would see a close it did not ask for.
func holdWS(ctx context.Context, r *http.Request, c *websocket.Conn) {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

// serveWebSocket answers an upgrade request if the path names a fixture. It
// reports whether it handled the request; a path with no fixture, or a request
// that is not an upgrade, falls through to the file server.
func serveWebSocket(rel string, w http.ResponseWriter, r *http.Request) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	ep, ok := wsEndpoints[rel]
	if !ok {
		return false
	}
	if ep.beforeAccept != nil && !ep.beforeAccept(w, r) {
		return true
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: ep.subprotocols,
		// The client does not offer permessage-deflate and several fixtures
		// deliberately disable it (compression hides the backpressure two of them
		// exist to measure), so it is off everywhere rather than per endpoint.
		CompressionMode: websocket.CompressionDisabled,
		// Every test connects from a different loopback origin than it serves from
		// (localhost vs 127.0.0.1, by design — see StartServer), and the fixtures
		// that care about the origin report it rather than enforcing it.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return true
	}
	c.SetReadLimit(-1)
	defer c.CloseNow()
	ep.serve(r.Context(), r, c)
	return true
}
