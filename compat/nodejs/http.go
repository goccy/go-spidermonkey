package nodejs

// http.go: the Go half of node:http (js/http.js). Go's net/http owns
// accept/parse/keep-alive; each request goroutine posts a dispatch onto the
// event loop and blocks until the guest ends the response through the
// http_respond/http_write/http_end ops (which run on the loop goroutine and
// write straight to the pending ResponseWriter).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

type httpState struct {
	mu      sync.Mutex
	nextID  int64
	servers map[int64]*httpServer
	reqs    map[int64]*httpPending
}

type httpServer struct {
	id  int64
	ln  net.Listener
	srv *http.Server
	rt  *Runtime
}

// httpCmd is one ordered response action handed from the loop goroutine to the
// request's own goroutine (which owns the ResponseWriter and where the actual
// socket writes block).
type cmdKind int

const (
	cmdHead cmdKind = iota
	cmdWrite
	cmdEnd
)

type httpCmd struct {
	kind      cmdKind
	status    int
	headers   [][2]string
	data      []byte
	onWritten *spidermonkey.Object // cmdWrite: fired (posted to the loop) once the chunk is flushed — this IS the write backpressure
}

type httpPending struct {
	w        http.ResponseWriter
	serverID int64 // the server this request belongs to
	rt       *Runtime

	mu      sync.Mutex
	queue   []httpCmd
	ended   bool          // cmdEnd enqueued
	closed  bool          // the serving goroutine has exited; further ops are no-ops
	stopReq bool          // server close asked the serving goroutine to stop
	wake    chan struct{} // buffered(1): new work or end

	done        chan struct{}
	closeOnce   sync.Once
	wroteHeader bool          // only touched by the serving goroutine
	resume      chan struct{} // buffered(1): the guest asks the body pump for the next chunk
}

func (p *httpPending) finish() { p.closeOnce.Do(func() { close(p.done) }) }

// enqueue hands a response command to the serving goroutine without blocking
// the loop. It reports false if the response is already ended/closed, so the
// caller can dispose of any callback the command carried.
func (p *httpPending) enqueue(c httpCmd) bool {
	p.mu.Lock()
	if p.closed || p.ended {
		p.mu.Unlock()
		return false
	}
	p.queue = append(p.queue, c)
	if c.kind == cmdEnd {
		p.ended = true
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return true
}

// serve runs on the request's net/http goroutine: it drains queued commands
// and performs the (blocking) socket writes HERE, off the loop goroutine, so a
// slow client can never freeze the loop. onWritten callbacks are posted back to
// the loop as each chunk is flushed, which paces the guest Writable.
func (p *httpPending) serve(ctx context.Context) {
	for {
		p.mu.Lock()
		q := p.queue
		p.queue = nil
		stop := p.stopReq
		p.mu.Unlock()

		for _, c := range q {
			switch c.kind {
			case cmdHead:
				if !p.wroteHeader {
					h := p.w.Header()
					for _, kv := range c.headers {
						h.Add(kv[0], kv[1])
					}
					p.wroteHeader = true
					p.w.WriteHeader(c.status)
				}
			case cmdWrite:
				if !p.wroteHeader {
					p.wroteHeader = true
					p.w.WriteHeader(http.StatusOK)
				}
				if len(c.data) > 0 {
					p.w.Write(c.data)
					if f, ok := p.w.(http.Flusher); ok {
						f.Flush()
					}
				}
				p.postCallback(c.onWritten)
			case cmdEnd:
				if !p.wroteHeader {
					p.wroteHeader = true
					p.w.WriteHeader(http.StatusOK)
				}
				p.shutdown(nil)
				return
			}
		}
		if stop {
			p.shutdown(nil)
			return
		}

		select {
		case <-p.wake:
		case <-ctx.Done():
			p.shutdown(ctx.Err())
			return
		}
	}
}

// stop asks the serving goroutine to finish (server close). w access stays on
// that goroutine.
func (p *httpPending) stop() {
	p.mu.Lock()
	p.stopReq = true
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// postCallback fires a write's completion callback on the loop goroutine and
// frees its handle. A nil callback is a no-op.
func (p *httpPending) postCallback(cb *spidermonkey.Object) {
	if cb == nil {
		return
	}
	p.rt.loop.Post(func() error {
		cb.Call()
		cb.Free()
		return nil
	})
}

// shutdown marks the response closed, unregisters it, releases any still-queued
// write callbacks (so the guest Writable is not left hanging), and finishes.
func (p *httpPending) shutdown(_ error) {
	p.mu.Lock()
	p.closed = true
	leftover := p.queue
	p.queue = nil
	p.mu.Unlock()
	for _, c := range leftover {
		if c.kind == cmdWrite {
			p.postCallback(c.onWritten)
		}
	}
	p.finish()
}

func (rt *Runtime) httpOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"http_listen":      rt.opHTTPListen,
		"http_close":       rt.opHTTPClose,
		"http_respond":     rt.opHTTPRespond,
		"http_write":       rt.opHTTPWrite,
		"http_end":         rt.opHTTPEnd,
		"http_body_resume": rt.opHTTPBodyResume,
	}
}

// opHTTPBodyResume lets the guest request the next request-body chunk (called
// from IncomingMessage._read), driving the pump's flow control.
func (rt *Runtime) opHTTPBodyResume(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	if p := rt.pendingReq(args[0]); p != nil && p.resume != nil {
		select {
		case p.resume <- struct{}{}:
		default: // already primed; the pump will read on the next turn
		}
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opHTTPListen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("http_listen: (host, port) required")
	}
	host, port := args[0].String(), args[1].Int()
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	if cfg.Listen == nil || !cfg.Listen("tcp", addr) {
		return spidermonkey.ValueOf(map[string]any{
			"code": "EACCES", "message": fmt.Sprintf("listen %s: permission denied", addr),
		}), nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}

	st := rt.http
	st.mu.Lock()
	st.nextID++
	s := &httpServer{id: st.nextID, ln: ln, rt: rt}
	s.srv = &http.Server{Handler: s}
	st.servers[s.id] = s
	st.mu.Unlock()

	rt.loop.AddPending() // a listening server keeps the loop alive
	go s.srv.Serve(ln)
	return spidermonkey.ValueOf(map[string]any{
		"id": s.id, "port": ln.Addr().(*net.TCPAddr).Port,
	}), nil
}

func (rt *Runtime) opHTTPClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.http
	st.mu.Lock()
	s, ok := st.servers[int64(args[0].Float())]
	if ok {
		delete(st.servers, s.id)
	}
	// Ask only THIS server's in-flight requests to stop; other servers keep
	// theirs. The serving goroutines unregister themselves as they exit.
	var stopping []*httpPending
	if ok {
		for _, p := range st.reqs {
			if p.serverID == s.id {
				stopping = append(stopping, p)
			}
		}
	}
	st.mu.Unlock()
	for _, p := range stopping {
		p.stop()
	}
	if ok {
		s.srv.Close()
		rt.loop.DonePending()
	}
	return spidermonkey.Undefined(), nil
}

// ServeHTTP runs on a Go server goroutine: it registers the pending
// response, posts the dispatch to the loop goroutine, and blocks until the
// guest ends the response (or the client goes away).
func (s *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.rt.http
	p := &httpPending{w: w, serverID: s.id, rt: s.rt, wake: make(chan struct{}, 1), done: make(chan struct{})}
	st.mu.Lock()
	st.nextID++
	reqID := st.nextID
	st.reqs[reqID] = p
	st.mu.Unlock()

	headerPairs := make([][2]string, 0, len(r.Header)+1)
	// Go's net/http lifts Host out of the header map; Node exposes it as a
	// plain header (req.hostname reads it). Content-Length stays in
	// r.Header for server requests — do NOT add it again, a duplicate
	// ("7, 7") breaks type-is's body detection.
	if r.Host != "" {
		headerPairs = append(headerPairs, [2]string{"Host", r.Host})
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			headerPairs = append(headerPairs, [2]string{k, v})
		}
	}

	rt := s.rt
	hasBody := r.ContentLength != 0 || r.Header.Get("Transfer-Encoding") != ""
	if hasBody {
		// Allocate the flow-control channel BEFORE dispatch: the guest's _read can
		// fire (and call http_body_resume) as soon as the dispatch Post runs on the
		// loop, which races the pump-goroutine setup below if p.resume isn't set yet.
		p.resume = make(chan struct{}, 1)
		p.resume <- struct{}{} // prime: allow the first read
	}
	rt.loop.Post(func() error {
		// Dispatch immediately with NO buffered body; the request body streams
		// in via __node_http_body chunks (below).
		_, cerr := rt.httpDispatch.Call(
			spidermonkey.ValueOf(s.id),
			spidermonkey.ValueOf(reqID),
			spidermonkey.ValueOf(r.Method),
			spidermonkey.ValueOf(r.URL.RequestURI()),
			spidermonkey.ValueOf(headerPairs),
			spidermonkey.ValueOf(hasBody),
			spidermonkey.ValueOf(r.RemoteAddr),
			spidermonkey.ValueOf(r.TLS != nil),
		)
		if cerr != nil {
			// Engine-level failure: answer 500 and keep the loop alive.
			s.fail(reqID, p)
		}
		return nil
	})

	// Stream the request body incrementally to the guest IncomingMessage. The
	// pump is the SOLE reader of r.Body; ServeHTTP waits for it below so it can
	// never read the body after ServeHTTP returns (which the net/http contract
	// forbids and would race net/http's own post-handler body drain).
	var pumpDone chan struct{}
	if hasBody {
		pumpDone = make(chan struct{})
		go func() {
			defer close(pumpDone)
			buf := make([]byte, 32<<10)
			for {
				// Flow control: read the next chunk only when the guest
				// IncomingMessage asked for more (its _read → http_body_resume),
				// so a handler that stops (or never) reads the body can't force
				// the whole body into guest memory (which, at MaxMemoryBytes,
				// would abort the entire instance). Bounds in-flight to ~1 chunk.
				select {
				case <-p.resume:
				case <-p.done: // response finished (handler didn't drain the body)
					return
				case <-r.Context().Done():
					return
				}
				n, rerr := r.Body.Read(buf)
				if n > 0 {
					chunk := append([]byte(nil), buf[:n]...)
					rt.loop.Post(func() error {
						u8, e := rt.js.NewBytes(chunk)
						if e == nil {
							rt.httpBody.Call(spidermonkey.ValueOf(reqID), u8)
							u8.Free()
						}
						return nil
					})
				}
				if rerr != nil {
					// io.EOF is a clean end-of-body (all Content-Length bytes
					// received). Anything else — ErrUnexpectedEOF, a reset, a read
					// deadline — means the body was truncated; signal that to the
					// guest as an abort (false) rather than a clean end (null) so a
					// handler doesn't persist a partial upload as if it were whole.
					aborted := rerr != io.EOF
					rt.loop.Post(func() error {
						if aborted {
							rt.httpBody.Call(spidermonkey.ValueOf(reqID), spidermonkey.ValueOf(false))
						} else {
							rt.httpBody.Call(spidermonkey.ValueOf(reqID), spidermonkey.Null())
						}
						return nil
					})
					return
				}
			}
		}()
	} else {
		rt.loop.Post(func() error {
			rt.httpBody.Call(spidermonkey.ValueOf(reqID), spidermonkey.Null())
			return nil
		})
	}

	// Drain response commands here (this goroutine owns w); blocking socket
	// writes happen off the loop. Returns when the guest ends the response or
	// the client disconnects.
	p.serve(r.Context())

	// Ensure the body pump has stopped before returning — no read may outlive
	// ServeHTTP. If it already finished (body fully consumed, the common case),
	// leave the connection untouched so keep-alive still works. Only if it is
	// still blocked in a read (the guest responded before consuming the body)
	// force that read to unblock with a past read deadline (which correctly
	// ends keep-alive for this undrained connection), then join it.
	if pumpDone != nil {
		select {
		case <-pumpDone:
		default:
			http.NewResponseController(w).SetReadDeadline(time.Now())
			<-pumpDone
		}
	}
	st.mu.Lock()
	delete(st.reqs, reqID)
	st.mu.Unlock()
}

// fail answers 500 for a request whose dispatch never reached the guest. The
// response is written through the serving goroutine (which owns w) like any
// other, so there is no cross-goroutine ResponseWriter access.
func (s *httpServer) fail(reqID int64, p *httpPending) {
	p.enqueue(httpCmd{kind: cmdHead, status: http.StatusInternalServerError})
	p.enqueue(httpCmd{kind: cmdWrite, data: []byte("internal error")})
	p.enqueue(httpCmd{kind: cmdEnd})
}

// pendingReq looks up (without removing) the pending response for an op.
func (rt *Runtime) pendingReq(v spidermonkey.Value) *httpPending {
	st := rt.http
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.reqs[int64(v.Float())]
}

func (rt *Runtime) opHTTPRespond(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("http_respond: (reqId, status, headersJSON) required")
	}
	p := rt.pendingReq(args[0])
	if p == nil {
		return spidermonkey.Undefined(), nil
	}
	var pairs [][2]string
	if err := json.Unmarshal([]byte(args[2].String()), &pairs); err != nil {
		return nil, fmt.Errorf("http_respond: bad headers: %w", err)
	}
	status := args[1].Int()
	if status < 100 || status > 999 {
		status = http.StatusInternalServerError
	}
	p.enqueue(httpCmd{kind: cmdHead, status: status, headers: pairs})
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opHTTPWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("http_write: (reqId, data) required")
	}
	p := rt.pendingReq(args[0])
	// onWritten (args[2]) is the guest Writable's _write callback: firing it
	// only after the chunk is flushed is what paces the guest (backpressure).
	var onWritten *spidermonkey.Object
	if len(args) > 2 {
		onWritten = args[2].Object()
	}
	if p == nil {
		rt.fireWriteCallback(onWritten)
		return spidermonkey.Undefined(), nil
	}
	var data []byte
	if o := args[1].Object(); o != nil {
		var err error
		data, err = o.Bytes()
		o.Free()
		if err != nil {
			rt.fireWriteCallback(onWritten)
			return nil, err
		}
	} else {
		data = []byte(args[1].String())
	}
	if !p.enqueue(httpCmd{kind: cmdWrite, data: data, onWritten: onWritten}) {
		// Response already ended/closed: still unblock the guest Writable.
		rt.fireWriteCallback(onWritten)
	}
	return spidermonkey.Undefined(), nil
}

// fireWriteCallback invokes a write callback on the loop and frees it, for the
// paths where no serving goroutine will (request already gone).
func (rt *Runtime) fireWriteCallback(cb *spidermonkey.Object) {
	if cb == nil {
		return
	}
	rt.loop.Post(func() error {
		cb.Call()
		cb.Free()
		return nil
	})
}

func (rt *Runtime) opHTTPEnd(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	if p := rt.pendingReq(args[0]); p != nil {
		p.enqueue(httpCmd{kind: cmdEnd})
	}
	return spidermonkey.Undefined(), nil
}

// closeHTTP tears down every server (Runtime.Close).
func (rt *Runtime) closeHTTP() {
	st := rt.http
	st.mu.Lock()
	servers := make([]*httpServer, 0, len(st.servers))
	for _, s := range st.servers {
		servers = append(servers, s)
	}
	st.servers = map[int64]*httpServer{}
	pending := make([]*httpPending, 0, len(st.reqs))
	for id, p := range st.reqs {
		delete(st.reqs, id)
		pending = append(pending, p)
	}
	st.mu.Unlock()
	for _, p := range pending {
		p.finish()
	}
	for _, s := range servers {
		s.srv.Close()
		rt.loop.DonePending()
	}
}
