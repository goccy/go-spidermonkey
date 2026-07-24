package nodejs_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
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
