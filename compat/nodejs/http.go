package nodejs

// http.go: the Go half of node:http (js/http.js). Go's net/http owns
// accept/parse/keep-alive; each request goroutine posts a dispatch onto the
// event loop and blocks until the guest ends the response through the
// http_respond/http_write/http_end ops (which run on the loop goroutine and
// write straight to the pending ResponseWriter).

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

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

type httpPending struct {
	w           http.ResponseWriter
	done        chan struct{}
	closeOnce   sync.Once
	wroteHeader bool
}

func (p *httpPending) finish() { p.closeOnce.Do(func() { close(p.done) }) }

func (rt *Runtime) httpOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"http_listen":  rt.opHTTPListen,
		"http_close":   rt.opHTTPClose,
		"http_respond": rt.opHTTPRespond,
		"http_write":   rt.opHTTPWrite,
		"http_end":     rt.opHTTPEnd,
	}
}

func (rt *Runtime) opHTTPListen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("http_listen: (host, port) required")
	}
	host, port := args[0].String(), args[1].Int()
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	if cfg.Listen != nil && !cfg.Listen("tcp", addr) {
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
	// Unblock any request goroutine still waiting on the guest.
	for id, p := range st.reqs {
		delete(st.reqs, id)
		p.finish()
	}
	st.mu.Unlock()
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
	p := &httpPending{w: w, done: make(chan struct{})}
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

	// Stream the request body incrementally to the guest IncomingMessage.
	if hasBody {
		go func() {
			buf := make([]byte, 32<<10)
			for {
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
					rt.loop.Post(func() error {
						rt.httpBody.Call(spidermonkey.ValueOf(reqID), spidermonkey.Null())
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

	select {
	case <-p.done:
	case <-r.Context().Done():
		// Client went away: forget the request; late guest ops become no-ops.
		st.mu.Lock()
		delete(st.reqs, reqID)
		st.mu.Unlock()
		p.finish()
	}
}

// fail answers 500 for a request whose dispatch never reached the guest.
func (s *httpServer) fail(reqID int64, p *httpPending) {
	st := s.rt.http
	st.mu.Lock()
	_, live := st.reqs[reqID]
	delete(st.reqs, reqID)
	st.mu.Unlock()
	if live && !p.wroteHeader {
		http.Error(p.w, "internal error", http.StatusInternalServerError)
	}
	p.finish()
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
	if p == nil || p.wroteHeader {
		return spidermonkey.Undefined(), nil
	}
	var pairs [][2]string
	if err := json.Unmarshal([]byte(args[2].String()), &pairs); err != nil {
		return nil, fmt.Errorf("http_respond: bad headers: %w", err)
	}
	h := p.w.Header()
	for _, kv := range pairs {
		h.Add(kv[0], kv[1])
	}
	status := args[1].Int()
	if status < 100 || status > 999 {
		status = http.StatusInternalServerError
	}
	p.wroteHeader = true
	p.w.WriteHeader(status)
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opHTTPWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("http_write: (reqId, data) required")
	}
	p := rt.pendingReq(args[0])
	if p == nil {
		return spidermonkey.Undefined(), nil
	}
	var data []byte
	if o := args[1].Object(); o != nil {
		var err error
		data, err = o.Bytes()
		o.Free()
		if err != nil {
			return nil, err
		}
	} else {
		data = []byte(args[1].String())
	}
	if len(data) > 0 {
		p.w.Write(data)
		if f, ok := p.w.(http.Flusher); ok {
			f.Flush()
		}
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opHTTPEnd(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	st := rt.http
	st.mu.Lock()
	id := int64(args[0].Float())
	p := st.reqs[id]
	delete(st.reqs, id)
	st.mu.Unlock()
	if p != nil {
		if !p.wroteHeader {
			p.wroteHeader = true
			p.w.WriteHeader(http.StatusOK)
		}
		p.finish()
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
