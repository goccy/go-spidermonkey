package nodejs_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
