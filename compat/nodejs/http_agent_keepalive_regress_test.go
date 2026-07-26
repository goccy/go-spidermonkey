package nodejs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
