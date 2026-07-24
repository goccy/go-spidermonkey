package nodejs_test

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// The network/listen hooks are fail-closed: an embedder that installs the
// node runtime but leaves Dial/Resolve/Listen nil must NOT get an open network.
// These tests build the runtime directly (bypassing newRuntime's allow-all
// test defaults) with a bare Config to assert the deny-by-default property.
func newBareRuntime(t *testing.T) (*spidermonkey.JS, *nodejs.Runtime) {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { rt.Close() })
	return js, rt
}

func TestNetConnectDeniedByDefault(t *testing.T) {
	js, rt := newBareRuntime(t)
	// net.connect is async now; the denial surfaces as an 'error' event once the
	// loop processes the (denied) dial. Run the loop to completion, then assert.
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		const net = require("net");
		const s = net.connect(9, "127.0.0.1");
		s.on("error", (e) => { __r.err = String(e && e.message || e); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Fatalf("nil Dial should deny net.connect; got err=%q", got)
	}
}

func TestListenDeniedByDefault(t *testing.T) {
	js, _ := newBareRuntime(t)
	r, err := js.Eval(context.Background(), `
		globalThis.__r = {};
		const net = require("net");
		try {
			const srv = net.createServer();
			srv.on("error", (e) => { __r.err = String(e && e.message || e); });
			srv.listen(0, () => { __r.listening = true; });
		} catch (e) { __r.err = String(e && e.message || e); }
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	if evalStr(t, js, `String(__r.listening ?? false)`) == "true" {
		t.Fatalf("nil Listen should deny net.createServer().listen()")
	}
	if got := evalStr(t, js, `__r.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Fatalf("nil Listen should deny listen; got err=%q", got)
	}
}

func TestHTTPClientDeniedByDefault(t *testing.T) {
	js, rt := newBareRuntime(t)
	// http.get is async; the denial surfaces as an 'error' event once the loop
	// processes the (denied) dial. Run the loop to completion, then assert.
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		const http = require("http");
		const req = http.get("http://127.0.0.1:9/", () => { __r.responded = true; });
		req.on("error", (e) => { __r.err = String(e && e.message || e); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Fatalf("nil Dial should deny http.get; got err=%q", got)
	}
}
