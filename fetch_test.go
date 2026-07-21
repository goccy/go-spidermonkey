package spidermonkey_test

// Feasibility proof for the fetch API on go-spidermonkey, composed the way
// syumai/workers' jsutil composes streams: EVERYTHING lives on the Go side,
// wired into the guest through the public object API — no __go_* helper
// globals. fetch itself is a Go closure (JS.NewFunction) assigned to
// globalThis.fetch; the Response is a plain guest object whose fields are Set
// from Go and whose methods (headers.get, bytes, text, json, arrayBuffer) are
// Go closures; the body is a ReadableStream CONSTRUCTED from Go (Object.New)
// around an underlyingSource whose pull/cancel are Go closures over the
// http.Response body — the exact ConvertReaderToReadableStream shape:
//
//	source, _ := js.NewObject()
//	source.DefineFunc("pull", st.pull)         // body.Read + enqueue
//	source.DefineFunc("cancel", st.cancel)
//	body, _ := readableStreamClass.New(source) // new ReadableStream(source)
//
// Each pull reads ONE chunk from the HTTP body and enqueues it as a real
// Uint8Array (JS.NewBytes), so chunked/flushed responses arrive incrementally.
// A Uint8Array request body crosses guest -> host via Object.Bytes.
//
// The single piece of JS left is the ReadableStream class polyfill below —
// a platform-class gap (the engine is pure ECMA-262), and its promise /
// async-iterator semantics are genuinely JS-shaped. Everything fetch-specific
// is Go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// readableStreamPolyfill fills the engine's missing platform class. It is
// deliberately host-agnostic: it calls whatever underlyingSource the
// constructor received — here, Go closures. Spec subset: getReader/read/
// releaseLock/cancel, values()/[Symbol.asyncIterator] (for-await; break
// cancels the stream unless preventCancel).
const readableStreamPolyfill = `(() => {
	class ReadableStreamDefaultReader {
		constructor(stream) {
			if (stream._locked) throw new TypeError("ReadableStream is locked");
			stream._locked = true;
			this._stream = stream;
		}
		read() {
			const s = this._stream;
			if (!s) return Promise.reject(new TypeError("reader has released its lock"));
			try {
				// A synchronous pull MUST make progress (enqueue, close, or
				// error): "no data yet" is inexpressible here, so retrying a
				// non-progressing pull could only busy-spin. One call, then
				// treat silence as a contract violation.
				if (s._queue.length === 0 && !s._closed && !s._errored) s._pull();
				if (s._queue.length > 0) return Promise.resolve({ value: s._queue.shift(), done: false });
				if (s._errored) return Promise.reject(s._errorValue);
				if (s._closed) return Promise.resolve({ value: undefined, done: true });
				return Promise.reject(new TypeError("underlyingSource.pull made no progress"));
			} catch (e) {
				return Promise.reject(e);
			}
		}
		releaseLock() { if (this._stream) { this._stream._locked = false; this._stream = null; } }
		cancel(reason) { const s = this._stream; this.releaseLock(); return s ? s.cancel(reason) : Promise.resolve(undefined); }
	}
	class ReadableStream {
		constructor(underlyingSource = {}) {
			this._source = underlyingSource;
			this._queue = [];
			this._closed = false;
			this._errored = false;
			this._errorValue = undefined;
			this._locked = false;
			const self = this;
			this._controller = {
				enqueue(chunk) { self._queue.push(chunk); },
				close() { self._closed = true; },
				error(e) { self._errored = true; self._errorValue = e; },
			};
			if (underlyingSource.start) underlyingSource.start(this._controller);
		}
		get locked() { return this._locked; }
		_pull() {
			if (this._source.pull) this._source.pull(this._controller);
			else this._closed = true;
		}
		getReader() { return new ReadableStreamDefaultReader(this); }
		cancel(reason) {
			this._queue = [];
			this._closed = true;
			if (this._source.cancel) this._source.cancel(reason);
			return Promise.resolve(undefined);
		}
		values({ preventCancel = false } = {}) {
			const reader = this.getReader();
			return {
				next() {
					return reader.read().then(r => {
						if (r.done) reader.releaseLock();
						return r;
					});
				},
				// for-await break/throw lands here; per spec the underlying
				// stream is cancelled unless preventCancel was requested.
				return(value) {
					const finish = preventCancel
						? (reader.releaseLock(), Promise.resolve(undefined))
						: reader.cancel(value);
					return Promise.resolve(finish).then(() => ({ value, done: true }));
				},
				[Symbol.asyncIterator]() { return this; },
			};
		}
		[Symbol.asyncIterator](opts) { return this.values(opts); }
	}
	globalThis.ReadableStream = ReadableStream;
})();`

// fetchAPI owns the Go side of fetch: the HTTP client, the guest builtins it
// keeps resolved (Promise, JSON, ReadableStream — looked up once, not per
// call), and the set of response bodies still open (for leak assertions and
// teardown).
type fetchAPI struct {
	js         *spidermonkey.JS
	client     *http.Client
	promiseCls *spidermonkey.Object // Promise constructor
	jsonObj    *spidermonkey.Object // JSON namespace object
	streamCls  *spidermonkey.Object // the polyfilled ReadableStream class

	mu   sync.Mutex
	open map[*fetchStream]struct{}
}

// fetchStream is one response body: the io.Reader the Go-provided pull/cancel
// closures drive. buf is allocated lazily on first pull — the bytes/text/json
// consumers never need it.
type fetchStream struct {
	api  *fetchAPI
	body io.ReadCloser
	buf  []byte
	done bool
}

func installFetch(t *testing.T, js *spidermonkey.JS) *fetchAPI {
	t.Helper()
	a := &fetchAPI{js: js, client: &http.Client{}, open: map[*fetchStream]struct{}{}}

	r, err := js.Eval(t.Context(), readableStreamPolyfill)
	if err != nil {
		t.Fatalf("polyfill eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("polyfill threw: %v", r.Error)
	}

	grab := func(name string) *spidermonkey.Object {
		v, err := js.Global().Get(name)
		if err != nil || v.Object() == nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		return v.Object()
	}
	a.promiseCls, a.jsonObj, a.streamCls = grab("Promise"), grab("JSON"), grab("ReadableStream")

	if err := js.Global().DefineFunc("fetch", a.fetch); err != nil {
		t.Fatalf("DefineFunc(fetch): %v", err)
	}

	t.Cleanup(a.closeAll)
	return a
}

// promise wraps v via the guest's own Promise[method] (resolve | reject).
func (a *fetchAPI) promise(method string, v spidermonkey.Value) (spidermonkey.Value, error) {
	return a.promiseCls.CallMethod(method, v)
}

// fetch is globalThis.fetch: (input, init?) => Promise<Response>. A transport
// failure resolves to a REJECTED promise (fetch semantics), not a throw.
func (a *fetchAPI) fetch(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fetch: an input URL is required")
	}
	url := args[0].String()
	method := "GET"
	var reqBody io.Reader
	headers := map[string]string{}

	if len(args) > 1 && args[1].IsObject() {
		init := args[1].Object()
		defer init.Free()
		if v, err := init.Get("method"); err == nil && !v.IsUndefined() {
			method = strings.ToUpper(v.String())
		}
		if v, err := init.Get("body"); err == nil {
			if o := v.Object(); o != nil {
				data, berr := o.Bytes() // Uint8Array/ArrayBuffer body: guest -> host binary
				o.Free()
				if berr != nil {
					return nil, berr
				}
				reqBody = bytes.NewReader(data)
			} else if v.Export() != nil { // string body; null/undefined mean none
				reqBody = strings.NewReader(v.String())
			}
		}
		if v, err := init.Get("headers"); err == nil {
			if o := v.Object(); o != nil {
				// Plain-object headers carry data, not identity: serialize
				// with the guest's own JSON builtin and decode host-side.
				s, serr := a.jsonObj.CallMethod("stringify", o)
				o.Free()
				if serr != nil {
					return nil, serr
				}
				if err := json.Unmarshal([]byte(s.String()), &headers); err != nil {
					return nil, fmt.Errorf("fetch: bad headers: %w", err)
				}
			}
		}
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return a.promise("reject", spidermonkey.ValueOf(err.Error()))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return a.promise("reject", spidermonkey.ValueOf(err.Error()))
	}

	respObj, err := a.newResponse(resp)
	if err != nil {
		return nil, err
	}
	return a.promise("resolve", respObj)
}

// newResponse builds the Response as a guest object composed entirely from
// Go: data fields via Set, behavior via NewFunction closures over the
// http.Response, and the body via `new ReadableStream(source)` where
// source.pull/cancel are Go closures.
func (a *fetchAPI) newResponse(resp *http.Response) (*spidermonkey.Object, error) {
	js := a.js
	r, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode <= 299
	fields := map[string]spidermonkey.Value{
		"url":        spidermonkey.ValueOf(resp.Request.URL.String()),
		"status":     spidermonkey.ValueOf(resp.StatusCode),
		"statusText": spidermonkey.ValueOf(http.StatusText(resp.StatusCode)),
		"ok":         spidermonkey.ValueOf(ok),
	}
	for name, v := range fields {
		if err := r.Set(name, v); err != nil {
			return nil, err
		}
	}

	// headers.get / headers.has: Go closures over the live http.Header —
	// case-insensitivity for free, no copying.
	hdr := resp.Header
	headersObj, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	if err := headersObj.DefineFunc("get", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if len(args) < 1 || len(hdr.Values(args[0].String())) == 0 {
			return spidermonkey.Null(), nil
		}
		return spidermonkey.ValueOf(hdr.Get(args[0].String())), nil
	}); err != nil {
		return nil, err
	}
	if err := headersObj.DefineFunc("has", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(len(args) > 0 && len(hdr.Values(args[0].String())) > 0), nil
	}); err != nil {
		return nil, err
	}
	if err := r.Set("headers", headersObj); err != nil {
		return nil, err
	}
	headersObj.Free()

	// body: new ReadableStream({ pull, cancel }) with Go closures — the
	// ConvertReaderToReadableStream shape.
	st := &fetchStream{api: a, body: resp.Body}
	a.mu.Lock()
	a.open[st] = struct{}{}
	a.mu.Unlock()

	source, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	if err := source.DefineFunc("pull", st.pull); err != nil {
		return nil, err
	}
	if err := source.DefineFunc("cancel", st.cancel); err != nil {
		return nil, err
	}
	bodyV, err := a.streamCls.New(source)
	source.Free()
	if err != nil {
		return nil, err
	}
	if err := r.Set("body", bodyV); err != nil {
		return nil, err
	}
	bodyV.Object().Free()

	// Consumers: read the REST of the body to EOF host-side and wrap the
	// result. Each returns a Promise, built with the guest's own machinery.
	consumeU8 := func() (*spidermonkey.Object, error) {
		data, err := st.readAll()
		if err != nil {
			return nil, err
		}
		return js.NewBytes(data)
	}
	if err := r.DefineFunc("bytes", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		u8, err := consumeU8()
		if err != nil {
			return nil, err
		}
		defer u8.Free()
		return a.promise("resolve", u8)
	}); err != nil {
		return nil, err
	}
	if err := r.DefineFunc("arrayBuffer", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		u8, err := consumeU8()
		if err != nil {
			return nil, err
		}
		defer u8.Free()
		buf, err := u8.Get("buffer")
		if err != nil {
			return nil, err
		}
		return a.promise("resolve", buf)
	}); err != nil {
		return nil, err
	}
	if err := r.DefineFunc("text", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		data, err := st.readAll()
		if err != nil {
			return nil, err
		}
		return a.promise("resolve", spidermonkey.ValueOf(string(data)))
	}); err != nil {
		return nil, err
	}
	if err := r.DefineFunc("json", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		data, err := st.readAll()
		if err != nil {
			return nil, err
		}
		parsed, perr := a.jsonObj.CallMethod("parse", spidermonkey.ValueOf(string(data)))
		if perr != nil {
			return a.promise("reject", spidermonkey.ValueOf(perr.Error()))
		}
		return a.promise("resolve", parsed)
	}); err != nil {
		return nil, err
	}
	return r, nil
}


// pull is the underlyingSource.pull the guest stream calls with its
// controller: read ONE chunk from the HTTP body and enqueue it as a real
// Uint8Array, or close at EOF. One Read per pull keeps the chunk boundaries
// the transport delivered.
func (st *fetchStream) pull(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 || args[0].Object() == nil {
		return nil, fmt.Errorf("pull: want the stream controller")
	}
	ctrl := args[0].Object()
	defer ctrl.Free() // the stream owns its controller; drop the arg pin
	if st.done {
		_, err := ctrl.CallMethod("close")
		return spidermonkey.Undefined(), err
	}
	if st.buf == nil {
		st.buf = make([]byte, 32<<10)
	}
	n, err := st.body.Read(st.buf) // blocks until a chunk arrives, EOF, or an error
	if n > 0 {
		u8, uerr := st.api.js.NewBytes(st.buf[:n]) // host -> guest binary
		if uerr != nil {
			return nil, uerr
		}
		_, cerr := ctrl.CallMethod("enqueue", u8)
		u8.Free() // the stream queue references the array now
		if cerr != nil {
			return nil, cerr
		}
		return spidermonkey.Undefined(), nil
	}
	if err == io.EOF {
		st.finish()
		_, cerr := ctrl.CallMethod("close")
		return spidermonkey.Undefined(), cerr
	}
	if err != nil {
		st.finish()
		return nil, err // throws -> the pending read() rejects
	}
	// (0, nil) — legal for io.Reader but "discouraged", and impossible for an
	// http body (Read blocks until data, EOF, or failure). A sync pull must
	// make progress, so surface it instead of silently returning.
	return nil, fmt.Errorf("response body read made no progress")
}

// cancel is the underlyingSource.cancel: the guest is done with the body
// (for-await break, reader.cancel), so release the HTTP side.
func (st *fetchStream) cancel(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	st.finish()
	return spidermonkey.Undefined(), nil
}

// readAll drains the remaining body to EOF (the bytes/text/json path).
func (st *fetchStream) readAll() ([]byte, error) {
	if st.done {
		return nil, nil
	}
	data, err := io.ReadAll(st.body)
	st.finish()
	return data, err
}

func (st *fetchStream) finish() {
	a := st.api
	a.mu.Lock()
	defer a.mu.Unlock()
	if st.done {
		return
	}
	st.done = true
	st.body.Close()
	delete(a.open, st)
}

// openStreams reports how many response bodies are still open; tests use it
// to prove completion/cancellation released the HTTP side.
func (a *fetchAPI) openStreams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.open)
}

func (a *fetchAPI) closeAll() {
	a.mu.Lock()
	streams := make([]*fetchStream, 0, len(a.open))
	for st := range a.open {
		streams = append(streams, st)
	}
	a.mu.Unlock()
	for _, st := range streams {
		st.finish()
	}
	for _, o := range []*spidermonkey.Object{a.promiseCls, a.jsonObj, a.streamCls} {
		if o != nil {
			o.Free()
		}
	}
}

// binPattern is the /bin fixture: 4 KiB covering every byte value, so any
// string-channel corruption (NUL, invalid UTF-8) would be caught.
func binPattern() []byte {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	return data
}

func newFetchTestServer(t *testing.T, echoed *[]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "こんにちは, fetch! 🎉")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"go-spidermonkey","n":42}`)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(binPattern())
	})
	mux.HandleFunc("/chunks", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a Flusher")
			return
		}
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "chunk-%d;", i)
			f.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if echoed != nil {
			*echoed = body
		}
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runFetchScript evaluates src (which populates globalThis.__r via promise
// chains); Eval drains the microtask queue, so the chains complete before it
// returns. Any rejection is recorded by the script itself under __r.err.
// A shared joinChunks helper is in scope for scripts that reassemble bodies.
func runFetchScript(t *testing.T, js *spidermonkey.JS, src string) {
	t.Helper()
	const prelude = `globalThis.__r = {};
		function joinChunks(chunks) {
			const joined = new Uint8Array(chunks.reduce((n, c) => n + c.byteLength, 0));
			let off = 0;
			for (const c of chunks) { joined.set(c, off); off += c.byteLength; }
			return joined;
		}
	`
	r, err := js.Eval(t.Context(), prelude+src)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	if errv := evalValue(t, js, `__r.err === undefined ? "" : String(__r.err)`); errv.String() != "" {
		t.Fatalf("fetch chain rejected: %s", errv.String())
	}
}

func newFetchJS(t *testing.T, base string) (*spidermonkey.JS, *fetchAPI) {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	a := installFetch(t, js)
	if err := js.Global().Set("BASE", spidermonkey.ValueOf(base)); err != nil {
		t.Fatalf("Set(BASE): %v", err)
	}
	return js, a
}

// bytesOf reads the Uint8Array stored at the global expression back into Go.
func bytesOf(t *testing.T, js *spidermonkey.JS, expr string) []byte {
	t.Helper()
	obj := evalValue(t, js, expr).Object()
	if obj == nil {
		t.Fatalf("%s is not an object", expr)
	}
	defer obj.Free()
	b, err := obj.Bytes()
	if err != nil {
		t.Fatalf("Bytes(%s): %v", expr, err)
	}
	return b
}

func TestFetchTextAndJSON(t *testing.T) {
	srv := newFetchTestServer(t, nil)
	js, _ := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		Promise.all([
			fetch(BASE + "/text").then(res => {
				__r.status = res.status;
				__r.ok = res.ok;
				__r.contentType = res.headers.get("Content-Type");
				__r.missing = res.headers.get("X-Missing");
				return res.text();
			}).then(text => { __r.text = text; }),
			fetch(BASE + "/json").then(res => res.json()).then(j => {
				__r.name = j.name;
				__r.n = j.n;
			}),
		]).catch(e => { __r.err = e; });
	`)

	if got := evalValue(t, js, `__r.status`).Int(); got != 200 {
		t.Errorf("status = %d, want 200", got)
	}
	if !evalValue(t, js, `__r.ok`).Bool() {
		t.Errorf("ok = false, want true")
	}
	if got := evalValue(t, js, `__r.contentType`).String(); got != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q", got)
	}
	if got := evalValue(t, js, `__r.missing === null`); !got.Bool() {
		t.Errorf("missing header: want null")
	}
	if got := evalValue(t, js, `__r.text`).String(); got != "こんにちは, fetch! 🎉" {
		t.Errorf("text = %q, want the UTF-8 greeting", got)
	}
	if got := evalValue(t, js, `__r.name`).String(); got != "go-spidermonkey" {
		t.Errorf("json name = %q", got)
	}
	if got := evalValue(t, js, `__r.n`).Int(); got != 42 {
		t.Errorf("json n = %d, want 42", got)
	}
}

func TestFetchBinaryBody(t *testing.T) {
	srv := newFetchTestServer(t, nil)
	js, _ := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		fetch(BASE + "/bin").then(res => res.bytes()).then(u8 => {
			__r.isU8 = u8 instanceof Uint8Array;
			__r.bin = u8;
		}).catch(e => { __r.err = e; });
	`)

	if !evalValue(t, js, `__r.isU8`).Bool() {
		t.Fatalf("bytes() did not resolve to a Uint8Array")
	}
	if got, want := bytesOf(t, js, `__r.bin`), binPattern(); !bytes.Equal(got, want) {
		t.Fatalf("binary body corrupted crossing the bridge: got %d bytes, want %d byte-exact", len(got), len(want))
	}
}

func TestFetchStreamsChunksIncrementally(t *testing.T) {
	srv := newFetchTestServer(t, nil)
	js, _ := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		fetch(BASE + "/chunks").then(async res => {
			__r.isStream = res.body instanceof ReadableStream;
			const reader = res.body.getReader();
			const chunks = [];
			let allU8 = true;
			for (;;) {
				const { value, done } = await reader.read();
				if (done) break;
				allU8 = allU8 && value instanceof Uint8Array;
				chunks.push(value);
			}
			__r.allU8 = allU8;
			__r.count = chunks.length;
			__r.joined = joinChunks(chunks);
		}).catch(e => { __r.err = e; });
	`)

	if !evalValue(t, js, `__r.isStream`).Bool() {
		t.Errorf("res.body is not a ReadableStream")
	}
	if !evalValue(t, js, `__r.allU8`).Bool() {
		t.Errorf("stream chunks are not Uint8Arrays")
	}
	if got := string(bytesOf(t, js, `__r.joined`)); got != "chunk-0;chunk-1;chunk-2;chunk-3;chunk-4;" {
		t.Errorf("reassembled body = %q", got)
	}
	// The server flushed 5 chunks 10ms apart; a buffered (non-streaming)
	// bridge would deliver exactly 1. Allow transport coalescing, but demand
	// more than one read to prove chunks crossed incrementally.
	if got := evalValue(t, js, `__r.count`).Int(); got < 2 {
		t.Errorf("read %d chunk(s); body was not streamed incrementally", got)
	}
}

func TestFetchPostUint8ArrayBody(t *testing.T) {
	var echoed []byte
	srv := newFetchTestServer(t, &echoed)
	js, _ := newFetchJS(t, srv.URL)

	payload := make([]byte, 8192)
	rand.New(rand.NewSource(7)).Read(payload)
	u8, err := js.NewBytes(payload)
	if err != nil {
		t.Fatalf("NewBytes: %v", err)
	}
	defer u8.Free()
	if err := js.Global().Set("payload", u8); err != nil {
		t.Fatalf("Set(payload): %v", err)
	}

	runFetchScript(t, js, `
		fetch(BASE + "/echo", {
			method: "POST",
			body: payload,
			headers: { "Content-Type": "application/octet-stream" },
		}).then(res => res.bytes()).then(u8 => { __r.echo = u8; })
			.catch(e => { __r.err = e; });
	`)

	// The server must have received the exact bytes (guest -> host -> HTTP).
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("server received %d bytes, want the %d-byte payload byte-exact", len(echoed), len(payload))
	}
	// And the echoed response must round-trip back (HTTP -> host -> guest -> host).
	if got := bytesOf(t, js, `__r.echo`); !bytes.Equal(got, payload) {
		t.Fatalf("echoed body is not byte-exact")
	}
}

func TestFetchStringBody(t *testing.T) {
	var echoed []byte
	srv := newFetchTestServer(t, &echoed)
	js, _ := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		fetch(BASE + "/echo", { method: "POST", body: "hello, ボディ" })
			.then(res => res.text()).then(text => { __r.echo = text; })
			.catch(e => { __r.err = e; });
	`)

	if got := string(echoed); got != "hello, ボディ" {
		t.Errorf("server received %q", got)
	}
	if got := evalValue(t, js, `__r.echo`).String(); got != "hello, ボディ" {
		t.Errorf("echo = %q", got)
	}
}

func TestFetchBodyAsyncIteration(t *testing.T) {
	srv := newFetchTestServer(t, nil)
	js, a := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		(async () => {
			const response = await fetch(BASE + "/chunks");
			const chunks = [];
			let allU8 = true;
			for await (const chunk of response.body) {
				allU8 = allU8 && chunk instanceof Uint8Array;
				chunks.push(chunk);
			}
			__r.allU8 = allU8;
			__r.count = chunks.length;
			__r.joined = joinChunks(chunks);
		})().catch(e => { __r.err = e; });
	`)

	if !evalValue(t, js, `__r.allU8`).Bool() {
		t.Errorf("iterated chunks are not Uint8Arrays")
	}
	if got := string(bytesOf(t, js, `__r.joined`)); got != "chunk-0;chunk-1;chunk-2;chunk-3;chunk-4;" {
		t.Errorf("reassembled body = %q", got)
	}
	if got := evalValue(t, js, `__r.count`).Int(); got < 2 {
		t.Errorf("iterated %d chunk(s); body was not streamed incrementally", got)
	}
	if got := a.openStreams(); got != 0 {
		t.Errorf("%d stream(s) still open after full iteration; EOF must release the host side", got)
	}
}

// TestFetchBodyIterationBreakCancelsStream: breaking out of the for-await
// loop calls the async iterator's return(), which per the Streams spec
// cancels the stream — the guest calls the Go-provided source.cancel, which
// must close the HTTP response body (no leaked connection).
func TestFetchBodyIterationBreakCancelsStream(t *testing.T) {
	srv := newFetchTestServer(t, nil)
	js, a := newFetchJS(t, srv.URL)

	runFetchScript(t, js, `
		(async () => {
			const response = await fetch(BASE + "/chunks");
			let count = 0;
			for await (const chunk of response.body) {
				count++;
				if (count === 2) break; // just exit the loop
			}
			__r.count = count;
		})().catch(e => { __r.err = e; });
	`)

	if got := evalValue(t, js, `__r.count`).Int(); got != 2 {
		t.Errorf("processed %d chunks before the break, want 2", got)
	}
	if got := a.openStreams(); got != 0 {
		t.Errorf("%d stream(s) still open after break; for-await break must cancel the stream and close the HTTP body", got)
	}
}

func TestFetchRejectsOnNetworkError(t *testing.T) {
	js, _ := newFetchJS(t, "http://127.0.0.1:1")

	r, err := js.Eval(t.Context(), `
		globalThis.__r = {};
		fetch(BASE + "/nope").then(res => { __r.resolved = true; })
			.catch(e => { __r.err = String(e); });
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	if evalValue(t, js, `__r.resolved === true`).Bool() {
		t.Fatalf("fetch to an unreachable host resolved; want rejection")
	}
	if got := evalValue(t, js, `String(__r.err)`).String(); got == "" || got == "undefined" {
		t.Fatalf("no rejection recorded; want a network error")
	}
}
