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
	mu           sync.Mutex
	nextID       int64
	conns        map[int64]net.Conn
	writers      map[int64]*connWriter
	listeners    map[int64]net.Listener
	udp          map[int64]*net.UDPConn
	readResume   map[int64]chan struct{}    // per-conn read flow-control (see armRead)
	readEnded    map[int64]bool             // read half saw a clean peer FIN; write half still open
	clientBodies map[int64]*clientBody      // per http-client-response body pump
	clientReqs   map[int64]*clientReqStream // per streaming http-client request body
	transports   map[int64]*http.Transport  // per keep-alive http.Agent connection pool
}

// clientReqStream is the request-body source for a streaming (chunked) http
// client request. The guest's req.write() pushes chunks (http_client_write) and
// req.end() closes the stream (http_client_end); the Go transport reads them as
// the request body from its own goroutine. Chunks are queued (not handed over a
// blocking pipe) so http_client_write never parks the single loop goroutine
// waiting for the transport to consume them.
type clientReqStream struct {
	mu     sync.Mutex
	chunks [][]byte
	cur    []byte
	closed bool
	notify chan struct{} // buffered(1): wakes a parked Read on write/close
	done   chan struct{} // closed to abort the reader (request destroyed/teardown)
	once   sync.Once
}

func (s *clientReqStream) write(b []byte) {
	s.mu.Lock()
	if !s.closed {
		s.chunks = append(s.chunks, b)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *clientReqStream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
}

func (s *clientReqStream) abort() { s.once.Do(func() { close(s.done) }) }

func (s *clientReqStream) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Read is called by the http transport on its round-trip goroutine. It returns
// queued chunks and blocks (on notify) when the queue is empty until more data
// is written, the stream is closed (EOF), or the request is aborted.
func (s *clientReqStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.cur) == 0 && len(s.chunks) > 0 {
			s.cur = s.chunks[0]
			s.chunks = s.chunks[1:]
		}
		if len(s.cur) > 0 {
			n := copy(p, s.cur)
			s.cur = s.cur[n:]
			s.mu.Unlock()
			return n, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		select {
		case <-s.notify:
		case <-s.done:
			return 0, io.EOF
		}
	}
}

// clientBody is the flow-control + lifetime handle for one streaming http-client
// response body. resume gates each read on the guest IncomingMessage asking for
// more (its _read → http_client_body_resume); done is closed to stop the pump
// (guest destroyed the response, or teardown). cancel aborts the request context
// so a pump PARKED inside a blocking resp.Body.Read (an idle SSE/long-poll
// stream) is actually interrupted — closing `done` alone can't unblock a read.
type clientBody struct {
	resume chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
}

func (cb *clientBody) finish() {
	cb.once.Do(func() {
		if cb.cancel != nil {
			cb.cancel()
		}
		close(cb.done)
	})
}

// headerScalar coerces a JSON-decoded header value to Node's String() form: a
// string verbatim, a number/bool textually, and any object to "[object Object]"
// (matching Node's coercion) rather than leaking raw JSON into the header.
func headerScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return "[object Object]"
	}
}

// freeObjects releases guest object handles, ignoring nils.
func freeObjects(objs ...*spidermonkey.Object) {
	for _, o := range objs {
		if o != nil {
			o.Free()
		}
	}
}

// armRead creates and primes the read flow-control channel for a connection and
// returns it. The reader pump waits on this channel before each read, so it only
// pulls the next chunk when the guest asked for more (its Socket._read →
// net_read_resume). The single primed token lets the pump read the first chunk
// before any _read fires; thereafter a guest that stops reading bounds in-flight
// host buffering to ~one chunk, instead of letting a fast peer stream unbounded
// data into the host and (via posted chunks) the guest heap.
func (st *netState) armRead(id int64) chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	st.mu.Lock()
	st.readResume[id] = ch
	st.mu.Unlock()
	return ch
}

// pokeRead releases one read on a connection's flow-control channel (no-op if it
// is unknown or already has a pending token). Used both by the guest's
// net_read_resume and by close/teardown paths to wake a pump parked on the
// channel so it observes the now-closed conn and unwinds.
func (st *netState) pokeRead(id int64) {
	st.mu.Lock()
	ch := st.readResume[id]
	st.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// connWriter serializes socket writes on the connection's OWN goroutine so a
// slow peer (full send window) can never block the single event-loop goroutine.
// Writes queued before the connection is established (async connect) are held
// until it attaches, preserving order.
// connWriter drives an io.WriteCloser (a socket, or a child's stdin pipe) — any
// destination whose Write can block on a full buffer.
type connWrite struct {
	data   []byte
	onDone func() // fired (on the writer goroutine) once this chunk is written; drives backpressure
}

type connWriter struct {
	mu            sync.Mutex
	conn          io.WriteCloser // nil until attached
	queue         []connWrite
	closeReq      bool
	halfCloseReq  bool // socket.end(): send FIN after flushing, keep the read side open
	halfCloseDone bool
	wake          chan struct{}
}

func newConnWriter() *connWriter { return &connWriter{wake: make(chan struct{}, 1)} }

func (w *connWriter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// enqueue adds a write; false once the writer is closing. Never blocks. onDone
// (may be nil) fires once the chunk is written (or dropped on close), so the
// guest Writable can pace itself instead of queueing unboundedly.
func (w *connWriter) enqueue(data []byte, onDone func()) bool {
	w.mu.Lock()
	if w.closeReq {
		w.mu.Unlock()
		if onDone != nil {
			onDone()
		}
		return false
	}
	w.queue = append(w.queue, connWrite{data: data, onDone: onDone})
	w.mu.Unlock()
	w.signal()
	return true
}

func (w *connWriter) attach(conn io.WriteCloser) {
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

// requestHalfClose flushes queued writes then half-closes the write side (sends
// FIN) while leaving the read side open — socket.end() semantics, so an
// EOF-delimited peer sees end-of-request and can still reply.
func (w *connWriter) requestHalfClose() {
	w.mu.Lock()
	w.halfCloseReq = true
	w.mu.Unlock()
	w.signal()
}

// writeSideEnded reports whether the guest already ended or closed the write
// half (socket.end()/destroy()). The read pump consults it on a clean peer FIN
// to decide between a read-side-only teardown (write half stays usable —
// allowHalfOpen) and a full teardown (both halves done).
func (w *connWriter) writeSideEnded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.halfCloseReq || w.closeReq
}

// run drains queued writes in order on its own goroutine and closes the conn on
// requestClose. It exits once closed; onErr reports a write failure.
func (w *connWriter) run(onErr func(error)) {
	for {
		w.mu.Lock()
		conn := w.conn
		var q []connWrite
		if conn != nil { // hold writes until the conn attaches (async connect)
			q = w.queue
			w.queue = nil
		}
		closeReq := w.closeReq
		w.mu.Unlock()

		for i, item := range q {
			_, err := conn.Write(item.data)
			if item.onDone != nil {
				item.onDone() // ack this chunk (backpressure), even on error
			}
			if err != nil {
				if onErr != nil {
					onErr(err)
				}
				// The conn is dead; we won't write the rest of this batch, but we
				// must still ack every already-dequeued chunk or the guest Writable
				// that is awaiting those write callbacks hangs forever.
				for _, rest := range q[i+1:] {
					if rest.onDone != nil {
						rest.onDone()
					}
				}
				break
			}
		}
		if closeReq {
			if conn != nil {
				conn.Close()
			}
			// Ack any writes still queued so the guest Writable isn't stranded.
			w.mu.Lock()
			leftover := w.queue
			w.queue = nil
			w.mu.Unlock()
			for _, item := range leftover {
				if item.onDone != nil {
					item.onDone()
				}
			}
			return
		}
		// Half-close (socket.end): once all queued writes are flushed, send FIN on
		// the write side but keep reading, so an EOF-delimited peer can respond.
		w.mu.Lock()
		doHalf := w.halfCloseReq && !w.halfCloseDone && len(w.queue) == 0 && conn != nil
		if doHalf {
			w.halfCloseDone = true
		}
		w.mu.Unlock()
		if doHalf {
			if hc, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = hc.CloseWrite()
			}
		}
		<-w.wake
	}
}

func newNetState() *netState {
	return &netState{
		conns:        map[int64]net.Conn{},
		writers:      map[int64]*connWriter{},
		listeners:    map[int64]net.Listener{},
		udp:          map[int64]*net.UDPConn{},
		readResume:   map[int64]chan struct{}{},
		readEnded:    map[int64]bool{},
		clientBodies: map[int64]*clientBody{},
		clientReqs:   map[int64]*clientReqStream{},
		transports:   map[int64]*http.Transport{},
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
		"net_connect":             rt.opNetConnect,
		"net_write":               rt.opNetWrite,
		"net_read_resume":         rt.opNetReadResume,
		"net_end":                 rt.opNetEnd,
		"net_close":               rt.opNetClose,
		"net_listen":              rt.opNetListen,
		"net_close_srv":           rt.opNetCloseServer,
		"net_attach":              rt.opNetAttach,
		"http_client_req":         rt.opHTTPClientReq,
		"http_client_req_stream":  rt.opHTTPClientReqStream,
		"http_client_write":       rt.opHTTPClientWrite,
		"http_client_end":         rt.opHTTPClientEnd,
		"http_client_body_resume": rt.opHTTPClientBodyResume,
		"http_client_body_cancel": rt.opHTTPClientBodyCancel,
		"http_agent_close":        rt.opHTTPAgentClose,
	}
}

// opHTTPClientBodyResume lets the guest IncomingMessage request the next
// response-body chunk (its _read), driving the client body pump's flow control.
func (rt *Runtime) opHTTPClientBodyResume(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	st.mu.Lock()
	cb := st.clientBodies[int64(args[0].Float())]
	st.mu.Unlock()
	if cb != nil {
		select {
		case cb.resume <- struct{}{}:
		default: // already primed; the pump reads on the next turn
		}
	}
	return spidermonkey.Undefined(), nil
}

// opHTTPClientBodyCancel stops the body pump when the guest destroys/abandons the
// response before consuming it (res.destroy()), so the reader goroutine unwinds.
func (rt *Runtime) opHTTPClientBodyCancel(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	id := int64(args[0].Float())
	st.mu.Lock()
	cb := st.clientBodies[id]
	stream := st.clientReqs[id]
	st.mu.Unlock()
	if cb != nil {
		cb.finish()
	}
	// A destroyed/aborted request also abandons its streaming request body (the
	// send half), so unblock any request-body Read still parked.
	if stream != nil {
		stream.abort()
	}
	return spidermonkey.Undefined(), nil
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
		// Stale/duplicate attach (the conn was removed during teardown): pumpConn
		// won't run to free these, so free the three callback handles here.
		freeObjects(args[1].Object(), args[2].Object(), args[3].Object())
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
func gatedTransport(cfg spidermonkey.Config) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Transport{
		// Node's raw http client never sends Accept-Encoding nor auto-decompresses
		// (the caller uses zlib) — disable Go's transparent gzip so the guest sees
		// the raw body with its original Content-Encoding/Content-Length headers.
		DisableCompression: true,
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
	}
}

func gatedHTTPClient(cfg spidermonkey.Config) *http.Client {
	return &http.Client{
		// Node's http.request/http.get do NOT auto-follow redirects — they hand the
		// 3xx to the caller (libraries like follow-redirects/node-fetch do their own
		// redirect handling). Return the last response instead of following.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     gatedTransport(cfg),
	}
}

// agentConfig is the guest http.Agent descriptor: a stable id plus the pooling
// knobs. A zero id or keepAlive=false means "no pooling" (a throwaway client).
type agentConfig struct {
	ID             int64 `json:"id"`
	KeepAlive      bool  `json:"keepAlive"`
	MaxSockets     int   `json:"maxSockets"`
	MaxFreeSockets int   `json:"maxFreeSockets"`
}

// clientForAgent returns an http.Client for the given agent descriptor and a
// cleanup to run when the round-trip finishes. A keep-alive agent reuses a
// persistent, per-agent *http.Transport (so sequential requests reuse the TCP
// connection and maxSockets throttles concurrent conns via MaxConnsPerHost); its
// cleanup is a no-op so idle connections stay pooled. A non-pooling request gets
// a fresh client whose idle connections are closed when the round-trip ends.
func (rt *Runtime) clientForAgent(cfg spidermonkey.Config, agentJSON string) (*http.Client, func()) {
	var a agentConfig
	if agentJSON != "" {
		_ = jsonUnmarshal(agentJSON, &a)
	}
	if !a.KeepAlive || a.ID == 0 {
		c := gatedHTTPClient(cfg)
		return c, func() { c.CloseIdleConnections() }
	}
	st := rt.net
	st.mu.Lock()
	tr := st.transports[a.ID]
	if tr == nil {
		tr = gatedTransport(cfg)
		tr.DisableKeepAlives = false
		if a.MaxSockets > 0 {
			tr.MaxConnsPerHost = a.MaxSockets
		}
		if a.MaxFreeSockets > 0 {
			tr.MaxIdleConnsPerHost = a.MaxFreeSockets
		}
		st.transports[a.ID] = tr
	}
	st.mu.Unlock()
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     tr,
	}, func() {} // keep-alive: leave idle connections pooled for reuse
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
			// Run the actor once to ACK any writes queued before the connect
			// failed (their _write callbacks would otherwise strand, hanging the
			// socket's Writable). With no conn it just fires the leftover onDone
			// callbacks and exits.
			go w.run(func(error) {})
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
		// The guest may have destroyed the socket while the dial was in flight;
		// opNetClose then deleted writers[id] to mark the close intentional. If so,
		// do NOT resurrect it — closing conn, freeing the handles and releasing the
		// pending, without firing connect/pump (which would emit spurious
		// connect/error/end on a socket the guest already abandoned).
		st.mu.Lock()
		_, stillOpen := st.writers[id]
		if stillOpen {
			st.conns[id] = conn
		}
		st.mu.Unlock()
		if !stillOpen {
			conn.Close()
			// opNetClose already requested close; run the actor (conn was never
			// attached to it) so any writes queued before destroy get their onDone
			// acked instead of stranding a guest awaiting them.
			go w.run(func(error) {})
			rt.loop.Post(func() error {
				defer rt.loop.DonePending()
				for _, o := range []*spidermonkey.Object{onData, onEnd, onError, onConnect} {
					if o != nil {
						o.Free()
					}
				}
				return nil
			})
			return
		}
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
	resume := rt.net.armRead(id)
	buf := make([]byte, 32<<10)
	for {
		// Flow control: read the next chunk only when the guest asked for more
		// (Socket._read → net_read_resume) or a close poked us; a guest that stops
		// reading can't force a fast peer's stream into unbounded host memory.
		<-resume
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
			w := rt.net.writers[id]
			// A clean peer FIN (io.EOF) only ends the READ half. If the guest has
			// not ended its write half yet, keep the conn + writer registered so
			// later writes still reach the peer (allowHalfOpen) — tearing the
			// writer down here made opNetWrite silently drop them. The write half
			// is then torn down by net_end/net_close (see opNetEnd), which observe
			// readEnded and do the full close. On a real error — or when the guest
			// already half-closed, so both directions are now done — tear down
			// everything as before. The writeSideEnded check runs under st.mu, the
			// same lock opNetEnd holds while setting halfCloseReq, so the two
			// teardown paths cannot both conclude "the other side does it".
			keepWriter := err == io.EOF && live && w != nil && !w.writeSideEnded()
			if keepWriter {
				rt.net.readEnded[id] = true
				delete(rt.net.readResume, id)
				w = nil // leave the write actor running
			} else {
				delete(rt.net.conns, id)
				delete(rt.net.writers, id)
				delete(rt.net.readResume, id)
				delete(rt.net.readEnded, id)
			}
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
			// keepWriter: the pump's AddPending is transferred to the still-open
			// write half — a half-open socket keeps the event loop alive until the
			// guest ends/destroys it (Node process-liveness), and the pending write
			// acks are posted onto the loop and must find it running. Released by
			// the full-close paths (opNetEnd/opNetClose/closeNet) that consume the
			// readEnded flag.
			if !keepWriter {
				rt.loop.DonePending()
			}
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
	// onWritten (args[2], optional) is the guest Writable's _write callback:
	// firing it only once the chunk is flushed paces the guest (backpressure).
	var onWritten *spidermonkey.Object
	if len(args) > 2 {
		onWritten = args[2].Object()
	}
	if w == nil {
		// Writer gone (peer reset raced this write): free the data Buffer arg (the
		// normal path frees it via valueBytes) so it doesn't leak.
		freeObjects(args[1].Object())
		rt.fireNetCallback(onWritten)
		return spidermonkey.ValueOf(false), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		rt.fireNetCallback(onWritten)
		return nil, err
	}
	onDone := func() {}
	if onWritten != nil {
		onDone = func() { rt.fireNetCallback(onWritten) }
	}
	// Enqueue on the connection's write actor (off-loop). Buffered even before
	// an async connect lands; false only once the socket is closing.
	return spidermonkey.ValueOf(w.enqueue(data, onDone)), nil
}

// fireNetCallback invokes a write-completion callback on the loop and frees it.
func (rt *Runtime) fireNetCallback(cb *spidermonkey.Object) {
	if cb == nil {
		return
	}
	rt.loop.Post(func() error {
		cb.Call()
		cb.Free()
		return nil
	})
}

// opNetReadResume(id) releases one read on the connection's flow-control channel
// — the guest Socket's _read calling for the next chunk (see armRead).
func (rt *Runtime) opNetReadResume(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	rt.net.pokeRead(int64(args[0].Float()))
	return spidermonkey.Undefined(), nil
}

// opNetEnd(id) ends the write side (socket.end). While the peer's half is still
// open this is a half-close: flush queued writes, send FIN, keep reading. If the
// read half already saw the peer's FIN (readEnded — the pump left the writer
// alive for allowHalfOpen writes), this end() completes the connection: flush,
// close the conn, and drop it from the tables so nothing leaks.
func (rt *Runtime) opNetEnd(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	id := int64(args[0].Float())
	rt.net.mu.Lock()
	w := rt.net.writers[id]
	readEnded := rt.net.readEnded[id]
	if w != nil {
		if readEnded {
			delete(rt.net.conns, id)
			delete(rt.net.writers, id)
			delete(rt.net.readEnded, id)
		} else {
			// Set halfCloseReq under st.mu so a concurrent peer-FIN in pumpConn
			// either sees it (and does the full teardown itself) or marks
			// readEnded first (in which case we'd have taken the branch above).
			w.requestHalfClose()
		}
	}
	rt.net.mu.Unlock()
	if w != nil && readEnded {
		w.requestClose()      // flush queued writes, then close the conn
		rt.loop.DonePending() // release the pending the read pump left with the write half
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opNetClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	rt.net.mu.Lock()
	id := int64(args[0].Float())
	w := rt.net.writers[id]
	conn := rt.net.conns[id]
	// Remove the entry NOW so the pump's imminent read error (from the writer
	// closing the conn) is seen as an intentional local close (live == false)
	// rather than surfaced to the guest as a spurious 'error'.
	delete(rt.net.conns, id)
	delete(rt.net.writers, id)
	readEnded := rt.net.readEnded[id]
	delete(rt.net.readEnded, id)
	rt.net.mu.Unlock()
	if w != nil {
		w.requestClose() // flush queued writes, then close the conn
	} else if conn != nil {
		conn.Close()
	}
	if readEnded {
		// The read pump already exited (peer FIN) leaving its pending with the
		// write half; this destroy consumes it.
		rt.loop.DonePending()
	}
	// If the pump is parked waiting for the guest to ask for more, wake it so it
	// observes the closed conn and unwinds (frees handles, DonePending).
	rt.net.pokeRead(id)
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
		freeObjects(onConn) // no listener will hold this callback for its lifetime
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "listen " + addr + ": permission denied"}), nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		freeObjects(onConn)
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
				// The listener was closed (server.close): free the accept
				// callback the goroutine held. Post it so it runs after any
				// already-queued onConn.Call, then release the pending.
				rt.loop.Post(func() error { freeObjects(onConn); return nil })
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

// applyClientHeaders sets the request headers from the guest's headersJSON. A
// value may be a string, an array (Node allows several values, e.g. multiple
// Set-Cookie/Cookie), or a scalar (number/bool); each is coerced to Node's
// String() form — a single non-string value must not fail the whole unmarshal
// and silently drop EVERY header.
func applyClientHeaders(req *http.Request, headersJSON string) {
	var headers map[string]any
	if err := jsonUnmarshal(headersJSON, &headers); err != nil {
		return
	}
	for k, v := range headers {
		switch val := v.(type) {
		case string:
			req.Header.Set(k, val)
		case []any:
			for _, e := range val {
				req.Header.Add(k, headerScalar(e))
			}
		default:
			req.Header.Set(k, headerScalar(val))
		}
	}
}

// deliverClientReqError fires onError on the next loop turn and frees both
// callback handles. Used when the request can't even be constructed (invalid
// method/URL): the JS shim ignores this op's return value, so an error must
// reach the guest through onError or the ClientRequest hangs forever.
func (rt *Runtime) deliverClientReqError(err error, onResponse, onError *spidermonkey.Object) {
	freeObjects(onResponse)
	if onError != nil {
		reqErr := err
		rt.loop.Post(func() error {
			onError.Call(netErr(reqErr))
			onError.Free()
			return nil
		})
	}
}

// opHTTPClientReq(method, url, headersJSON, body, agentJSON, onResponse, onError).
// The buffered fast path: the whole request body is known up front (Node sets
// Content-Length). Asynchronous — the round-trip runs on its own goroutine and
// the result is delivered through onResponse({status, statusText, headers}) or
// onError({code, message}) posted back onto the loop, so a slow peer cannot
// freeze the loop. agentJSON selects a keep-alive connection pool (see
// clientForAgent). The JS http.request shim adapts it to the
// ClientRequest/IncomingMessage event surface.
func (rt *Runtime) opHTTPClientReq(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 7 {
		return nil, fmt.Errorf("http_client_req: (method, url, headersJSON, body, agentJSON, onResponse, onError) required")
	}
	method := args[0].String()
	rawURL := strArg(args[1])
	var reqBody io.Reader
	if args[3].IsObject() || args[3].String() != "" {
		if b, err := valueBytes(args[3]); err == nil && len(b) > 0 {
			reqBody = bytes.NewReader(b)
		}
	}
	agentJSON := args[4].String()
	onResponse := args[5].Object()
	onError := args[6].Object()

	req, err := http.NewRequest(method, rawURL, reqBody)
	if err != nil {
		rt.deliverClientReqError(err, onResponse, onError)
		return spidermonkey.Undefined(), nil
	}
	applyClientHeaders(req, args[2].String())

	client, clientCleanup := rt.clientForAgent(cfg, agentJSON)
	id := rt.startClientRoundTrip(cfg, req, onResponse, onError, client, clientCleanup, nil)
	// Hand the guest the body id so ClientRequest.abort()/destroy() can cancel the
	// request, instead of leaving an aborted/unconsumed response parked.
	return spidermonkey.ValueOf(float64(id)), nil
}

// opHTTPClientReqStream(method, url, headersJSON, agentJSON, onResponse, onError).
// The streaming path used when the guest calls req.write() before req.end(): the
// request is sent with a chunked Transfer-Encoding body fed by http_client_write
// / http_client_end, so headers and the first chunk reach the server promptly
// instead of waiting for the whole body. The response is delivered exactly as in
// the buffered path.
func (rt *Runtime) opHTTPClientReqStream(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 6 {
		return nil, fmt.Errorf("http_client_req_stream: (method, url, headersJSON, agentJSON, onResponse, onError) required")
	}
	method := args[0].String()
	rawURL := strArg(args[1])
	agentJSON := args[3].String()
	onResponse := args[4].Object()
	onError := args[5].Object()

	stream := &clientReqStream{notify: make(chan struct{}, 1), done: make(chan struct{})}
	// The body length is unknown (a live stream), so Go's transport sends it with
	// Transfer-Encoding: chunked and flushes each chunk as it is read — the server
	// sees headers + the first chunk without waiting for req.end().
	req, err := http.NewRequest(method, rawURL, stream)
	if err != nil {
		rt.deliverClientReqError(err, onResponse, onError)
		return spidermonkey.Undefined(), nil
	}
	applyClientHeaders(req, args[2].String())

	client, clientCleanup := rt.clientForAgent(cfg, agentJSON)
	id := rt.startClientRoundTrip(cfg, req, onResponse, onError, client, clientCleanup, stream)
	return spidermonkey.ValueOf(float64(id)), nil
}

// opHTTPClientWrite(id, chunk) pushes one request-body chunk into the streaming
// request; the transport reads it as chunked body data. Non-blocking: the chunk
// is queued, never handed over a blocking pipe, so the loop goroutine is free.
func (rt *Runtime) opHTTPClientWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	st.mu.Lock()
	stream := st.clientReqs[int64(args[0].Float())]
	st.mu.Unlock()
	if stream != nil {
		if b, err := valueBytes(args[1]); err == nil && len(b) > 0 {
			stream.write(append([]byte(nil), b...))
		}
	} else {
		// The streaming request was already ended/aborted/torn down: free the
		// chunk Buffer that valueBytes would otherwise consume.
		freeObjects(args[1].Object())
	}
	return spidermonkey.Undefined(), nil
}

// opHTTPClientEnd(id) closes the streaming request body (req.end()): the reader
// returns EOF and the transport terminates the chunked body.
func (rt *Runtime) opHTTPClientEnd(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	st.mu.Lock()
	stream := st.clientReqs[int64(args[0].Float())]
	st.mu.Unlock()
	if stream != nil {
		stream.close()
	}
	return spidermonkey.Undefined(), nil
}

// opHTTPAgentClose(agentId) tears down a keep-alive agent's connection pool
// (http.Agent.destroy()): its idle connections are closed and the transport is
// dropped, so a destroyed agent leaks no sockets.
func (rt *Runtime) opHTTPAgentClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.net
	st.mu.Lock()
	tr := st.transports[int64(args[0].Float())]
	delete(st.transports, int64(args[0].Float()))
	st.mu.Unlock()
	if tr != nil {
		tr.CloseIdleConnections()
	}
	return spidermonkey.Undefined(), nil
}

// startClientRoundTrip registers the response-body handle (and, for a streaming
// request, the request-body stream) under one id, then runs the round-trip on
// its own goroutine so a slow/hung peer never freezes the single loop goroutine.
// Response headers are delivered as soon as they arrive (onResponse) and the
// body then streams in bounded chunks — never buffering the whole body in host
// memory. Returns the id the guest uses to resume/cancel the response and to
// write/end a streaming request body.
func (rt *Runtime) startClientRoundTrip(cfg spidermonkey.Config, req *http.Request, onResponse, onError *spidermonkey.Object, client *http.Client, clientCleanup func(), stream *clientReqStream) int64 {
	// A cancelable request context so cancelling the body handle (res.destroy() or
	// teardown) interrupts a pump PARKED inside resp.Body.Read — an idle SSE /
	// long-poll stream that isn't sending bytes — which closing `done` alone can't.
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	// Register the streaming body handle BEFORE dispatch so the guest's _read can
	// call http_client_body_resume as soon as onResponse fires.
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	body := &clientBody{resume: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	body.resume <- struct{}{} // prime: allow the first read
	st.clientBodies[id] = body
	if stream != nil {
		st.clientReqs[id] = stream
	}
	st.mu.Unlock()

	cleanup := func() {
		st.mu.Lock()
		delete(st.clientBodies, id)
		if stream != nil {
			delete(st.clientReqs, id)
		}
		st.mu.Unlock()
		if stream != nil {
			stream.abort() // unblock a request-body Read parked after the round-trip ended
		}
	}

	rt.loop.AddPending()
	go func() {
		defer cancel() // release the request context on every exit (idempotent with finish())
		defer clientCleanup()

		resp, derr := client.Do(req)
		if derr != nil {
			rt.loop.Post(func() error {
				defer rt.loop.DonePending()
				defer freeObjects(onResponse, onError)
				cleanup()
				if onError != nil {
					onError.Call(netErr(derr))
				}
				return nil
			})
			return
		}

		var hdrPairs [][2]string
		for k, vs := range resp.Header {
			for _, v := range vs {
				hdrPairs = append(hdrPairs, [2]string{k, v})
			}
		}
		status, statusText := resp.StatusCode, resp.Status
		// Deliver status + headers immediately (on headers, not on full body).
		rt.loop.Post(func() error {
			defer func() {
				if onResponse != nil {
					onResponse.Free()
				}
			}()
			obj, oerr := rt.js.NewObject()
			if oerr != nil {
				// Materialization failed: report and abandon the stream.
				if onError != nil {
					onError.Call(netErr(oerr))
				}
				return nil
			}
			defer obj.Free()
			obj.Set("id", spidermonkey.ValueOf(id))
			obj.Set("status", spidermonkey.ValueOf(status))
			obj.Set("statusText", spidermonkey.ValueOf(statusText))
			obj.Set("headers", spidermonkey.ValueOf(hdrPairs))
			if onResponse != nil {
				onResponse.Call(obj)
			}
			return nil
		})

		// Pump the body incrementally, gated by the guest asking for more. onError
		// outlives onResponse so a mid-body read failure can still be surfaced.
		defer rt.loop.DonePending()
		// Free onError ON the loop, never on this goroutine: closeNet() wakes this
		// pump during rt.Close() teardown, and freeing a guest handle off-loop then
		// races the engine shutdown -> SIGSEGV. A post that doesn't run at teardown
		// just leaks the handle, which the engine close reclaims anyway.
		defer func() {
			if onError != nil {
				o := onError
				rt.loop.Post(func() error { o.Free(); return nil })
			}
		}()
		defer resp.Body.Close()
		defer cleanup()
		buf := make([]byte, 32<<10)
		var total int64
		for {
			select {
			case <-body.resume:
			case <-body.done: // guest destroyed the response / teardown
				return
			}
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				total += int64(n)
				if total > maxClientBody {
					rt.loop.Post(func() error {
						if rt.httpClientBody != nil {
							rt.httpClientBody.Call(spidermonkey.ValueOf(id), spidermonkey.ValueOf(false))
						}
						return nil
					})
					return
				}
				chunk := append([]byte(nil), buf[:n]...)
				rt.loop.Post(func() error {
					if rt.httpClientBody != nil {
						u8, e := rt.js.NewBytes(chunk)
						if e == nil {
							rt.httpClientBody.Call(spidermonkey.ValueOf(id), u8)
							u8.Free()
						}
					}
					return nil
				})
			}
			if rerr != nil {
				// io.EOF is a clean end (null); anything else is an aborted/truncated
				// body (false), matching the server-side pump's signaling.
				aborted := rerr != io.EOF
				rt.loop.Post(func() error {
					if rt.httpClientBody != nil {
						if aborted {
							rt.httpClientBody.Call(spidermonkey.ValueOf(id), spidermonkey.ValueOf(false))
						} else {
							rt.httpClientBody.Call(spidermonkey.ValueOf(id), spidermonkey.Null())
						}
					}
					return nil
				})
				return
			}
		}
	}()
	return id
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
	resumes := make([]chan struct{}, 0, len(st.readResume))
	for _, ch := range st.readResume {
		resumes = append(resumes, ch)
	}
	bodies := make([]*clientBody, 0, len(st.clientBodies))
	for _, cb := range st.clientBodies {
		bodies = append(bodies, cb)
	}
	streams := make([]*clientReqStream, 0, len(st.clientReqs))
	for _, s := range st.clientReqs {
		streams = append(streams, s)
	}
	transports := make([]*http.Transport, 0, len(st.transports))
	for _, tr := range st.transports {
		transports = append(transports, tr)
	}
	halfOpen := len(st.readEnded)
	st.conns = map[int64]net.Conn{}
	st.writers = map[int64]*connWriter{}
	st.listeners = map[int64]net.Listener{}
	st.udp = map[int64]*net.UDPConn{}
	st.readResume = map[int64]chan struct{}{}
	st.readEnded = map[int64]bool{}
	st.clientBodies = map[int64]*clientBody{}
	st.clientReqs = map[int64]*clientReqStream{}
	st.transports = map[int64]*http.Transport{}
	st.mu.Unlock()
	// Release the pending each half-open write half still holds (its read pump
	// exited on peer FIN and left the pending with the writer — see pumpConn).
	for i := 0; i < halfOpen; i++ {
		rt.loop.DonePending()
	}
	// Stop any client body pump so its goroutine unwinds and releases AddPending.
	for _, cb := range bodies {
		cb.finish()
	}
	// Abort any in-flight streaming request body so a parked Read unwinds, and
	// close every kept-alive agent's idle connections so no socket leaks.
	for _, s := range streams {
		s.abort()
	}
	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
	// Wake any pump parked on its flow-control channel so it sees the closed conn
	// and releases its AddPending; otherwise the loop can't reach idle to exit.
	for _, ch := range resumes {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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
