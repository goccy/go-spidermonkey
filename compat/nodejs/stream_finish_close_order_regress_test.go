package nodejs_test

// Regression coverage for stream event ordering: 'close' must be the LAST
// event a stream emits. The writable state used to be marked finished
// synchronously while 'finish' itself was deferred to a nextTick, so a Duplex
// whose 'end' handler calls end() (the auto-end-after-peer-FIN socket path)
// emitted 'close' BEFORE the deferred 'finish'.

import (
	"net"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// Plain Writable: end() → 'finish' then 'close', never the reverse; listeners
// attached right after end() still catch 'finish' (deferred emit preserved).
func TestWritableEmitsFinishBeforeClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Writable } = require("stream");
		globalThis.r = { order: [] };
		const ws = new Writable({ write(c, e, cb) { cb(); } });
		ws.end("x");
		// Attached AFTER end() — the deferred 'finish' must still reach them.
		ws.on("finish", () => r.order.push("finish"));
		ws.on("close", () => r.order.push("close"));
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "finish,close" {
		t.Errorf("event order = %q, want finish,close", got)
	}
}

// Duplex whose 'end' handler calls end() — the exact shape of the socket
// auto-end path — must emit end → finish → close, with 'close' last.
func TestDuplexEndInsideEndHandlerEmitsFinishBeforeClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Duplex } = require("stream");
		globalThis.r = { order: [] };
		const d = new Duplex({ read() {}, write(c, e, cb) { cb(); } });
		d.on("end", () => { r.order.push("end"); d.end(); });
		d.on("finish", () => r.order.push("finish"));
		d.on("close", () => r.order.push("close"));
		d.resume();
		d.push(null);
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "end,finish,close" {
		t.Errorf("event order = %q, want end,finish,close ('close' must be last)", got)
	}
}

// Real socket: after the peer's FIN the default auto-end must produce
// end → finish → close on the net.Socket, 'close' strictly last.
func TestSocketAutoEndAfterPeerFinEmitsCloseLast(t *testing.T) {
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
		conn.Write([]byte("data"))
		conn.Close() // FIN → guest auto-ends its writable half
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { order: [] };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.on("data", () => {});
		sock.on("end", () => r.order.push("end"));
		sock.on("finish", () => r.order.push("finish"));
		sock.on("close", () => r.order.push("close"));
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `r.order.join(",")`); got != "end,finish,close" {
		t.Errorf("socket event order = %q, want end,finish,close ('close' must be last)", got)
	}
}
