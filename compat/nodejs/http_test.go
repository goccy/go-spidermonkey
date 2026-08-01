package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { Readable, Writable, Transform, PassThrough, pipeline } = require("stream");
		globalThis.r = {};

		// data/end events.
		const src = new Readable({ read() {} });
		const got = [];
		src.on("data", (c) => got.push(c.toString()));
		src.on("end", () => { r.events = got.join("|"); });
		src.push("a");
		src.push("b");
		src.push(null);

		// pipe through a Transform into a Writable.
		const upper = new Transform({
			transform(chunk, enc, cb) { cb(null, chunk.toString().toUpperCase()); },
		});
		const sink = [];
		const dest = new Writable({
			write(chunk, enc, cb) { sink.push(chunk.toString()); cb(); },
		});
		dest.on("finish", () => { r.piped = sink.join(""); });
		const s2 = new Readable({ read() {} });
		s2.pipe(upper).pipe(dest);
		s2.push("hel");
		s2.push("lo");
		s2.push(null);

		// setEncoding decodes multi-byte chunks split across pushes.
		const s3 = new Readable({ read() {} });
		s3.setEncoding("utf8");
		const parts = [];
		s3.on("data", (s) => parts.push(s));
		s3.on("end", () => { r.decoded = parts.join(""); });
		const bytes = Buffer.from("こん");
		s3.push(bytes.subarray(0, 2)); // splits こ mid-sequence
		s3.push(bytes.subarray(2));
		s3.push(null);

		// pipeline helper.
		const p1 = new Readable({ read() {} });
		const p3 = [];
		pipeline(p1, new PassThrough(), new Writable({
			write(c, e, cb) { p3.push(c.toString()); cb(); },
		}), (err) => { r.pipeline = err ? String(err) : p3.join(""); });
		p1.push("xyz");
		p1.push(null);
	`)
	for expr, want := range map[string]string{
		"r.events":   "a|b",
		"r.piped":    "HELLO",
		"r.decoded":  "こん",
		"r.pipeline": "xyz",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestNodeCrypto(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		r.sha1 = crypto.createHash("sha1").update("abc").digest("hex");
		r.md5 = crypto.createHash("md5").update("abc").digest("hex");
		r.chunked = crypto.createHash("sha256").update("he").update("llo").digest("hex")
			=== crypto.createHash("sha256").update("hello").digest("hex");
		r.hmac = crypto.createHmac("sha256", "key").update("msg").digest("base64");
		const rb = crypto.randomBytes(16);
		r.rbLen = rb.length;
		r.rbIsBuf = Buffer.isBuffer(rb);
		r.tse = crypto.timingSafeEqual(Buffer.from("aa"), Buffer.from("aa"));
	`)
	// Cross-check sha1 against Go.
	sum := sha1.Sum([]byte("abc"))
	if got := evalStr(t, js, `r.sha1`); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha1 = %s, want %s", got, hex.EncodeToString(sum[:]))
	}
	if got := evalStr(t, js, `r.md5`); got != "900150983cd24fb0d6963f7d28e17f72" {
		t.Errorf("md5 = %s", got)
	}
	for _, expr := range []string{"r.chunked", "r.rbIsBuf", "r.tse"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
	if got := evalStr(t, js, `String(r.rbLen)`); got != "16" {
		t.Errorf("randomBytes length = %s", got)
	}
}

func TestLegacyURLParse(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const url = require("url");
		globalThis.r = {};
		const u1 = url.parse("/users/42?v=hi&x=1");
		r.p1 = [u1.pathname, u1.query, u1.path, String(u1.host)].join("|");
		const u2 = url.parse("https://ex.com:8443/a/b?q=z#frag", true);
		r.p2 = [u2.protocol, u2.host, u2.pathname, u2.query.q, u2.hash].join("|");
	`)
	if got := evalStr(t, js, `r.p1`); got != "/users/42|v=hi&x=1|/users/42?v=hi&x=1|null" {
		t.Errorf("path parse = %s", got)
	}
	if got := evalStr(t, js, `r.p2`); got != "https:|ex.com:8443|/a/b|z|#frag" {
		t.Errorf("full parse = %s", got)
	}
}

// startServer evaluates a server script, returns its port, and drives the
// event loop in the background until the test closes the server.
func startServer(t *testing.T, js *spidermonkey.JS, rt *nodejs.Runtime, script string) (port string, waitDone chan error) {
	t.Helper()
	ctx := context.Background()
	r, err := js.Eval(ctx, script)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("server script threw: %v", r.Error)
	}
	port = evalStr(t, js, `String(PORT)`)
	waitDone = make(chan error, 1)
	go func() { waitDone <- rt.Wait(context.Background()) }()
	t.Cleanup(func() {
		js.Eval(context.Background(), `if (globalThis.__server) __server.close()`)
		select {
		case <-waitDone:
		case <-time.After(10 * time.Second):
			t.Error("event loop did not stop after server.close()")
		}
	})
	return port, waitDone
}

func TestHTTPServerBasics(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, _ := startServer(t, js, rt, `
		const http = require("http");
		const server = http.createServer((req, res) => {
			if (req.url === "/hello") {
				res.setHeader("Content-Type", "text/plain");
				res.setHeader("X-Custom", "yes");
				res.end("hello " + req.method);
				return;
			}
			if (req.url === "/echo") {
				const chunks = [];
				req.on("data", (c) => chunks.push(c));
				req.on("end", () => {
					const body = Buffer.concat(chunks);
					res.writeHead(200, { "Content-Type": req.headers["content-type"] || "application/octet-stream" });
					res.end(body);
				});
				return;
			}
			if (req.url === "/chunked") {
				res.writeHead(200);
				res.write("one,");
				setTimeout(() => { res.write("two,"); res.end("three"); }, 10);
				return;
			}
			res.statusCode = 404;
			res.end("not found");
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	base := "http://127.0.0.1:" + port

	resp, err := http.Get(base + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello GET" {
		t.Fatalf("GET /hello = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom = %q", got)
	}

	// Request body round trip (Japanese text exercises the byte path).
	resp2, err := http.Post(base+"/echo", "text/plain", strings.NewReader("エコー test"))
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != "エコー test" {
		t.Errorf("POST /echo = %q", body2)
	}

	// Streaming writes across a timer.
	resp3, err := http.Get(base + "/chunked")
	if err != nil {
		t.Fatal(err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if string(body3) != "one,two,three" {
		t.Errorf("GET /chunked = %q", body3)
	}

	resp4, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != 404 {
		t.Errorf("GET /nope = %d", resp4.StatusCode)
	}
}

func TestHTTPServerHandlerThrowIs500(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, _ := startServer(t, js, rt, `
		const http = require("http");
		const server = http.createServer((req, res) => { throw new Error("handler boom"); });
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestHTTPListenPermission(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Listen: func(network, addr string) bool { return false },
	})
	runScript(t, rt, `
		const http = require("http");
		globalThis.r = {};
		const server = http.createServer(() => {});
		server.on("error", (e) => { r.code = e.code; });
		server.listen(0);
	`)
	if got := evalStr(t, js, `r.code`); got != "EACCES" {
		t.Errorf("listen error code = %q, want EACCES", got)
	}
	_ = fmt.Sprint()
}

// The handler responds WITHOUT reading the request body (early reject). The
// server must not hang, must not race the body pump, and must return the
// intended status — the pump is joined before ServeHTTP returns.
func TestHTTPServerEarlyResponseUnconsumedBody(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		const server = http.createServer((req, res) => {
			// Do NOT read req; respond immediately.
			res.statusCode = 413;
			res.end("too large");
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	big := strings.NewReader(strings.Repeat("x", 1<<20)) // 1 MiB, unconsumed
	resp, err := http.Post("http://127.0.0.1:"+port+"/upload", "application/octet-stream", big)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 413 || string(body) != "too large" {
		t.Fatalf("early response = %d %q, want 413 \"too large\"", resp.StatusCode, body)
	}
}

// A keep-alive http.Agent must reuse the TCP connection across sequential
// requests: the server sees the SAME client remote port for both. A no-keepalive
// agent (agent:false) opens a fresh connection each time (different port).
func TestHTTPAgentKeepAliveReusesSocket(t *testing.T) {
	var (
		mu    sync.Mutex
		ports []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ports = append(ports, r.RemoteAddr)
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	// Two sequential requests through one keepAlive agent. The second is issued
	// from the first's 'end', so the connection is back in the pool and reused.
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = { bodies: [] };
		const http = require("http");
		const agent = new http.Agent({ keepAlive: true });
		function once(cb) {
			const req = http.request(BASE, { agent }, (res) => {
				let b = "";
				res.on("data", (c) => { b += c; });
				res.on("end", () => { r.bodies.push(b); cb(); });
			});
			req.end();
		}
		once(() => { once(() => {}); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ports...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2: %v", len(got), got)
	}
	if got[0] != got[1] {
		t.Errorf("keep-alive agent did not reuse the socket: remote ports %q vs %q", got[0], got[1])
	}
	if body := evalStr(t, js, `r.bodies.join(",")`); body != "ok,ok" {
		t.Errorf("response bodies = %q, want ok,ok", body)
	}
}

// With agent:false (no pooling) each request opens a fresh connection, so the
// server sees two different client remote ports.
func TestHTTPAgentFalseOpensFreshSockets(t *testing.T) {
	var (
		mu    sync.Mutex
		ports []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ports = append(ports, r.RemoteAddr)
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = { done: 0 };
		const http = require("http");
		function once(cb) {
			const req = http.request(BASE, { agent: false }, (res) => {
				res.on("data", () => {});
				res.on("end", () => { r.done++; cb(); });
			});
			req.end();
		}
		once(() => { once(() => {}); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ports...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(got))
	}
	if got[0] == got[1] {
		t.Errorf("agent:false reused a socket (%q); each request should open a fresh connection", got[0])
	}
}

// The default globalAgent is keep-alive (Node v19+): sequential requests with no
// explicit agent reuse the connection.
func TestHTTPGlobalAgentIsKeepAlive(t *testing.T) {
	js, _ := newRuntime(t, spidermonkey.Config{})
	if got := evalStr(t, js, `require("http").globalAgent.keepAlive ? "on" : "off"`); got != "on" {
		t.Errorf("http.globalAgent.keepAlive = %q, want on", got)
	}

	var (
		mu    sync.Mutex
		ports []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ports = append(ports, r.RemoteAddr)
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js2, rt2 := newRuntime(t, spidermonkey.Config{})
	js2.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt2.RunScript(context.Background(), `
		globalThis.r = { done: 0 };
		const http = require("http");
		function once(cb) {
			const req = http.request(BASE, (res) => {
				res.on("data", () => {});
				res.on("end", () => { r.done++; cb(); });
			});
			req.end();
		}
		once(() => { once(() => {}); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), ports...)
	mu.Unlock()
	if len(got) == 2 && got[0] != got[1] {
		t.Errorf("globalAgent did not reuse the socket: %q vs %q", got[0], got[1])
	}
}

// http.ClientRequest#abort() emits 'abort' exactly once, however many times it
// is called, and sets the deprecated `aborted` flag.
//
// It used to only destroy the request, so nothing that waited on 'abort' ever
// continued — and because such code typically closes its server from that
// handler, the server stayed open and the event loop never went idle. That one
// missing event is why ~195 http tests in Node's suite hang here.
func TestClientRequestAbortEmitsAbortOnce(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:      fstest.MapFS{},
		Listen:  func(network, addr string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	r, err := rt.RunScript(ctx, `
		const http = require("http");
		globalThis.log = [];
		const server = http.createServer((req, res) => res.end());
		server.listen(0, function () {
			const req = http.request({ port: this.address().port }, () => log.push("response"));
			req.on("abort", () => {
				log.push("abort:" + req.aborted);
				server.close(() => log.push("closed"));
			});
			req.end();
			req.abort();
			req.abort(); // a second abort must NOT produce a second event
		});
	`)
	if err != nil {
		t.Fatalf("the run did not finish (%v) — 'abort' never fired and the server stayed open", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the loop only ended at the deadline", elapsed)
	}
	v, err := js.Eval(context.Background(), `globalThis.log.join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Value.String(); got != "abort:true,closed" {
		t.Errorf("events = %q, want \"abort:true,closed\"", got)
	}
}

// The http client must run off the loop goroutine: a slow response must not
// freeze timers or other work. A ~150ms response races a 10ms timer; the timer
// has to fire first, which is only possible if the request does not block the
// loop.
func TestHTTPClientDoesNotBlockLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("slow-body"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		const http = require("http");
		http.get(BASE, (res) => {
			let body = "";
			res.on("data", (c) => { body += c; });
			res.on("end", () => { __order.push("response:" + body); });
		});
		setTimeout(() => { __order.push("timer"); }, 10);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "timer,response:slow-body" {
		t.Fatalf("order = %q, want timer,response:slow-body (client blocked the loop)", got)
	}
}

// When the guest calls req.write(chunk) before req.end(), the request body must
// stream to the server chunked: the server sees the headers and the first chunk
// promptly, without waiting for req.end() (which happens ~300ms later), and the
// request arrives with Transfer-Encoding: chunked.
func TestHTTPClientStreamingRequestBody(t *testing.T) {
	var (
		mu             sync.Mutex
		firstChunkAt   time.Time
		endAt          time.Time
		transferEnc    []string
		fullBody       string
		handlerEntered = make(chan struct{})
		firstSeen      = make(chan struct{})
	)
	start := time.Now()
	var firstOnce, handlerOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerOnce.Do(func() { close(handlerEntered) })
		mu.Lock()
		transferEnc = r.TransferEncoding
		mu.Unlock()
		buf := make([]byte, 5)
		n, _ := io.ReadFull(r.Body, buf)
		mu.Lock()
		firstChunkAt = time.Now()
		got := string(buf[:n])
		mu.Unlock()
		firstOnce.Do(func() { close(firstSeen) })
		rest, _ := io.ReadAll(r.Body)
		mu.Lock()
		endAt = time.Now()
		fullBody = got + string(rest)
		mu.Unlock()
		w.Write([]byte("done"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = {};
		const http = require("http");
		const req = http.request(BASE, { method: "POST" }, (res) => {
			let body = "";
			res.on("data", (c) => { body += c; });
			res.on("end", () => { r.body = body; });
		});
		req.write("first");
		setTimeout(() => { req.end("SECOND"); }, 300);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	mu.Lock()
	fc, ea := firstChunkAt, endAt
	te := append([]string(nil), transferEnc...)
	fb := fullBody
	mu.Unlock()

	// The server must have received the first chunk well before req.end() fired.
	if fc.IsZero() || ea.IsZero() {
		t.Fatalf("server did not complete the request (firstChunk=%v end=%v)", fc, ea)
	}
	if gap := ea.Sub(fc); gap < 150*time.Millisecond {
		t.Errorf("first chunk arrived only %v before end; expected streaming (gap >= 150ms). firstChunk@%v end@%v",
			gap, fc.Sub(start), ea.Sub(start))
	}
	if len(te) != 1 || te[0] != "chunked" {
		t.Errorf("request Transfer-Encoding = %v, want [chunked]", te)
	}
	if fb != "firstSECOND" {
		t.Errorf("server assembled body = %q, want firstSECOND", fb)
	}
	if got := evalStr(t, js, `r.body ?? ""`); got != "done" {
		t.Errorf("guest response body = %q, want done", got)
	}
}

// The buffered fast path (the whole body handed to req.end(body) in one shot)
// must still send a Content-Length request, not chunked.
func TestHTTPClientBufferedBodyUsesContentLength(t *testing.T) {
	var (
		mu          sync.Mutex
		transferEnc []string
		contentLen  int64
		body        string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		transferEnc = r.TransferEncoding
		contentLen = r.ContentLength
		body = string(b)
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = {};
		const http = require("http");
		const req = http.request(BASE, { method: "POST" }, (res) => {
			let body = "";
			res.on("data", (c) => { body += c; });
			res.on("end", () => { r.body = body; });
		});
		req.end("hello-buffered");
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	mu.Lock()
	te := append([]string(nil), transferEnc...)
	cl, b := contentLen, body
	mu.Unlock()

	if len(te) != 0 {
		t.Errorf("buffered request Transfer-Encoding = %v, want none (Content-Length path)", te)
	}
	if cl != int64(len("hello-buffered")) {
		t.Errorf("Content-Length = %d, want %d", cl, len("hello-buffered"))
	}
	if b != "hello-buffered" {
		t.Errorf("server body = %q", b)
	}
	if got := evalStr(t, js, `r.body ?? ""`); got != "ok" {
		t.Errorf("guest response body = %q, want ok", got)
	}
}

// Multiple streaming writes are all delivered in order and the response streams
// back normally.
func TestHTTPClientStreamingMultipleChunks(t *testing.T) {
	var (
		mu   sync.Mutex
		body string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.Write([]byte("received"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = {};
		const http = require("http");
		const req = http.request(BASE, { method: "PUT" }, (res) => {
			let body = "";
			res.on("data", (c) => { body += c; });
			res.on("end", () => { r.body = body; });
		});
		req.write("a");
		req.write("b");
		req.write("c");
		req.end("d");
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	mu.Lock()
	b := body
	mu.Unlock()
	if b != "abcd" {
		t.Errorf("server assembled streamed body = %q, want abcd", b)
	}
	if got := evalStr(t, js, `r.body ?? ""`); got != "received" {
		t.Errorf("guest response body = %q, want received", got)
	}
}

// TestHTTPRequestPathOverride verifies http.request(url, { path }) uses the
// overridden path, not the URL's original path.
func TestHTTPRequestPathOverride(t *testing.T) {
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		const http = require("http");
		const req = http.request(BASE + "/original", { path: "/override" }, (res) => { res.resume?.(); });
		req.on("error", () => {});
		req.end();
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	select {
	case p := <-gotPath:
		if p != "/override" {
			t.Errorf("server saw path %q, want /override", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never received the request")
	}
}

// TestHTTPClientSetCookieArray verifies a client response with a single
// Set-Cookie exposes it as a one-element array (Node contract).
func TestHTTPClientSetCookieArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sid=1; Path=/")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		const http = require("http");
		globalThis.r = {};
		http.get(BASE + "/", (res) => {
			const sc = res.headers["set-cookie"];
			r.isArray = Array.isArray(sc);
			r.len = Array.isArray(sc) ? sc.length : -1;
			res.resume();
		}).on("error", (e) => { r.err = String(e); });
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	if got := evalStr(t, js, "String(r.isArray)"); got != "true" {
		t.Errorf("single set-cookie not an array (isArray=%q)", got)
	}
	if got := evalStr(t, js, "String(r.len)"); got != "1" {
		t.Errorf("set-cookie array length = %q, want 1", got)
	}
}

// TestHTTPClientStreamsResponseOnHeaders verifies http.get delivers the
// 'response' event on headers and streams the body incrementally, rather than
// buffering the whole body before emitting 'response'. The server sends one
// chunk, waits, then a second chunk: streaming yields two separate 'data'
// events with 'response' before them; buffering would deliver one combined
// chunk with 'response' only after the whole body arrived.
func TestHTTPClientStreamsResponseOnHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.WriteHeader(200)
		w.Write([]byte("AAAA"))
		if f != nil {
			f.Flush()
		}
		time.Sleep(120 * time.Millisecond)
		w.Write([]byte("BBBB"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		globalThis.__order = [];
		const http = require("http");
		http.get(BASE, (res) => {
			__order.push("response:" + res.statusCode);
			res.on("data", (c) => __order.push("d:" + c));
			res.on("end", () => __order.push("end"));
		});
	`)
	// Two separate data events with response before them. Buffering would instead
	// yield "response:200,d:AAAABBBB,end" (one combined chunk).
	if got := evalStr(t, js, `__order.join(",")`); got != "response:200,d:AAAA,d:BBBB,end" {
		t.Errorf("order = %q, want response:200,d:AAAA,d:BBBB,end (client body not streamed)", got)
	}
}

// TestHTTPClientDestroyStopsIdlePump verifies res.destroy() on an idle streaming
// response (an SSE-style stream parked with no bytes pending) interrupts the body
// pump via request-context cancellation, so the loop reaches idle instead of
// hanging forever with a leaked goroutine/AddPending.
func TestHTTPClientDestroyStopsIdlePump(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.WriteHeader(200)
		w.Write([]byte("hi"))
		if f != nil {
			f.Flush()
		}
		<-release // hold the stream open (idle) until the test tears down
	}))
	defer srv.Close()
	defer close(release)

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	script := `
		globalThis.__done = false;
		const http = require("http");
		http.get(BASE, (res) => {
			res.on("data", () => {});
			setTimeout(() => { res.destroy(); __done = true; }, 50);
		});
	`
	done := make(chan error, 1)
	go func() { _, e := rt.RunScript(context.Background(), script); done <- e }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("RunScript: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("RunScript hung — res.destroy() did not stop the idle client body pump")
	}
	if got := evalStr(t, js, `String(__done)`); got != "true" {
		t.Errorf("__done = %q, want true", got)
	}
}

// TestHTTPClientBadMethodEmitsError verifies http.request with an invalid method
// emits 'error' (not hangs) — the op's NewRequest failure must reach the guest
// via onError, since the JS shim ignores the op's return value.
func TestHTTPClientBadMethodEmitsError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	script := `
		globalThis.r = {};
		const http = require("http");
		const req = http.request({ host: "127.0.0.1", port: 1, method: "BAD METHOD" }, () => { r.got = "response"; });
		req.on("error", (e) => { r.error = e.code || e.message || "error"; });
		req.end();
	`
	done := make(chan error, 1)
	go func() { _, e := rt.RunScript(context.Background(), script); done <- e }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("RunScript: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunScript hung — invalid method did not emit 'error' (client request never settled)")
	}
	if got := evalStr(t, js, `String(r.error ?? "")`); got == "" {
		t.Errorf("no 'error' emitted for invalid method (got r.got=%q)", evalStr(t, js, `String(r.got ?? "")`))
	}
}

// TestHTTPClientDestroyReleasesLoop verifies req.destroy() cancels the round-trip
// and releases the host body pump, so an aborted/unconsumed multi-chunk response
// doesn't park the pump and wedge the event loop forever.
func TestHTTPClientDestroyReleasesLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write(bytes.Repeat([]byte("x"), 256*1024)) // >32KiB so the pump needs multiple reads
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	script := `
		globalThis.__done = false;
		const http = require("http");
		const req = http.get(BASE, (res) => { req.destroy(); __done = true; });
	`
	done := make(chan error, 1)
	go func() { _, e := rt.RunScript(context.Background(), script); done <- e }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("RunScript: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("RunScript hung — req.destroy() did not release the client body pump")
	}
	if got := evalStr(t, js, `String(__done)`); got != "true" {
		t.Errorf("__done = %q, want true", got)
	}
}

// TestHTTPClientNoAutoRedirect verifies the node:http client hands the 3xx to the
// caller instead of transparently following it (Node semantics).
func TestHTTPClientNoAutoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/dest", http.StatusFound)
			return
		}
		w.Write([]byte("final"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		globalThis.r = {};
		const http = require("http");
		http.get(BASE + "/redir", (res) => { r.status = res.statusCode; r.loc = res.headers.location; });
	`)
	if got := evalStr(t, js, `String(r.status)`); got != "302" {
		t.Errorf("status = %q, want 302 (client should not auto-follow)", got)
	}
	if got := evalStr(t, js, `String(r.loc ?? "")`); !strings.HasSuffix(got, "/dest") {
		t.Errorf("location = %q, want .../dest", got)
	}
}

// TestHTTPClientArrayHeaderKeepsAll verifies an array-valued request header
// (multiple cookies) is sent AND doesn't cause every other header to be dropped.
func TestHTTPClientArrayHeaderKeepsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Echo-Cookies", strings.Join(r.Header["Cookie"], "|"))
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		globalThis.r = {};
		const http = require("http");
		const req = http.request(BASE, { headers: { Authorization: "Bearer x", Cookie: ["a=1", "b=2"] } }, (res) => {
			r.auth = res.headers["x-echo-auth"];
			r.cookies = res.headers["x-echo-cookies"];
		});
		req.end();
	`)
	if got := evalStr(t, js, `String(r.auth ?? "")`); got != "Bearer x" {
		t.Errorf("Authorization = %q, want 'Bearer x' (array header dropped all headers)", got)
	}
	if got := evalStr(t, js, `String(r.cookies ?? "")`); !strings.Contains(got, "a=1") || !strings.Contains(got, "b=2") {
		t.Errorf("Cookies = %q, want both a=1 and b=2", got)
	}
}

// TestHTTPClientNoGzipDecompress verifies the client does NOT auto-decompress a
// gzip response (Node hands the raw body + Content-Encoding to the caller).
func TestHTTPClientNoGzipDecompress(t *testing.T) {
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	gz.Write([]byte("hello-gzip-payload"))
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(body.Bytes())
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		globalThis.r = {};
		const http = require("http");
		http.get(BASE, (res) => {
			r.enc = res.headers["content-encoding"];
			const chunks = [];
			res.on("data", (c) => chunks.push(c));
			res.on("end", () => { r.firstByte = Buffer.concat(chunks)[0]; });
		});
	`)
	if got := evalStr(t, js, `String(r.enc ?? "")`); got != "gzip" {
		t.Errorf("content-encoding = %q, want gzip (client decompressed and stripped it)", got)
	}
	// gzip magic byte 0x1f = 31 — proves the body is raw gzip, not decompressed text.
	if got := evalStr(t, js, `String(r.firstByte)`); got != "31" {
		t.Errorf("first body byte = %q, want 31 (gzip magic; body was decompressed)", got)
	}
}

// Closing the Runtime while a request is still in flight must release the
// request's host goroutine — including when the guest has already called
// server.close().
//
// That combination is the leak: server.close() is a GRACEFUL shutdown, so it
// removes the server from the runtime's table and waits (on an unbounded
// context) for every in-flight ServeHTTP to finish. Runtime.Close can no longer
// find that server to hard-close it, and the only signal it does send — closing
// the request's done channel — was not something the serving goroutine watched.
// This asserts the liveness property — Close returns and the client is
// released — rather than the leak itself: the leak was observed as parked
// goroutines in a long external-suite run, and this fixture does not reproduce
// that exact interleaving.
func TestCloseReleasesUnfinishedResponse(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:      fstest.MapFS{},
		Listen:  func(network, addr string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A server whose handler never answers: the request reaches the guest and
	// stays there.
	// The listening server keeps the loop alive, so this run ends at its
	// deadline by design; keep it short.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const http = require("http");
		globalThis.__reached = 0;
		const server = http.createServer((req, res) => {
			globalThis.__reached++;
			// Never responds — and asks for a graceful close while this request is
			// still open, which is the path that used to leak.
			server.close();
		});
		server.listen(0, "127.0.0.1", () => { globalThis.__port = server.address().port; });
	`); err != nil {
		// The loop stays alive for the listening server; a deadline here is the
		// expected way out, not a failure.
		t.Logf("RunScript returned: %v", err)
	}
	pv, err := js.Eval(context.Background(), "globalThis.__port || 0")
	if err != nil {
		t.Fatal(err)
	}
	port := pv.Value.Int()
	if port == 0 {
		t.Skip("server did not report a port; nothing to measure")
	}

	// Fire a request that will never be answered, from the HOST side so the
	// connection is real, and give it time to reach the guest handler.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		c := &http.Client{Timeout: 30 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + itoa(port) + "/")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(300 * time.Millisecond)

	// Closing must return promptly AND let the in-flight request go: the client
	// gets an answer (an aborted connection or an error), rather than hanging
	// until its own timeout.
	closed := make(chan error, 1)
	go func() { closed <- rt.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Logf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime.Close did not return within 10s")
	}
	select {
	case <-reqDone:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request still blocked 10s after the Runtime was closed: " +
			"its serving goroutine leaked")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestHTTPSServerIdNoCollision verifies a guest cannot force an https server id
// to collide with a live http server (which would evict+leak it and hang).
func TestHTTPSServerIdNoCollision(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	_ = js
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const http = require("http");
			const https = require("https");
			globalThis.r = {};
			const httpSrv = http.createServer((req, res) => res.end("http"));
			httpSrv.listen(0, "127.0.0.1", () => {
				// Force the next https id to collide with the http server's id.
				globalThis.__node_set_next_https(httpSrv._id - 1);
				const { cert, key } = require("tls").generateSelfSigned("localhost");
				const httpsSrv = https.createServer({ cert, key }, (req, res) => res.end("https"));
				httpsSrv.listen(0, "127.0.0.1", () => {
					r.bothLive = httpSrv._id !== httpsSrv._id;
					httpsSrv.close();
					httpSrv.close();
				});
				httpsSrv.on("error", (e) => { r.err = String(e); httpSrv.close(); });
			});
		`)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("https/http id collision hung the loop")
	}
	if got := evalStr(t, js, "String(r.bothLive ?? false)"); got != "true" {
		t.Errorf("http and https share an id (collision): bothLive=%q", got)
	}
}

// TestHeadersSentAfterWriteHead verifies res.headersSent is true right after
// writeHead (framework error-middleware branches on it).
func TestHeadersSentAfterWriteHead(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.r = {};
		const server = http.createServer((req, res) => {
			res.writeHead(200, { "content-type": "text/plain" });
			r.sent = res.headersSent;
			res.end("ok");
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := evalStr(t, js, "String(r.sent ?? false)"); got != "true" {
		t.Errorf("headersSent after writeHead = %q, want true", got)
	}
}

// TestHTTPServerClientDisconnectClosesResponse verifies a client disconnect
// mid-response fires res.on('close') on the guest handler — the canonical SSE /
// long-poll cancellation hook that a bodyless GET would otherwise never see.
func TestHTTPServerClientDisconnectClosesResponse(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.__closed = false;
		globalThis.__server = http.createServer((req, res) => {
			res.writeHead(200, { "Content-Type": "text/event-stream" });
			res.write("data: hi\n\n");
			const iv = setInterval(() => { try { res.write("data: tick\n\n"); } catch {} }, 20);
			res.on("close", () => { __closed = true; clearInterval(iv); });
		});
		__server.listen(0);
		globalThis.PORT = __server.address().port;
	`)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	fmt.Fprintf(conn, "GET /sse HTTP/1.1\r\nHost: x\r\n\r\n")
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf) // read some of the response, then drop the connection
	conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if evalStr(t, js, `String(__closed)`) == "true" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := evalStr(t, js, `String(__closed)`); got != "true" {
		t.Errorf("res.on('close') did not fire on client disconnect (got %q)", got)
	}
}

// TestHTTPServerCloseGraceful verifies server.close() lets an in-flight request
// finish (full body, not truncated) and fires its callback only afterwards.
func TestHTTPServerCloseGraceful(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { order: [] };
		const http = require("http");
		const server = http.createServer((req, res) => {
			setTimeout(() => res.end("done"), 60);
		});
		server.listen(0, () => {
			const port = server.address().port;
			http.get({ host: "127.0.0.1", port, path: "/" }, (res) => {
				let b = "";
				res.on("data", (c) => { b += c; });
				res.on("end", () => r.order.push("response:" + b));
			});
			// Close while the request is still in flight.
			setTimeout(() => server.close(() => r.order.push("closed")), 20);
		});
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "response:done,closed" {
		t.Errorf("order = %q, want response:done,closed (close was not graceful)", got)
	}
}

// TestHTTPResponseContentLength verifies res.end(body) sets Content-Length (not
// Transfer-Encoding: chunked), matching Node.
func TestHTTPResponseContentLength(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.__server = http.createServer((req, res) => { res.end("hi"); });
		__server.listen(0);
		globalThis.PORT = __server.address().port;
	`)
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Length"); got != "2" {
		t.Errorf("Content-Length = %q, want 2", got)
	}
	if got := resp.TransferEncoding; len(got) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none (should not be chunked)", got)
	}
}

// streamingReader emits N chunks with a small delay, so the server sees them
// incrementally rather than all at once.
type streamingReader struct {
	chunks [][]byte
	i      int
}

func (s *streamingReader) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	time.Sleep(15 * time.Millisecond)
	n := copy(p, s.chunks[s.i])
	s.i++
	return n, nil
}

func TestHTTPServerStreamsRequestBody(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.r = { chunkSizes: [] };
		const server = http.createServer((req, res) => {
			req.on("data", (c) => { r.chunkSizes.push(c.length); });
			req.on("end", () => { res.end("received " + r.chunkSizes.length + " chunks"); });
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	body := &streamingReader{chunks: [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cccc")}}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/", body)
	req.ContentLength = 12
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(out) != "received 3 chunks" && string(out) != "received 2 chunks" && string(out) != "received 1 chunks" {
		t.Fatalf("body = %q", out)
	}
	// The server must have observed MORE THAN ONE chunk (proving streaming);
	// a buffered impl would deliver exactly 1.
	if got := evalStr(t, js, "String(r.chunkSizes.length)"); got == "1" {
		t.Errorf("request body was buffered, not streamed (1 chunk)")
	}
}

// TestHTTPServerAbortedRequestBody verifies a client that declares a body length
// but disconnects mid-body is surfaced to the handler as an abort — NOT as a
// clean 'end' — so a handler can't persist a truncated request as if it were
// whole.
func TestHTTPServerAbortedRequestBody(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.r = {};
		const server = http.createServer((req, res) => {
			let got = 0;
			req.on("data", (c) => { got += c.length; });
			req.on("error", () => {});          // swallow ECONNRESET
			req.on("aborted", () => { r.aborted = true; r.bytes = got; });
			req.on("end", () => { r.ended = true; });
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)

	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	// Declare 1000 bytes, send 10, then abruptly close the socket.
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n")
	conn.Write([]byte("0123456789"))
	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evalStr(t, js, `String(r.aborted ?? false)`) == "true" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if evalStr(t, js, `String(r.aborted ?? false)`) != "true" {
		t.Fatal("handler did not observe the request abort")
	}
	if evalStr(t, js, `String(r.ended ?? false)`) == "true" {
		t.Error("handler saw a clean 'end' for a truncated request body")
	}
}

// An https request carries its own TLS settings — rejectUnauthorized, ca,
// servername — and they never reached the host transport, so every request to
// a self-signed server failed the handshake against the system roots. That is
// what `rejectUnauthorized: false` exists for, and it is how nearly every
// https test (and every local dev server) is set up.
func TestHTTPSClientRequestTLSOptions(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Resolve: func(host string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Listen:  func(network, addr string) bool { return true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const https = require("https");
		const { cert, key } = require("tls").generateSelfSigned("localhost");
		globalThis.r = {};
		const server = https.createServer({ cert, key }, (req, res) => res.end("hello"));
		server.listen(0, "127.0.0.1", () => {
			const port = server.address().port;
			const get = (opts, tag, then) => {
				const req = https.request({ host: "127.0.0.1", port, path: "/", ...opts }, (res) => {
					let body = "";
					res.setEncoding("utf8");
					res.on("data", (c) => { body += c; });
					res.on("end", () => { r[tag] = res.statusCode + ":" + body; then(); });
				});
				req.on("error", (e) => { r[tag] = "ERR " + e.message; then(); });
				req.end();
			};
			// Verification off: the self-signed certificate is accepted.
			get({ rejectUnauthorized: false }, "insecure", () => {
				// The same server, trusted explicitly through the ca option.
				get({ ca: cert, servername: "localhost" }, "withCA", () => {
					// And with neither, the handshake must still be REJECTED —
					// the fix must not have made https unconditionally trusting.
					get({}, "verified", () => server.close());
				});
			});
		});
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	if got := evalStr(t, js, `r.insecure`); got != "200:hello" {
		t.Errorf("rejectUnauthorized:false = %q, want 200:hello", got)
	}
	if got := evalStr(t, js, `r.withCA`); got != "200:hello" {
		t.Errorf("explicit ca = %q, want 200:hello", got)
	}
	if got := evalStr(t, js, `r.verified`); got == "200:hello" {
		t.Errorf("default verification accepted a self-signed certificate (%q)", got)
	}
}
