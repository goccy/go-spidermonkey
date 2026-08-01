// Package cfworkers runs Cloudflare-Workers-style modules — `export default
// { fetch(request, env, ctx) }` — on go-spidermonkey behind Go's net/http.
//
// The serving model is goroutine-per-request over a pool of warmed engine
// instances (the sql.DB shape): Go owns accept/TLS/HTTP parsing, the guest
// sees Request → Response. Env bindings are the sanctioned way to hand Go
// assets (database handles, caches, host services) to the worker.
//
//	pool, err := cfworkers.NewPool(cfworkers.PoolConfig{
//		Source: workerJS, // export default { fetch }
//		Env: map[string]cfworkers.Binding{
//			"GREETING": cfworkers.Static("hello"),
//		},
//	})
//	http.ListenAndServe(":8080", pool)
//
// Instances are reused across requests, so module-level state persists
// (exactly like a warm Workers isolate). ctx.waitUntil work is drained after
// the response is written, before the instance returns to the pool.
package cfworkers

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

//go:embed js/glue.js
var glueJS string

// Binding produces the guest value for one env entry, once per instance at
// warm-up. It may build objects or functions against the instance (e.g. a
// query function closing over a *sql.DB).
type Binding func(js *spidermonkey.JS) (spidermonkey.Value, error)

// Static is a Binding for plain data (anything encoding/json can marshal —
// it materializes guest-side as a fresh value per instance).
func Static(v any) Binding {
	return func(*spidermonkey.JS) (spidermonkey.Value, error) { return spidermonkey.ValueOf(v), nil }
}

// PoolConfig configures NewPool.
type PoolConfig struct {
	// Config is the per-instance engine config — including the permission
	// hooks, which govern everything the worker can reach.
	Config spidermonkey.Config
	// Size is the number of warmed instances (max concurrent requests).
	// Zero means GOMAXPROCS.
	Size int
	// Source is the worker module: `export default { fetch(req, env, ctx) }`.
	Source string
	// Env maps binding names to their constructors.
	Env map[string]Binding
	// Loader, when non-nil, is installed as the fallback module loader, so
	// the worker module's own imports resolve — e.g. nodejs.ESMLoader with
	// Config.FS pointing at an app directory containing node_modules.
	Loader spidermonkey.ModuleLoader
	// DrainTimeout bounds the post-response waitUntil drain before an
	// instance returns to the pool. Zero means 30s.
	DrainTimeout time.Duration
}

// Pool is a fixed-size pool of warmed worker instances. It implements
// http.Handler: each request checks out an instance for its duration.
type Pool struct {
	workers      chan *worker
	size         int
	drainTimeout time.Duration
}

// NewPool builds and warms the pool: every instance boots the module and
// resolves its handler before NewPool returns.
func NewPool(cfg PoolConfig) (*Pool, error) {
	size := cfg.Size
	if size <= 0 {
		size = runtime.GOMAXPROCS(0)
	}
	drain := cfg.DrainTimeout
	if drain <= 0 {
		drain = 30 * time.Second
	}
	p := &Pool{workers: make(chan *worker, size), size: size, drainTimeout: drain}
	var created []*worker
	for i := 0; i < size; i++ {
		w, err := newWorker(cfg)
		if err != nil {
			for _, cw := range created {
				cw.close()
			}
			return nil, fmt.Errorf("cfworkers: warming instance %d: %w", i, err)
		}
		created = append(created, w)
	}
	for _, w := range created {
		p.workers <- w
	}
	return p, nil
}

// Close shuts down every pooled instance. In-flight requests keep their
// instance until they finish; Close reclaims instances as they return.
func (p *Pool) Close() error {
	var firstErr error
	for i := 0; i < p.size; i++ {
		select {
		case w := <-p.workers:
			if err := w.close(); err != nil && firstErr == nil {
				firstErr = err
			}
		case <-time.After(p.drainTimeout):
			return fmt.Errorf("cfworkers: close timed out waiting for busy instances")
		}
	}
	p.size = 0
	return firstErr
}

// ServeHTTP checks an instance out of the pool for the request's duration.
func (p *Pool) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	select {
	case w := <-p.workers:
		defer func() { p.workers <- w }()
		w.serve(rw, req, p.drainTimeout)
	case <-req.Context().Done():
		http.Error(rw, "no worker available", http.StatusServiceUnavailable)
	}
}

// Scheduled fires the worker's scheduled() (cron) handler on a pooled
// instance and drives the loop until it settles. cron is the schedule
// expression; scheduledTimeMs is the trigger time in Unix ms.
func (p *Pool) Scheduled(ctx context.Context, cron string, scheduledTimeMs int64) error {
	var w *worker
	select {
	case w = <-p.workers:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { p.workers <- w }()
	return w.runNonFetch(ctx, w.runScheduled, p.drainTimeout,
		spidermonkey.ValueOf(cron), spidermonkey.ValueOf(scheduledTimeMs))
}

// Queue fires the worker's queue() handler with a batch. queueName names the
// queue; bodies are the message payloads (any JSON-encodable values).
func (p *Pool) Queue(ctx context.Context, queueName string, bodies []any) error {
	var w *worker
	select {
	case w = <-p.workers:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { p.workers <- w }()
	messages := make([]map[string]any, len(bodies))
	for i, b := range bodies {
		messages[i] = map[string]any{"id": fmt.Sprint(i), "timestamp": 0, "body": b}
	}
	batch, err := json.Marshal(map[string]any{"queue": queueName, "messages": messages})
	if err != nil {
		return err
	}
	return w.runNonFetch(ctx, w.runQueue, p.drainTimeout, spidermonkey.ValueOf(string(batch)))
}

// runNonFetch drives a scheduled/queue handler to completion.
func (wk *worker) runNonFetch(ctx context.Context, entry *spidermonkey.Object, drain time.Duration, args ...spidermonkey.Value) (err error) {
	// Clear leftover loop state before this instance returns to the pool, and
	// recover a handler panic so it can't poison the instance.
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
		wk.web.ResetPerRequest()
		wk.web.Loop().Reset()
	}()
	if _, err := entry.Call(args...); err != nil {
		return fmt.Errorf("invoking handler: %w", err)
	}
	// Run until the handler settles, not until fully idle (a handler may leave
	// timers running); see the fetch path for the rationale.
	stop := func() bool {
		st, serr := wk.status.Call()
		return serr != nil || st.String() != "pending"
	}
	if err := wk.web.Loop().RunUntil(ctx, stop); err != nil {
		return err
	}
	if st, _ := wk.status.Call(); st == nil || st.String() == "pending" {
		return fmt.Errorf("handler never settled")
	}
	if st, _ := wk.status.Call(); st != nil && st.String() == "error" {
		msg, _ := wk.errMsg.Call()
		return fmt.Errorf("worker error: %s", msg.String())
	}
	dctx, cancel := context.WithTimeout(context.Background(), drain)
	wk.web.Loop().RunUntil(dctx, func() bool {
		v, err := wk.waiting.Call()
		return err != nil || v.Int() == 0
	})
	cancel()
	return nil
}

// worker is one warmed engine instance plus its resolved glue functions.
type worker struct {
	js           *spidermonkey.JS
	web          *web.Web
	makeReq      *spidermonkey.Object
	run          *spidermonkey.Object
	runScheduled *spidermonkey.Object
	runQueue     *spidermonkey.Object
	status       *spidermonkey.Object
	errMsg       *spidermonkey.Object
	meta         *spidermonkey.Object
	body         *spidermonkey.Object
	needsStream  *spidermonkey.Object // __cfw_response_needs_stream(): body is a ReadableStream
	streamBody   *spidermonkey.Object // __cfw_stream_body(gen, write, done, fail): pump the stream
	waiting      *spidermonkey.Object // __cfw_waiting(): live ctx.waitUntil count

	// Streaming callbacks are created ONCE per worker: a NewFunction
	// registration lives for the interpreter's lifetime, so per-request
	// functions would permanently leak their closures (and pin every past
	// request's ResponseWriter). The shared callbacks dispatch through sink,
	// which is set for the duration of one streamed response and cleared
	// after; gen invalidates any stale pump from an earlier request so it can
	// never write into a later response.
	hostWrite *spidermonkey.Object
	hostDone  *spidermonkey.Object
	hostFail  *spidermonkey.Object
	sink      *streamSink
	streamGen int64
}

// streamSink is the per-request target of the shared streaming callbacks. The
// pump runs on the instance's event loop while RunUntil drives it from the
// serving goroutine, so no locking is needed.
type streamSink struct {
	rw       http.ResponseWriter
	fl       http.Flusher
	pumpDone bool
	failed   bool
}

func newWorker(cfg PoolConfig) (*worker, error) {
	js, err := spidermonkey.New(cfg.Config)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			js.Close()
		}
	}()

	w, err := web.Install(js)
	if err != nil {
		return nil, err
	}
	// A Workers global is a ServiceWorker-shaped scope: `self`, never
	// `window` — and code sniffs the difference (see compat/nodejs).
	if _, err := js.Eval(context.Background(),
		`for (const g of ["window", "frames", "parent", "top",
			"postMessage", "onmessage", "onmessageerror"]) delete globalThis[g];`); err != nil {
		return nil, err
	}
	wk := &worker{js: js, web: w}
	if cfg.Loader != nil {
		js.SetModuleLoader(cfg.Loader)
	}

	envObj, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	for name, bind := range cfg.Env {
		v, berr := bind(js)
		if berr != nil {
			return nil, fmt.Errorf("env binding %q: %w", name, berr)
		}
		if err := envObj.Set(name, v); err != nil {
			return nil, fmt.Errorf("env binding %q: %w", name, err)
		}
	}
	if err := js.Global().Set("__cfw_env", envObj); err != nil {
		return nil, err
	}
	envObj.Free()

	ctx := context.Background()
	if r, err := js.Eval(ctx, glueJS); err != nil {
		return nil, err
	} else if r.Error != nil {
		return nil, fmt.Errorf("glue threw: %w", r.Error)
	}

	src := cfg.Source
	js.RegisterModuleResolver("cfworkers:", func(_ spidermonkey.Config, specifier, referrer string) (string, error) {
		if specifier == "cfworkers:user" {
			return src, nil
		}
		return "", fmt.Errorf("unknown module %q", specifier)
	})
	mr, err := js.EvalModule(ctx, "cfworkers:boot",
		`import handler from "cfworkers:user"; globalThis.__cfw_handler = handler;`)
	if err != nil {
		return nil, err
	}
	if mr.Error != nil {
		return nil, fmt.Errorf("worker module threw: %w", mr.Error)
	}
	if r, err := js.Eval(ctx, `typeof __cfw_handler === "object" && __cfw_handler !== null && typeof __cfw_handler.fetch === "function"`); err != nil {
		return nil, err
	} else if !r.Value.Bool() {
		return nil, fmt.Errorf("worker module's default export has no fetch handler")
	}

	for name, dst := range map[string]**spidermonkey.Object{
		"__cfw_make_request":          &wk.makeReq,
		"__cfw_run":                   &wk.run,
		"__cfw_run_scheduled":         &wk.runScheduled,
		"__cfw_run_queue":             &wk.runQueue,
		"__cfw_status":                &wk.status,
		"__cfw_error":                 &wk.errMsg,
		"__cfw_response_meta":         &wk.meta,
		"__cfw_response_body":         &wk.body,
		"__cfw_response_needs_stream": &wk.needsStream,
		"__cfw_stream_body":           &wk.streamBody,
		"__cfw_waiting":               &wk.waiting,
	} {
		v, gerr := js.Global().Get(name)
		if gerr != nil {
			return nil, gerr
		}
		o := v.Object()
		if o == nil || !o.IsFunction() {
			return nil, fmt.Errorf("glue function %s missing", name)
		}
		*dst = o
	}
	if wk.hostWrite, err = js.NewFunction("__cfw_host_write", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("write: (gen, chunk) required")
		}
		o := args[1].Object()
		if o == nil {
			return nil, fmt.Errorf("write: chunk must be bytes")
		}
		chunk, berr := o.Bytes()
		o.Free()
		if berr != nil {
			return nil, berr
		}
		sink := wk.sink
		if sink == nil || int64(args[0].Float()) != wk.streamGen {
			// A stale pump from a finished request. Erroring it makes the guest
			// cancel its source; it must never reach a later response's writer.
			return nil, fmt.Errorf("response stream finished")
		}
		if _, werr := sink.rw.Write(chunk); werr != nil {
			// The client went away mid-stream. Throwing into the guest pump makes
			// it cancel the source stream and report fail().
			return nil, werr
		}
		if sink.fl != nil {
			sink.fl.Flush()
		}
		return spidermonkey.Undefined(), nil
	}); err != nil {
		return nil, err
	}
	if wk.hostDone, err = js.NewFunction("__cfw_host_done", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if s := wk.sink; s != nil && len(args) > 0 && int64(args[0].Float()) == wk.streamGen {
			s.pumpDone = true
		}
		return spidermonkey.Undefined(), nil
	}); err != nil {
		return nil, err
	}
	if wk.hostFail, err = js.NewFunction("__cfw_host_fail", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if s := wk.sink; s != nil && len(args) > 0 && int64(args[0].Float()) == wk.streamGen {
			s.pumpDone = true
			s.failed = true
		}
		return spidermonkey.Undefined(), nil
	}); err != nil {
		return nil, err
	}
	ok = true
	return wk, nil
}

func (wk *worker) close() error {
	wk.web.Close()
	return wk.js.Close()
}

type responseMeta struct {
	Status     int         `json:"status"`
	StatusText string      `json:"statusText"`
	Headers    [][2]string `json:"headers"`
}

// maxRequestBody caps a buffered Workers request body so a large/slow upload
// can't exhaust memory while holding a pool slot.
const maxRequestBody = 100 << 20 // 100 MiB

func (wk *worker) serve(rw http.ResponseWriter, req *http.Request, drainTimeout time.Duration) {
	ctx := req.Context()
	// A panic in the handler path (e.g. an out-of-range WriteHeader) must not
	// return a mid-operation instance to the pool. Recover, best-effort 500,
	// and always clear leftover loop state (timers/pending ops) so one
	// request's un-awaited work can't fire during the next on this instance.
	defer func() {
		if r := recover(); r != nil {
			// net/http uses ErrAbortHandler as a sentinel to abort the response
			// without logging; re-panic it (after clearing loop state) so the
			// server handles it as intended rather than turning it into a 500.
			if r == http.ErrAbortHandler {
				wk.web.ResetPerRequest()
				wk.web.Loop().Reset()
				panic(r)
			}
			func() {
				defer func() { _ = recover() }()
				http.Error(rw, "internal error", http.StatusInternalServerError)
			}()
		}
		wk.web.ResetPerRequest()
		wk.web.Loop().Reset()
	}()
	fail := func(status int, format string, args ...any) {
		http.Error(rw, fmt.Sprintf(format, args...), status)
	}

	reqBody, err := io.ReadAll(http.MaxBytesReader(rw, req.Body, maxRequestBody))
	if err != nil {
		fail(http.StatusRequestEntityTooLarge, "reading request body: %v", err)
		return
	}

	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	fullURL := scheme + "://" + req.Host + req.URL.RequestURI()

	headerPairs := make([][2]string, 0, len(req.Header))
	for k, vs := range req.Header {
		for _, v := range vs {
			headerPairs = append(headerPairs, [2]string{k, v})
		}
	}

	var bodyVal spidermonkey.Value = spidermonkey.Null()
	if len(reqBody) > 0 {
		u8, berr := wk.js.NewBytes(reqBody)
		if berr != nil {
			fail(http.StatusInternalServerError, "building request body: %v", berr)
			return
		}
		defer u8.Free()
		bodyVal = u8
	}

	reqV, err := wk.makeReq.Call(
		spidermonkey.ValueOf(req.Method),
		spidermonkey.ValueOf(fullURL),
		spidermonkey.ValueOf(headerPairs),
		bodyVal,
	)
	if err != nil {
		fail(http.StatusInternalServerError, "building Request: %v", err)
		return
	}
	reqObj := reqV.Object()
	if reqObj == nil {
		fail(http.StatusInternalServerError, "building Request: not an object")
		return
	}
	if _, err := wk.run.Call(reqObj); err != nil {
		reqObj.Free()
		fail(http.StatusInternalServerError, "invoking handler: %v", err)
		return
	}
	reqObj.Free()

	// Drive the loop until the handler's promise settles — NOT until the loop is
	// fully idle. A handler can settle while leaving timers running (a
	// setInterval, an un-fired AbortSignal.timeout); waiting for idle would delay
	// or hang the already-available response and pin the pooled instance. RunUntil
	// returns as soon as status flips, or when the loop goes idle while still
	// pending (the promise awaits something nothing will resolve).
	stop := func() bool {
		st, serr := wk.status.Call()
		return serr != nil || st.String() != "pending"
	}
	if werr := wk.web.Loop().RunUntil(ctx, stop); werr != nil {
		fail(http.StatusInternalServerError, "handler failed: %v", werr)
		return
	}
	// Check the real status after the loop returns (it may have returned because
	// the loop went idle, coinciding with the handler settling): pending here
	// means the promise awaits something nothing will resolve.
	if st, _ := wk.status.Call(); st == nil || st.String() == "pending" {
		fail(http.StatusInternalServerError, "handler never settled")
		return
	}

	if st, _ := wk.status.Call(); st != nil && st.String() == "error" {
		msg, _ := wk.errMsg.Call()
		fail(http.StatusInternalServerError, "worker error: %s", msg.String())
		return
	}

	metaV, err := wk.meta.Call()
	if err != nil {
		fail(http.StatusInternalServerError, "reading response: %v", err)
		return
	}
	var meta responseMeta
	if err := json.Unmarshal([]byte(metaV.String()), &meta); err != nil {
		fail(http.StatusInternalServerError, "decoding response meta: %v", err)
		return
	}
	// A ReadableStream body (guest-constructed or a fetch() Response returned
	// straight through) is streamed chunk-by-chunk instead of buffered, so the
	// first byte reaches the client as soon as the worker produces it.
	if v, serr := wk.needsStream.Call(); serr == nil && v.Bool() {
		wk.serveStreamingBody(ctx, rw, meta, drainTimeout)
		return
	}

	var respBody []byte
	bodyV, err := wk.body.Call()
	if err != nil {
		fail(http.StatusInternalServerError, "reading response body: %v", err)
		return
	}
	if o := bodyV.Object(); o != nil {
		respBody, err = o.Bytes()
		o.Free()
		if err != nil {
			fail(http.StatusInternalServerError, "reading response body: %v", err)
			return
		}
	}

	h := rw.Header()
	for _, kv := range meta.Headers {
		h.Add(kv[0], kv[1])
	}
	// A worker can set any status (Response.error() uses 0); an out-of-range
	// code panics net/http's WriteHeader, so clamp it.
	if meta.Status < 100 || meta.Status > 999 {
		meta.Status = http.StatusInternalServerError
	}
	// ctx.waitUntil work must run AFTER the response reaches the client, not
	// before (that is the whole point of waitUntil — schedule background
	// analytics/logging without adding latency). net/http keeps a small response
	// buffered in its bufio writer until ServeHTTP returns, so entering the
	// blocking drain loop below without flushing would withhold the response from
	// the client until the background work settled (up to drainTimeout).
	//
	// When work is pending, deliver the (fully-known) body NOW: set an explicit
	// Content-Length so the client can complete its read without waiting for the
	// connection to close, then flush. Only do this when waitUntil work is
	// actually pending — the common no-waitUntil path returns immediately and net/
	// http sets Content-Length itself, so it needs no special handling.
	flushNow := false
	if v, err := wk.waiting.Call(); err == nil && v.Int() > 0 {
		flushNow = true
		if h.Get("Content-Length") == "" && h.Get("Transfer-Encoding") == "" {
			h.Set("Content-Length", strconv.Itoa(len(respBody)))
		}
	}
	rw.WriteHeader(meta.Status)
	if len(respBody) > 0 {
		rw.Write(respBody)
	}
	if flushNow {
		if fl, ok := rw.(http.Flusher); ok {
			fl.Flush()
		}
	}

	// Drain waitUntil work (best-effort, bounded) before pool reuse — but stop as
	// soon as the waitUntil promises settle, NOT when the loop goes fully idle. A
	// handler that leaves a ref'd timer running would otherwise pin this pooled
	// instance for the whole drain timeout (30s), collapsing throughput.
	wk.drainWaitUntil(drainTimeout)
}

// drainWaitUntil runs pending ctx.waitUntil work (best-effort, bounded) before
// the instance returns to the pool.
func (wk *worker) drainWaitUntil(drainTimeout time.Duration) {
	dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	wk.web.Loop().RunUntil(dctx, func() bool {
		v, err := wk.waiting.Call()
		return err != nil || v.Int() == 0
	})
	cancel()
}

// serveStreamingBody delivers a Response whose body is a ReadableStream:
// status and headers go out first, then every chunk is written and flushed as
// the worker's stream produces it (chunked transfer encoding unless the worker
// supplied a Content-Length). The pump runs on the instance's event loop; the
// write/done/fail callbacks below execute on this goroutine while RunUntil
// drives the loop, so the local state needs no locking. waitUntil work is
// drained after the stream completes, exactly like the buffered path.
func (wk *worker) serveStreamingBody(ctx context.Context, rw http.ResponseWriter, meta responseMeta, drainTimeout time.Duration) {
	h := rw.Header()
	for _, kv := range meta.Headers {
		h.Add(kv[0], kv[1])
	}
	if meta.Status < 100 || meta.Status > 999 {
		meta.Status = http.StatusInternalServerError
	}
	rw.WriteHeader(meta.Status)
	fl, _ := rw.(http.Flusher)

	// Arm the shared callbacks for this request; clear on every exit path
	// (including the abort panic) so the ResponseWriter is never pinned past
	// its request and a stale pump finds no sink.
	wk.streamGen++
	sink := &streamSink{rw: rw, fl: fl}
	wk.sink = sink
	defer func() { wk.sink = nil }()

	if _, cerr := wk.streamBody.Call(spidermonkey.ValueOf(wk.streamGen), wk.hostWrite, wk.hostDone, wk.hostFail); cerr != nil {
		sink.failed = true
	}
	if !sink.failed && !sink.pumpDone {
		wk.web.Loop().RunUntil(ctx, func() bool { return sink.pumpDone })
	}
	if !sink.pumpDone || sink.failed {
		// Status and headers (and possibly some bytes) are already on the wire;
		// the only honest failure signal left is aborting the connection so the
		// client sees truncation instead of a clean end. ErrAbortHandler is
		// re-panicked by serve's recover after the loop state is cleared.
		panic(http.ErrAbortHandler)
	}
	wk.drainWaitUntil(drainTimeout)
}
