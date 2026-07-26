package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
