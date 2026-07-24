package nodejs

// net.go: raw TCP for node:net (Socket.connect + createServer) and the
// http.request client, all over Go net with Config.Dial/Resolve/Listen
// enforcement. Sockets are host-side objects driven from the loop goroutine;
// inbound data and lifecycle events are posted back onto the loop.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

type netState struct {
	mu        sync.Mutex
	nextID    int64
	conns     map[int64]net.Conn
	writers   map[int64]*connWriter
	listeners map[int64]net.Listener
	udp       map[int64]*net.UDPConn
}

// connWriter serializes socket writes on the connection's OWN goroutine so a
// slow peer (full send window) can never block the single event-loop goroutine.
// Writes queued before the connection is established (async connect) are held
// until it attaches, preserving order.
type connWriter struct {
	mu       sync.Mutex
	conn     net.Conn // nil until attached
	queue    [][]byte
	closeReq bool
	wake     chan struct{}
}

func newConnWriter() *connWriter { return &connWriter{wake: make(chan struct{}, 1)} }

func (w *connWriter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// enqueue adds a write; false once the writer is closing. Never blocks.
func (w *connWriter) enqueue(data []byte) bool {
	w.mu.Lock()
	if w.closeReq {
		w.mu.Unlock()
		return false
	}
	w.queue = append(w.queue, data)
	w.mu.Unlock()
	w.signal()
	return true
}

func (w *connWriter) attach(conn net.Conn) {
	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()
	w.signal()
}

func (w *connWriter) requestClose() {
	w.mu.Lock()
	w.closeReq = true
	w.mu.Unlock()
	w.signal()
}

// run drains queued writes in order on its own goroutine and closes the conn on
// requestClose. It exits once closed; onErr reports a write failure.
func (w *connWriter) run(onErr func(error)) {
	for {
		w.mu.Lock()
		conn := w.conn
		var q [][]byte
		if conn != nil { // hold writes until the conn attaches (async connect)
			q = w.queue
			w.queue = nil
		}
		closeReq := w.closeReq
		w.mu.Unlock()

		for _, data := range q {
			if _, err := conn.Write(data); err != nil {
				if onErr != nil {
					onErr(err)
				}
				break
			}
		}
		if closeReq {
			if conn != nil {
				conn.Close()
			}
			return
		}
		<-w.wake
	}
}

func newNetState() *netState {
	return &netState{
		conns:     map[int64]net.Conn{},
		writers:   map[int64]*connWriter{},
		listeners: map[int64]net.Listener{},
		udp:       map[int64]*net.UDPConn{},
	}
}

// registerConn stores an established conn and starts its write actor, returning
// the writer. onErr reports async write failures.
func (rt *Runtime) registerConn(id int64, conn net.Conn, w *connWriter, onErr func(error)) {
	st := rt.net
	st.mu.Lock()
	st.conns[id] = conn
	st.writers[id] = w
	st.mu.Unlock()
	w.attach(conn)
	go w.run(onErr)
}

func (rt *Runtime) netOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"net_connect":     rt.opNetConnect,
		"net_write":       rt.opNetWrite,
		"net_close":       rt.opNetClose,
		"net_listen":      rt.opNetListen,
		"net_close_srv":   rt.opNetCloseServer,
		"net_attach":      rt.opNetAttach,
		"http_client_req": rt.opHTTPClientReq,
	}
}

// opNetAttach(id, onData, onEnd, onError) starts the reader pump for an
// already-accepted connection (the server path: the guest builds its Socket
// wrapper and its callbacks only after the connection event fires).
func (rt *Runtime) opNetAttach(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("net_attach: (id, onData, onEnd, onError) required")
	}
	id := int64(args[0].Float())
	rt.net.mu.Lock()
	conn := rt.net.conns[id]
	rt.net.mu.Unlock()
	if conn == nil {
		return spidermonkey.Undefined(), nil
	}
	rt.loop.AddPending()
	go rt.pumpConn(id, conn, args[1].Object(), args[2].Object(), args[3].Object())
	return spidermonkey.Undefined(), nil
}

// resolveDialAddr enforces the outbound-connection policy fail-closed and
// returns the exact "ip:port" to dial. It resolves a hostname ONCE here and
// returns the specific authorized address, so the connection lands on the same
// IP that was checked — a later independent lookup cannot smuggle a different
// (e.g. internal) address past Config.Dial (DNS-rebinding TOCTOU).
//
// Fail-closed, matching Config.Exec: a nil hook denies. A literal-IP dial needs
// Config.Dial (host is passed as "" since no name was resolved). A hostname
// dial needs both Config.Resolve (to permit the lookup) and Config.Dial (to
// permit at least one resolved address, WITH the requested host so a policy can
// match host and port jointly).
func resolveDialAddr(cfg spidermonkey.Config, network, host string, port int) (string, error) {
	portStr := strconv.Itoa(port)
	if ip := net.ParseIP(host); ip != nil {
		if cfg.Dial == nil || !cfg.Dial(network, "", ip.String(), port) {
			return "", fmt.Errorf("dial %s:%d: permission denied", host, port)
		}
		return net.JoinHostPort(ip.String(), portStr), nil
	}
	if cfg.Resolve == nil || !cfg.Resolve(host) {
		return "", fmt.Errorf("resolve %q: permission denied", host)
	}
	if cfg.Dial == nil {
		return "", fmt.Errorf("dial %s:%d: permission denied (no Dial policy)", host, port)
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if cfg.Dial(network, host, ip.String(), port) {
			return net.JoinHostPort(ip.String(), portStr), nil
		}
	}
	return "", fmt.Errorf("dial %s:%d: permission denied", host, port)
}

// maxClientBody caps a buffered node:http client response so an
// approved-but-huge peer can't exhaust host memory.
const maxClientBody = 100 << 20 // 100 MiB

// gatedHTTPClient builds an http.Client whose DialContext enforces the same
// resolve-once, dial-the-approved-IP policy as compat/web's fetch, so the
// node:http/https client cannot be DNS-rebound past Config.Dial and connects
// only to addresses the policy approved. Redirects reuse the same DialContext.
func gatedHTTPClient(cfg spidermonkey.Config) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(portStr)
			dialAddr, err := resolveDialAddr(cfg, network, host, port)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, dialAddr)
		},
	}}
}

// opNetConnect(host, port, onData, onEnd, onError, onConnect) -> id | err.
// The callbacks are guest functions the loop calls as bytes arrive / the
// socket closes; a reader goroutine posts each event onto the loop.
func (rt *Runtime) opNetConnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 6 {
		return nil, fmt.Errorf("net_connect: (host, port, onData, onEnd, onError, onConnect) required")
	}
	host := args[0].String()
	port := args[1].Int()
	onData := args[2].Object()
	onEnd := args[3].Object()
	onError := args[4].Object()
	onConnect := args[5].Object()

	// Reserve the socket id synchronously (Node's net.connect returns a socket
	// immediately) but resolve+dial OFF the loop, so a slow DNS lookup or TCP
	// connect can never freeze the single event-loop goroutine. Writes issued
	// before the connection lands are buffered by the writer and flushed on
	// attach, so early write()s aren't lost.
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	w := newConnWriter()
	st.writers[id] = w
	st.mu.Unlock()

	rt.loop.AddPending()
	go func() {
		addr, derr := resolveDialAddr(cfg, "tcp", host, port)
		var conn net.Conn
		if derr == nil {
			conn, derr = net.DialTimeout("tcp", addr, 30*time.Second)
		}
		if derr != nil {
			st.mu.Lock()
			delete(st.writers, id)
			st.mu.Unlock()
			w.requestClose()
			rt.loop.Post(func() error {
				defer rt.loop.DonePending()
				if onError != nil {
					onError.Call(netErr(derr)) // {code, message} so the guest sees EACCES/ECONNREFUSED
				}
				for _, o := range []*spidermonkey.Object{onData, onEnd, onError, onConnect} {
					if o != nil {
						o.Free()
					}
				}
				return nil
			})
			return
		}
		st.mu.Lock()
		st.conns[id] = conn
		st.mu.Unlock()
		w.attach(conn)
		go w.run(func(error) {}) // write failures surface via the read side (onError/onEnd)
		if onConnect != nil {
			rt.loop.Post(func() error { onConnect.Call(); onConnect.Free(); return nil })
		}
		rt.pumpConn(id, conn, onData, onEnd, onError) // becomes the read pump; DonePending at close
	}()
	return spidermonkey.ValueOf(id), nil
}

// pumpConn reads the socket on a goroutine, posting data/end/error onto the
// loop and freeing the callback handles when the connection closes.
func (rt *Runtime) pumpConn(id int64, conn net.Conn, onData, onEnd, onError *spidermonkey.Object) {
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			rt.loop.Post(func() error {
				if onData != nil {
					u8, uerr := rt.js.NewBytes(chunk)
					if uerr != nil {
						return nil
					}
					onData.Call(u8)
					u8.Free()
				}
				return nil
			})
		}
		if err != nil {
			rt.net.mu.Lock()
			_, live := rt.net.conns[id]
			delete(rt.net.conns, id)
			w := rt.net.writers[id]
			delete(rt.net.writers, id)
			rt.net.mu.Unlock()
			if w != nil {
				w.requestClose() // stop the write actor for this closed conn
			}
			rt.loop.Post(func() error {
				if live {
					if err != io.EOF && onError != nil {
						onError.Call(spidermonkey.ValueOf(err.Error()))
					}
					if onEnd != nil {
						onEnd.Call()
					}
				}
				for _, o := range []*spidermonkey.Object{onData, onEnd, onError} {
					if o != nil {
						o.Free()
					}
				}
				return nil
			})
			rt.loop.DonePending()
			return
		}
	}
}

func (rt *Runtime) opNetWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("net_write: (id, data) required")
	}
	rt.net.mu.Lock()
	w := rt.net.writers[int64(args[0].Float())]
	rt.net.mu.Unlock()
	if w == nil {
		return spidermonkey.ValueOf(false), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	// Enqueue on the connection's write actor (off-loop). Buffered even before
	// an async connect lands; false only once the socket is closing.
	return spidermonkey.ValueOf(w.enqueue(data)), nil
}

func (rt *Runtime) opNetClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	rt.net.mu.Lock()
	id := int64(args[0].Float())
	w := rt.net.writers[id]
	conn := rt.net.conns[id]
	rt.net.mu.Unlock()
	if w != nil {
		w.requestClose() // flush queued writes, then close the conn
	} else if conn != nil {
		conn.Close()
	}
	return spidermonkey.Undefined(), nil
}

func netErr(err error) spidermonkey.Value {
	code := "ECONNREFUSED"
	if strings.Contains(err.Error(), "permission denied") {
		code = "EACCES"
	}
	return spidermonkey.ValueOf(map[string]any{"code": code, "message": err.Error()})
}

// opNetListen(host, port, onConnection) -> {id, port} | err.
func (rt *Runtime) opNetListen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("net_listen: (host, port, onConnection) required")
	}
	host := args[0].String()
	port := args[1].Int()
	onConn := args[2].Object()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Listen == nil || !cfg.Listen("tcp", addr) {
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "listen " + addr + ": permission denied"}), nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.listeners[id] = ln
	st.mu.Unlock()

	rt.loop.AddPending()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				rt.loop.DonePending()
				return
			}
			st.mu.Lock()
			st.nextID++
			cid := st.nextID
			st.mu.Unlock()
			rt.registerConn(cid, conn, newConnWriter(), func(error) {})
			rt.loop.Post(func() error {
				if onConn != nil {
					onConn.Call(spidermonkey.ValueOf(cid), spidermonkey.ValueOf(conn.RemoteAddr().String()))
				}
				return nil
			})
		}
	}()
	return spidermonkey.ValueOf(map[string]any{"id": id, "port": ln.Addr().(*net.TCPAddr).Port}), nil
}

func (rt *Runtime) opNetCloseServer(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	st.mu.Lock()
	ln := st.listeners[int64(args[0].Float())]
	delete(st.listeners, int64(args[0].Float()))
	st.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	return spidermonkey.Undefined(), nil
}

// opHTTPClientReq(method, url, headersJSON, body, onResponse, onError).
// Asynchronous: the round-trip runs on its own goroutine and the result is
// delivered through onResponse({status, statusText, headers, body}) or
// onError({code, message}) posted back onto the loop, so a slow peer cannot
// freeze the loop. The JS http.request shim adapts it to the
// ClientRequest/IncomingMessage event surface.
func (rt *Runtime) opHTTPClientReq(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 6 {
		return nil, fmt.Errorf("http_client_req: (method, url, headersJSON, body, onResponse, onError) required")
	}
	method := args[0].String()
	rawURL := args[1].String()
	var reqBody io.Reader
	if args[3].Object() != nil || args[3].String() != "" {
		if b, err := valueBytes(args[3]); err == nil && len(b) > 0 {
			reqBody = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, rawURL, reqBody)
	if err != nil {
		return netErr(err), nil
	}
	var headers map[string]string
	if err := jsonUnmarshal(args[2].String(), &headers); err == nil {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	onResponse := args[4].Object()
	onError := args[5].Object()

	// Do the round-trip on a separate goroutine so a slow/hung peer never
	// freezes the single loop goroutine (which drives every timer and every
	// other in-flight request). The result is posted back onto the loop.
	rt.loop.AddPending()
	go func() {
		// The gated client enforces Config.Resolve/Dial in its DialContext and
		// connects only to the approved IP (no DNS-rebinding window).
		client := gatedHTTPClient(cfg)
		defer client.CloseIdleConnections() // don't leak kept-alive conns per request
		resp, derr := client.Do(req)
		var body []byte
		var hdrPairs [][2]string
		var status int
		var statusText string
		if derr == nil {
			// Cap the buffered body so an approved-but-huge response can't
			// exhaust host memory. Overflow surfaces as an error.
			body, derr = io.ReadAll(io.LimitReader(resp.Body, maxClientBody+1))
			if derr == nil && int64(len(body)) > maxClientBody {
				derr = fmt.Errorf("response body exceeds %d bytes", maxClientBody)
			}
			if cerr := resp.Body.Close(); derr == nil {
				derr = cerr
			}
			status, statusText = resp.StatusCode, resp.Status
			for k, vs := range resp.Header {
				for _, v := range vs {
					hdrPairs = append(hdrPairs, [2]string{k, v})
				}
			}
		}
		rt.loop.Post(func() (perr error) {
			defer rt.loop.DonePending()
			defer func() {
				if onResponse != nil {
					onResponse.Free()
				}
				if onError != nil {
					onError.Free()
				}
			}()
			// Deliver failures — including guest-object materialization
			// failures — through onError. Returning an error here would stop the
			// WHOLE shared loop (every other in-flight request/timer), which is
			// the wrong blast radius for one client call.
			fail := func(e error) {
				if onError != nil {
					onError.Call(netErr(e))
				}
			}
			if derr != nil {
				fail(derr)
				return nil
			}
			obj, oerr := rt.js.NewObject()
			if oerr != nil {
				fail(oerr)
				return nil
			}
			defer obj.Free()
			obj.Set("status", spidermonkey.ValueOf(status))
			obj.Set("statusText", spidermonkey.ValueOf(statusText))
			obj.Set("headers", spidermonkey.ValueOf(hdrPairs))
			u8, uerr := rt.js.NewBytes(body)
			if uerr != nil {
				fail(uerr)
				return nil
			}
			defer u8.Free()
			obj.Set("body", u8)
			if onResponse != nil {
				onResponse.Call(obj)
			}
			return nil
		})
	}()
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) closeNet() {
	st := rt.net
	st.mu.Lock()
	conns := make([]net.Conn, 0, len(st.conns))
	for _, c := range st.conns {
		conns = append(conns, c)
	}
	lns := make([]net.Listener, 0, len(st.listeners))
	for _, l := range st.listeners {
		lns = append(lns, l)
	}
	udps := make([]*net.UDPConn, 0, len(st.udp))
	for _, u := range st.udp {
		udps = append(udps, u)
	}
	writers := make([]*connWriter, 0, len(st.writers))
	for _, w := range st.writers {
		writers = append(writers, w)
	}
	st.conns = map[int64]net.Conn{}
	st.writers = map[int64]*connWriter{}
	st.listeners = map[int64]net.Listener{}
	st.udp = map[int64]*net.UDPConn{}
	st.mu.Unlock()
	for _, w := range writers {
		w.requestClose()
	}
	for _, c := range conns {
		c.Close()
	}
	for _, l := range lns {
		l.Close()
	}
	for _, u := range udps {
		u.Close()
	}
}
