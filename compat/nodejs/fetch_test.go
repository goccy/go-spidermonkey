package nodejs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestFetchDoesNotDeadlockSelfServe verifies an http handler that awaits a
// fetch() to a server on the SAME instance completes (fetch is async, so it no
// longer wedges the single loop goroutine and deadlocks the inner request).
func TestFetchDoesNotDeadlockSelfServe(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	_ = js
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const http = require("http");
			globalThis.r = {};
			const server = http.createServer(async (req, res) => {
				if (req.url === "/inner") { res.end("pong"); return; }
				const resp = await fetch("http://127.0.0.1:" + server.address().port + "/inner");
				const body = await resp.text();
				r.proxied = body;
				res.end(body);
			});
			server.listen(0, "127.0.0.1", () => {
				fetch("http://127.0.0.1:" + server.address().port + "/")
					.then((rr) => rr.text())
					.then((t) => { r.outer = t; server.close(); })
					.catch((e) => { r.err = String(e); server.close(); });
			});
		`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("self-serve fetch deadlocked the loop")
	}
	if got := evalStr(t, js, "String(r.outer ?? '')"); got != "pong" {
		t.Errorf("outer fetch got %q, want pong", got)
	}
	if got := evalStr(t, js, "String(r.proxied ?? '')"); got != "pong" {
		t.Errorf("handler proxied %q, want pong", got)
	}
}

// TestFetchStreamBodyAsync verifies a fetch response body read via getReader()
// works and doesn't block the loop (the pull now reads off-loop).
func TestFetchStreamBodyAsync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("chunk-one-two-three"))
	}))
	defer srv.Close()
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("URL", spidermonkey.ValueOf(srv.URL))
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			globalThis.r = {};
			fetch(URL).then(async (resp) => {
				const reader = resp.body.getReader();
				let out = "";
				for (;;) {
					const { value, done } = await reader.read();
					if (done) break;
					out += new TextDecoder().decode(value);
				}
				r.streamed = out;
			}).catch(e => { r.err = String(e); });
		`)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("stream read deadlocked the loop")
	}
	if got := evalStr(t, js, "String(r.streamed ?? '')"); got != "chunk-one-two-three" {
		t.Errorf("streamed body = %q, want chunk-one-two-three", got)
	}
}

// TestFetchConcurrentReadsNoRace verifies two concurrent reader.read() calls on
// a fetch body don't run overlapping off-loop reads (the pull is serialized).
func TestFetchConcurrentReadsNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abcdefghij"))
	}))
	defer srv.Close()
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("URL", spidermonkey.ValueOf(srv.URL))
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			globalThis.r = {};
			fetch(URL).then(async (resp) => {
				const reader = resp.body.getReader();
				// Two concurrent reads (legal per the Streams spec).
				const [a, b] = await Promise.all([reader.read(), reader.read()]);
				let out = "";
				for (const x of [a, b]) if (!x.done && x.value) out += new TextDecoder().decode(x.value);
				// Drain the rest.
				for (;;) { const c = await reader.read(); if (c.done) break; out += new TextDecoder().decode(c.value); }
				r.out = out;
			}).catch(e => { r.err = String(e); });
		`)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("concurrent reads deadlocked")
	}
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("error: %s", got)
	}
	if got := evalStr(t, js, "String(r.out ?? '')"); got != "abcdefghij" {
		t.Errorf("concurrent-read body = %q, want abcdefghij", got)
	}
}
