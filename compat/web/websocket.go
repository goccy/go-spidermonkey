package web

// websocket.go: the Go half of WebSocket (https://websockets.spec.whatwg.org/).
//
// RFC 6455 itself is github.com/coder/websocket — the framing, the masking, the
// close handshake and the close codes, including the empty close frame that
// makes close() with no argument report 1005 rather than 1000. Neither the
// standard library nor golang.org/x has a client worth using here
// (golang.org/x/net/websocket is deprecated and cannot report a close code at
// all), and a hand-written frame codec would be a second implementation of a
// protocol that is already solved. The library has no dependencies of its own.
//
// The API SHAPE belongs to the guest (js/websocket.js): readyState, the event
// objects, binaryType, argument validation, bufferedAmount. This side owns one
// connection per handle, does every blocking thing on its own goroutines, and
// reports each transition by calling one guest function on the loop. So the
// guest never blocks, and the loop goroutine never touches the network.
//
// Permissions are the interpreter's, exactly as for fetch: Config.Resolve per
// hostname and Config.Dial per resolved address, applied by the same dialer
// (see newHTTPClient) so a WebSocket cannot reach an origin fetch could not.

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
)

// wsAPI owns every open WebSocket on one interpreter.
type wsAPI struct {
	js       *spidermonkey.JS
	loop     *eventloop.Loop
	dispatch *spidermonkey.Object // __ws_dispatch(id, type, a, b, c)

	roots      *x509.CertPool
	clientOnce sync.Once
	client     *http.Client

	mu    sync.Mutex
	conns map[int64]*wsConn
	next  int64
}

// wsOut is one thing the guest asked to be put on the wire. A close request
// travels the same queue as the messages so that send() followed by close()
// flushes rather than races: the writer sees them in the order they were made.
type wsOut struct {
	close  bool
	binary bool
	data   []byte
	code   int
	reason string
}

type wsConn struct {
	api    *wsAPI
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	out    chan wsOut

	mu       sync.Mutex
	conn     *websocket.Conn
	sentDone bool // the guest has been told this socket is closed
	outDone  bool // out has been closed; no further sends may be queued
	// closing means the guest asked for the closing handshake. From then on the
	// authoritative outcome is writeLoop's Close() call, and readLoop must not
	// report the connection-teardown error it is about to see as an abnormal
	// closure: Close() tearing the connection down under a blocked Read is what
	// a SUCCESSFUL handshake looks like from the read side.
	closing bool
}

func (c *wsConn) isClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing
}

func installWebSocket(js *spidermonkey.JS, loop *eventloop.Loop, roots *x509.CertPool) (*wsAPI, error) {
	a := &wsAPI{js: js, loop: loop, roots: roots, conns: map[int64]*wsConn{}}
	v, err := js.Global().Get("__ws_dispatch")
	if err != nil {
		return nil, err
	}
	o := v.Object()
	if o == nil || !o.IsFunction() {
		return nil, fmt.Errorf("web: __ws_dispatch is not a function")
	}
	a.dispatch = o
	for name, fn := range map[string]spidermonkey.Func{
		"__ws_connect": a.opConnect,
		"__ws_send":    a.opSend,
		"__ws_close":   a.opClose,
	} {
		if err := js.Global().DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// closeAll drops every open connection. Called from Web.Close, and between
// pooled requests, so a socket cannot outlive the instance that opened it.
func (a *wsAPI) closeAll() {
	a.mu.Lock()
	conns := make([]*wsConn, 0, len(a.conns))
	for _, c := range a.conns {
		conns = append(conns, c)
	}
	a.conns = map[int64]*wsConn{}
	a.mu.Unlock()
	for _, c := range conns {
		c.cancel()
	}
}

// __ws_connect(url, protocolsCSV) starts a handshake and returns the handle the
// guest addresses the socket by. It returns immediately: the dial, the
// handshake and everything after it happen on goroutines of their own.
//
// The URL and the subprotocol list have already been validated by the guest —
// that is the specification's own order, and the errors are DOMExceptions with
// names only the guest can raise.
func (a *wsAPI) opConnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	// The handshake goes over a client of its own, not fetch's. It shares the
	// permission-checked dialer — the point of the check — but not the HTTP cache
	// or the permissive writer: a 101 response hands back the connection itself,
	// and a transport that wants to read the body to the end would take it away.
	a.clientOnce.Do(func() {
		a.client = &http.Client{Transport: &http.Transport{
			DialContext: permissionDial(cfg), TLSClientConfig: tlsConfig(a.roots),
		}}
	})
	if len(args) < 2 {
		return nil, fmt.Errorf("__ws_connect: (url, protocols) required")
	}
	target := args[0].String()
	var protocols []string
	if s := args[1].String(); s != "" {
		protocols = strings.Split(s, ",")
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.next++
	c := &wsConn{api: a, id: a.next, ctx: ctx, cancel: cancel, out: make(chan wsOut, 64)}
	a.conns[c.id] = c
	a.mu.Unlock()
	// The socket keeps the loop alive until its close event has been delivered:
	// an open socket is an in-flight op, and a run that ended while one was still
	// connected would report a timeout instead of the message it was waiting for.
	a.loop.AddPending("websocket")
	go c.run(target, protocols)
	return spidermonkey.ValueOf(float64(c.id)), nil
}

// run performs the handshake and then drives the connection until it ends.
func (c *wsConn) run(target string, protocols []string) {
	conn, err := c.dial(target, protocols)
	if err != nil {
		// A handshake that failed is a connection that never existed: the guest
		// fires error and then close(1006), which is what "fail the WebSocket
		// connection" means.
		c.finish(1006, "", false)
		return
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.post("open", spidermonkey.ValueOf(conn.Subprotocol()), spidermonkey.ValueOf(""))

	// One goroutine reads, one writes. They are separate because both block, and
	// a socket must be able to receive while a large send is still going out.
	go c.writeLoop()
	c.readLoop()
}

// dial performs the opening handshake over the same permission-checked
// transport fetch uses.
func (c *wsConn) dial(target string, protocols []string) (*websocket.Conn, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	// Credentials in the URL become an Authorization header, as they do for any
	// other fetch: the handshake is an HTTP request, and the userinfo must not
	// travel in the request line.
	if u.User != nil {
		pass, _ := u.User.Password()
		header.Set("Authorization", "Basic "+basicAuth(u.User.Username(), pass))
		u.User = nil
	}
	conn, resp, err := websocket.Dial(c.ctx, u.String(), &websocket.DialOptions{
		HTTPClient:   c.api.client,
		HTTPHeader:   header,
		Subprotocols: protocols,
		// The extension is not offered. A browser negotiates permessage-deflate,
		// but nothing observable here depends on it (extensions reports "" either
		// way when the server declines), and compression defeats the backpressure
		// the send queue exists to expose.
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	// The default read limit is 32 KiB; the suite sends 64 KiB messages, and a
	// limit is a policy this layer has no business imposing on the guest.
	conn.SetReadLimit(-1)
	return conn, nil
}

// writeLoop puts queued messages on the wire, reporting each one's size back to
// the guest so bufferedAmount can fall. A close request ends the loop.
func (c *wsConn) writeLoop() {
	for out := range c.out {
		conn := c.socket()
		if conn == nil {
			return
		}
		if out.close {
			code := websocket.StatusCode(out.code)
			if out.code == 0 {
				// close() with no argument sends an EMPTY close frame, and the peer
				// echoes an empty one back — which is what makes the close event
				// report 1005 rather than 1000. The library writes no payload for
				// this code rather than refusing it.
				code = websocket.StatusNoStatusRcvd
			}
			// Close performs the whole closing handshake, and ITS outcome is the
			// authoritative one. It races readLoop for the peer's close frame —
			// whichever reads it first wins, and when Close wins, readLoop's Read
			// fails with a connection error that LOOKS like an abnormal closure.
			// Reporting from both sides (finish is once-only) is what makes the
			// race harmless; deciding from readLoop alone misreported a completed
			// handshake as 1006/unclean whenever Close got there first.
			err := conn.Close(code, out.reason)
			var ce websocket.CloseError
			switch {
			case err == nil:
				// The peer answered with the code we sent. An empty frame is
				// reported as 1005 with no reason, per the protocol.
				reported, reason := out.code, out.reason
				if out.code == 0 {
					reported, reason = 1005, ""
				}
				c.finish(reported, reason, true)
			case errors.As(err, &ce):
				// The handshake completed, with a different answer than we sent.
				c.finish(int(ce.Code), ce.Reason, true)
			default:
				c.finish(1006, "", false)
			}
			return
		}
		typ := websocket.MessageText
		if out.binary {
			typ = websocket.MessageBinary
		}
		if err := conn.Write(c.ctx, typ, out.data); err != nil {
			return
		}
		c.post("drain", spidermonkey.ValueOf(float64(len(out.data))))
	}
}

// readLoop delivers messages until the connection ends, then reports how.
func (c *wsConn) readLoop() {
	conn := c.socket()
	for {
		typ, data, err := conn.Read(c.ctx)
		if err != nil {
			code, reason, clean := closeOutcome(err)
			if !clean && c.isClosing() {
				// The guest asked to close, so this error is (usually) just
				// Close() tearing the connection down mid-Read; writeLoop reports
				// the handshake's real outcome. finish is once-only, so if Close()
				// genuinely failed, its 1006 still gets through.
				return
			}
			c.finish(code, reason, clean)
			return
		}
		if typ == websocket.MessageBinary {
			c.postBytes(data)
			continue
		}
		c.post("message", spidermonkey.ValueOf(false), spidermonkey.ValueOf(string(data)))
	}
}

// closeOutcome turns the error that ended a read into the three things a close
// event carries. A close frame the peer sent gives all three; anything else is
// an abnormal closure, which is 1006 and never clean.
func closeOutcome(err error) (code int, reason string, clean bool) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		// 1005 means the peer's close frame carried no payload. It is reported as
		// the code, and the closure is still clean — the handshake completed.
		return int(ce.Code), ce.Reason, true
	}
	return 1006, "", false
}

// socket returns the live connection, or nil if the handshake has not finished.
func (c *wsConn) socket() *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// post calls the guest dispatcher on the loop goroutine.
func (c *wsConn) post(kind string, args ...spidermonkey.Value) {
	c.api.loop.Post(func() error {
		all := append([]spidermonkey.Value{spidermonkey.ValueOf(float64(c.id)), spidermonkey.ValueOf(kind)}, args...)
		_, err := c.api.dispatch.Call(all...)
		return err
	})
}

// postBytes delivers a binary message. The Uint8Array has to be built on the
// loop goroutine, so the bytes travel in the closure rather than the value.
func (c *wsConn) postBytes(data []byte) {
	c.api.loop.Post(func() error {
		u8, err := c.api.js.NewBytes(data)
		if err != nil {
			return err
		}
		defer u8.Free()
		_, err = c.api.dispatch.Call(spidermonkey.ValueOf(float64(c.id)),
			spidermonkey.ValueOf("message"), spidermonkey.ValueOf(true), u8)
		return err
	})
}

// finish reports the close exactly once and releases the socket's hold on the
// loop. Both goroutines can reach it — a failed handshake, a read error, a
// cancelled context — and only the first report is the one that happened.
func (c *wsConn) finish(code int, reason string, clean bool) {
	c.mu.Lock()
	if c.sentDone {
		c.mu.Unlock()
		return
	}
	c.sentDone = true
	if !c.outDone {
		c.outDone = true
		close(c.out)
	}
	c.mu.Unlock()

	c.api.mu.Lock()
	delete(c.api.conns, c.id)
	c.api.mu.Unlock()

	c.api.loop.Post(func() error {
		defer c.api.loop.DonePending("websocket")
		defer c.cancel()
		_, err := c.api.dispatch.Call(spidermonkey.ValueOf(float64(c.id)),
			spidermonkey.ValueOf("close"), spidermonkey.ValueOf(float64(code)),
			spidermonkey.ValueOf(reason), spidermonkey.ValueOf(clean))
		return err
	})
}

// queue hands one item to the writer, or reports that the socket is already
// finished. It never blocks the loop goroutine: the channel is buffered, and a
// full buffer means the guest is outrunning the network — which is what
// bufferedAmount is for, so the send is dropped only if the socket is gone.
func (c *wsConn) queue(out wsOut) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outDone {
		return false
	}
	select {
	case c.out <- out:
		return true
	default:
	}
	// The buffer is full. Growing it off the loop keeps send() synchronous, which
	// the API requires: send() may not block and may not fail on backpressure.
	go func() {
		select {
		case c.out <- out:
		case <-c.ctx.Done():
		}
	}()
	return true
}

// __ws_send(id, isBinary, data) queues one message. Validation (readyState, the
// argument types) is the guest's; this only encodes.
func (a *wsAPI) opSend(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("__ws_send: (id, isBinary, data) required")
	}
	c := a.conn(args[0])
	if c == nil {
		return spidermonkey.ValueOf(false), nil
	}
	out := wsOut{binary: args[1].Bool()}
	if out.binary {
		b, err := argBytes(args[2])
		if err != nil {
			return nil, err
		}
		out.data = b
	} else {
		out.data = []byte(args[2].String())
	}
	return spidermonkey.ValueOf(c.queue(out)), nil
}

// __ws_close(id, code, reason) starts the closing handshake. code 0 means the
// guest called close() with no argument, and an empty close frame goes out.
func (a *wsAPI) opClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("__ws_close: (id, code, reason) required")
	}
	c := a.conn(args[0])
	if c == nil {
		return spidermonkey.Undefined(), nil
	}
	code, reason := int(args[1].Float()), args[2].String()
	if c.socket() == nil {
		// Still connecting: there is no handshake to close, so the connection is
		// abandoned. Cancelling the context makes the dial return promptly, and the
		// guest hears 1006 — "fail the WebSocket connection".
		c.cancel()
		return spidermonkey.Undefined(), nil
	}
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.queue(wsOut{close: true, code: code, reason: reason})
	return spidermonkey.Undefined(), nil
}

func (a *wsAPI) conn(v spidermonkey.Value) *wsConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[int64(v.Float())]
}

// basicAuth encodes credentials the way http.Request.SetBasicAuth does, which
// net/http does not export on its own.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
