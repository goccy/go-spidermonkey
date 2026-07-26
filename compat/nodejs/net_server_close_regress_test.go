package nodejs_test

// Regression coverage for net.Server.close(): it used to emit 'close' (and run
// its callback) on the next tick unconditionally. Node semantics: close() stops
// accepting immediately, but 'close' — and the callback, which Node registers
// as a once('close') listener — fire only after every tracked server-side
// connection has ended (immediately if there are none).

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// With a connection still open, server.close() must defer 'close' (and the
// callback) until that connection has fully closed.
func TestNetServerCloseWaitsForActiveConnections(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { order: [] };
		const server = net.createServer((sock) => {
			sock.on("data", () => {});
			sock.on("close", () => { r.order.push("conn-closed"); });
			server.close(() => { r.order.push("close-cb"); });
			r.order.push("close-called");
			r.listeningAfterClose = server.listening;
			// End the client a couple of ticks later; only then may the server
			// emit 'close'.
			setTimeout(() => globalThis.__client.end(), 20);
		});
		server.on("close", () => { r.order.push("server-close-event"); });
		server.listen(0, "127.0.0.1", () => {
			const client = net.connect(server.address().port, "127.0.0.1");
			globalThis.__client = client;
			client.on("data", () => {});
			client.on("error", (e) => { r.err = String(e); });
		});
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("client error: %s", got)
	}
	// Accepting must stop immediately even though 'close' is deferred.
	if got := evalStr(t, js, `String(r.listeningAfterClose)`); got != "false" {
		t.Errorf("server.listening = %s right after close(), want false", got)
	}
	want := "close-called,conn-closed,server-close-event,close-cb"
	if got := evalStr(t, js, `r.order.join(",")`); got != want {
		t.Errorf("event order = %q, want %q ('close' fired before the connection ended?)", got, want)
	}
}

// With no connections, close() emits 'close' and runs the callback promptly
// (and only once), and a second close() reports ERR_SERVER_NOT_RUNNING.
func TestNetServerCloseImmediateWhenNoConnections(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { closes: 0 };
		const server = net.createServer(() => {});
		server.on("close", () => { r.closes++; });
		server.listen(0, "127.0.0.1", () => {
			server.close((err) => { r.cbErr = err ? String(err.code || err) : "none"; });
			server.close((err) => { r.secondErr = (err && err.code) || "none"; });
		});
	`)
	if got := evalStr(t, js, `String(r.closes)`); got != "1" {
		t.Errorf("'close' emitted %s times, want 1", got)
	}
	if got := evalStr(t, js, `String(r.cbErr)`); got != "none" {
		t.Errorf("first close() callback err = %q, want none", got)
	}
	if got := evalStr(t, js, `String(r.secondErr)`); got != "ERR_SERVER_NOT_RUNNING" {
		t.Errorf("second close() callback err = %q, want ERR_SERVER_NOT_RUNNING", got)
	}
}

// Two overlapping connections: 'close' must wait for BOTH to end, not just
// the first.
func TestNetServerCloseWaitsForAllConnections(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { order: [], conns: 0 };
		const clients = [];
		const server = net.createServer((sock) => {
			sock.on("data", () => {});
			const n = ++r.conns;
			sock.on("close", () => { r.order.push("conn" + n + "-closed"); });
			if (n === 2) {
				server.close();
				r.order.push("close-called");
				// Stagger the client teardowns.
				setTimeout(() => clients[0].end(), 20);
				setTimeout(() => clients[1].end(), 60);
			}
		});
		server.on("close", () => { r.order.push("server-closed"); });
		server.listen(0, "127.0.0.1", () => {
			const port = server.address().port;
			for (let i = 0; i < 2; i++) {
				const c = net.connect(port, "127.0.0.1");
				c.on("data", () => {});
				c.on("error", (e) => { r.err = String(e); });
				clients.push(c);
			}
		});
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("client error: %s", got)
	}
	if got := evalStr(t, js, `r.order[0]`); got != "close-called" {
		t.Fatalf("order[0] = %q, want close-called (got order %s)", got, evalStr(t, js, `r.order.join(",")`))
	}
	if got := evalStr(t, js, `r.order[r.order.length - 1]`); got != "server-closed" {
		t.Errorf("server 'close' was not the last event: order = %s", evalStr(t, js, `r.order.join(",")`))
	}
	if got := evalStr(t, js, `String(r.order.filter((x) => x.endsWith("-closed") && x.startsWith("conn")).length)`); got != "2" {
		t.Errorf("expected both connection closes before server 'close': order = %s", evalStr(t, js, `r.order.join(",")`))
	}
}
