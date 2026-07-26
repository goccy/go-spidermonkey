package nodejs_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// https.request with an OPTIONS OBJECT (no protocol) must speak TLS, not
// plaintext. Regression: it aliased http's ClientRequest, which defaults the
// scheme to "http", so intended-HTTPS requests went out unencrypted. Target
// here is a PLAINTEXT server: with the fix, https attempts a TLS handshake and
// errors instead of returning the plaintext 200.
func TestHTTPSRequestOptionsUsesTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "PLAINTEXT-OK")
	}))
	defer srv.Close()
	h, p, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("H", spidermonkey.ValueOf(h))
	js.Global().Set("P", spidermonkey.ValueOf(p))
	runScript(t, rt, `
		const https = require("https");
		globalThis.r = {};
		const req = https.request({ host: H, port: Number(P), path: "/", method: "GET" }, (res) => {
			r.status = res.statusCode;
			let body = "";
			res.on("data", (d) => body += d);
			res.on("end", () => { r.body = body; });
		});
		req.on("error", (e) => { r.err = String(e.code || e.message); });
		req.end();
	`)
	if got := evalStr(t, js, `String(r.status ?? '')`); got == "200" {
		t.Errorf("https.request(options) reached a plaintext server with 200 (%q) — TLS downgrade", evalStr(t, js, `String(r.body ?? '')`))
	}
	if got := evalStr(t, js, `String(r.err ?? '')`); got == "" {
		t.Error("https.request(options) to a plaintext port did not error — it did not attempt TLS")
	}
}

// tls.createServer().listen({port,host}, cb) must bind the REQUESTED port, not
// silently fall back to an ephemeral one (the object-options form was ignored).
func TestTLSServerListenObjectPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("WANT", spidermonkey.ValueOf(want))
	js.Global().Set("CERT", spidermonkey.ValueOf(string(testCertPEM)))
	js.Global().Set("KEY", spidermonkey.ValueOf(string(testKeyPEM)))
	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const server = tls.createServer({ cert: CERT, key: KEY });
		server.on("error", (e) => { r.err = String(e.code || e.message); });
		server.listen({ port: WANT, host: "127.0.0.1" });
		// listen() sets the bound port synchronously (address() is valid before
		// the 'listening' tick); capture it, then close so the loop drains.
		const a = server.address();
		r.bound = a ? a.port : null;
		server.close();
	`)
	if got := evalStr(t, js, `String(r.bound ?? '')`); got != strconv.Itoa(want) {
		t.Errorf("tls listen({port:%d}) bound %q, want %d (object port dropped)", want, got, want)
	}
}

// watchFile() then unwatchFile() must let the event loop reach idle — the
// watcher's host loop-pending has to be released by unwatchFile. Regression:
// unwatchFile was a no-op, so the loop hung forever.
func TestWatchFileUnwatchReleasesLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cfg.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: os.DirFS(dir)})
	r, err := js.Eval(context.Background(), `
		const fs = require("fs");
		fs.watchFile("cfg.json", () => {});
		fs.unwatchFile("cfg.json");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("loop did not reach idle after unwatchFile (hang): %v", err)
	}
}
