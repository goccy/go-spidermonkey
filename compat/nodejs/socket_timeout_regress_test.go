package nodejs_test

// Regression coverage for socket idle timeouts: net.Socket#setTimeout (and the
// http shims that delegate to it) used to be silent no-ops. Node semantics:
// setTimeout(ms[, cb]) arms an idle timer reset by any activity (data, write,
// connect); after ms of inactivity the socket emits 'timeout' (and fires the
// one-shot cb) WITHOUT destroying the socket; setTimeout(0) disables; the
// timer never keeps the event loop alive on its own.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// An idle connection must emit 'timeout' (and invoke the setTimeout callback)
// after the configured inactivity window, and must NOT be destroyed by it.
func TestSocketSetTimeoutEmitsTimeoutOnIdle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Stay silent; just hold the conn open until the guest closes it.
		io.Copy(io.Discard, conn)
		conn.Close()
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { events: 0 };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.setTimeout(50, () => { r.cb = true; });
		sock.on("timeout", () => {
			r.events++;
			r.destroyedAtTimeout = sock.destroyed === true;
			r.writableAtTimeout = sock.writable === true;
			sock.destroy(); // Node leaves teardown to the app; we end the test here
		});
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `String(r.events)`); got != "1" {
		t.Fatalf("'timeout' fired %s times, want 1 (setTimeout is a no-op?)", got)
	}
	if got := evalStr(t, js, `String(r.cb)`); got != "true" {
		t.Errorf("setTimeout(ms, cb) callback did not fire")
	}
	if got := evalStr(t, js, `String(r.destroyedAtTimeout)`); got != "false" {
		t.Errorf("socket was destroyed at 'timeout' — Node leaves that to the app")
	}
	if got := evalStr(t, js, `String(r.writableAtTimeout)`); got != "true" {
		t.Errorf("socket not writable at 'timeout' — must stay usable")
	}
}

// Incoming data must RESET the idle countdown: with chunks arriving every
// 150ms and a 500ms timeout, 'timeout' may fire only after the stream goes
// silent — i.e. after every chunk has been received.
func TestSocketSetTimeoutResetByIncomingData(t *testing.T) {
	const chunks = 6
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		for i := 0; i < chunks; i++ {
			time.Sleep(150 * time.Millisecond)
			if _, err := conn.Write([]byte("x")); err != nil {
				break
			}
		}
		// Go silent; the guest destroys the socket on its idle timeout.
		io.Copy(io.Discard, conn)
		conn.Close()
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { bytes: 0 };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.setTimeout(500);
		sock.on("data", (d) => { r.bytes += d.length; });
		sock.on("timeout", () => { r.bytesAtTimeout = r.bytes; sock.destroy(); });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	// Total transfer time (6 x 150ms = 900ms) exceeds the 500ms timeout, so a
	// timer that is NOT reset by data would fire mid-stream after ~3 chunks.
	if got := evalStr(t, js, `String(r.bytesAtTimeout)`); got != fmt.Sprint(chunks) {
		t.Errorf("idle timeout fired after %s/%d chunks — incoming data did not reset the timer", got, chunks)
	}
}

// setTimeout(0) must disable a previously armed idle timeout.
func TestSocketSetTimeoutZeroDisables(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Silent well past the (disabled) 50ms timeout, then reply and close.
		time.Sleep(300 * time.Millisecond)
		conn.Write([]byte("bye"))
		conn.Close()
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { timedOut: false, data: "" };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.setTimeout(50);
		sock.setTimeout(0); // disable
		sock.setEncoding("utf8");
		sock.on("timeout", () => { r.timedOut = true; });
		sock.on("data", (d) => { r.data += d; });
		sock.on("close", () => { r.closed = true; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `String(r.timedOut)`); got != "false" {
		t.Errorf("'timeout' fired although setTimeout(0) disabled it")
	}
	if got := evalStr(t, js, `r.data`); got != "bye" {
		t.Errorf("data = %q, want bye", got)
	}
}

// An armed idle timer must not keep the event loop alive once the socket is
// gone: with a 60s timeout configured, the script must still return as soon
// as the connection closes (not after a minute).
func TestSocketSetTimeoutDoesNotHoldEventLoopOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("hi"))
		conn.Close() // peer FIN → guest auto-ends → socket closes
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	start := time.Now()
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1");
		sock.setTimeout(60000);
		sock.on("data", () => {});
		sock.on("close", () => { r.closed = true; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("script took %v — the 60s idle timer kept the loop alive", elapsed)
	}
	if got := evalStr(t, js, `String(r.closed)`); got != "true" {
		t.Errorf("socket never closed")
	}
}

// req.setTimeout/res.setTimeout on the http server shims must delegate to the
// per-connection socket: a handler that stalls its response sees 'timeout'
// fire, and the response can still complete afterwards.
func TestHTTPServerResponseSetTimeoutFiresWhileHandlerStalls(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const http = require("http");
		globalThis.r = { order: [] };
		const server = http.createServer((req, res) => {
			res.setTimeout(80, () => { r.order.push("timeout"); });
			setTimeout(() => { r.order.push("end"); res.end("late"); }, 400);
		});
		server.listen(0, "127.0.0.1", () => {
			http.get("http://127.0.0.1:" + server.address().port + "/", (resp) => {
				let body = "";
				resp.setEncoding("utf8");
				resp.on("data", (d) => { body += d; });
				resp.on("end", () => { r.body = body; server.close(); });
			}).on("error", (e) => { r.err = String(e); });
		});
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("request error: %s", got)
	}
	if got := evalStr(t, js, `r.order.join(",")`); got != "timeout,end" {
		t.Errorf("event order = %q, want timeout,end (res.setTimeout is a no-op?)", got)
	}
	if got := evalStr(t, js, `r.body`); got != "late" {
		t.Errorf("body = %q, want late (timeout must not kill the response)", got)
	}
}

// ClientRequest.setTimeout must emit 'timeout' on the request when the server
// stalls, without aborting the request — the late response still arrives.
func TestHTTPClientRequestSetTimeoutOnStalledServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, "slow-ok")
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		const http = require("http");
		globalThis.r = { order: [] };
		const req = http.get(BASE + "/", (res) => {
			let body = "";
			res.setEncoding("utf8");
			res.on("data", (d) => { body += d; });
			res.on("end", () => { r.order.push("end"); r.body = body; });
		});
		req.setTimeout(80, () => { r.order.push("timeout"); });
		req.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("request error: %s", got)
	}
	if got := evalStr(t, js, `r.order.join(",")`); got != "timeout,end" {
		t.Errorf("event order = %q, want timeout,end (req.setTimeout is a no-op?)", got)
	}
	if got := evalStr(t, js, `r.body`); got != "slow-ok" {
		t.Errorf("body = %q, want slow-ok (timeout must not abort the request)", got)
	}
}
