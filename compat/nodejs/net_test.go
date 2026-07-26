package nodejs_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestNetClientEchoesGoServer(t *testing.T) {
	// A Go TCP server that upper-cases each line; the guest connects, sends,
	// and reads the reply through node:net.
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
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		fmt.Fprint(conn, "ECHO:"+line)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1", () => {
			r.connected = true;
			sock.write("hello\n");
		});
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; sock.end(); });
		sock.on("close", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if !evalVal(t, js, `r.connected`).Bool() {
		t.Error("connect callback did not fire")
	}
	if got := evalStr(t, js, `r.reply`); got != "ECHO:hello\n" {
		t.Errorf("reply = %q, want %q", got, "ECHO:hello\n")
	}
}

// TestNetClientBackpressureLargeStream drives a payload far larger than a single
// read chunk through the read-flow-control path (Socket._read → net_read_resume),
// verifying that a slow guest reader still receives every byte with no loss or
// deadlock across the many resume cycles.
func TestNetClientBackpressureLargeStream(t *testing.T) {
	const total = 4 << 20 // 4 MiB, ~128 read chunks
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
		defer conn.Close()
		payload := make([]byte, total)
		for i := range payload {
			payload[i] = byte(i)
		}
		conn.Write(payload)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1");
		let bytes = 0, ok = true;
		sock.on("data", (d) => {
			for (let i = 0; i < d.length; i++) {
				if (d[i] !== ((bytes + i) & 0xff)) ok = false;
			}
			bytes += d.length;
		});
		sock.on("end", () => { r.bytes = bytes; r.ok = ok; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalVal(t, js, `r.bytes ?? -1`).Int(); got != total {
		t.Fatalf("received %d bytes, want %d", got, total)
	}
	if !evalVal(t, js, `r.ok`).Bool() {
		t.Error("payload content mismatch")
	}
}

// TestNetDestroyDuringConnect verifies destroying a socket while its async
// connect is still in flight does NOT later emit spurious connect/error/end
// events when the dial finally completes.
func TestNetDestroyDuringConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { connect: false, error: false, end: false };
		const sock = net.connect(PORT, "127.0.0.1", () => { r.connect = true; });
		sock.on("error", () => { r.error = true; });
		sock.on("end", () => { r.end = true; });
		sock.on("connect", () => { r.connect = true; });
		sock.destroy(); // abandon before the async dial completes
	`)
	if evalVal(t, js, "r.connect").Bool() {
		t.Error("destroyed-during-connect socket still emitted 'connect'")
	}
	if evalVal(t, js, "r.error").Bool() {
		t.Error("destroyed-during-connect socket emitted a spurious 'error'")
	}
	if evalVal(t, js, "r.end").Bool() {
		t.Error("destroyed-during-connect socket emitted a spurious 'end'")
	}
}

func TestNetServerAcceptsGoClient(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, waitDone := startServer(t, js, rt, `
		const net = require("net");
		const server = net.createServer((sock) => {
			sock.on("data", (d) => sock.write("srv:" + d.toString()));
		});
		server.listen(0, "127.0.0.1");
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	_ = waitDone

	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "ping")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "srv:ping" {
		t.Errorf("server reply = %q, want srv:ping", got)
	}
}

func TestNetConnectDenied(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Dial: func(network, host, ip string, port int) bool { return false },
	})
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(9999, "127.0.0.1");
		sock.on("error", (e) => { r.code = e.code; });
	`)
	if got := evalStr(t, js, `r.code`); got != "EACCES" {
		t.Errorf("denied connect code = %q, want EACCES", got)
	}
}

func TestHTTPRequestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Echo", "1")
			fmt.Fprintf(w, "got:%s", body)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello client")
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))

	runScript(t, rt, `
		const http = require("http");
		globalThis.r = {};
		http.get(BASE + "/", (res) => {
			r.status = res.statusCode;
			r.ct = res.headers["content-type"];
			let body = "";
			res.setEncoding("utf8");
			res.on("data", (c) => { body += c; });
			res.on("end", () => { r.body = body; });
		}).on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("http.get error: %s", got)
	}
	if got := evalVal(t, js, `r.status`).Int(); got != 200 {
		t.Errorf("status = %d", got)
	}
	if got := evalStr(t, js, `r.body`); got != "hello client" {
		t.Errorf("body = %q", got)
	}

	// POST with a body.
	runScript(t, rt, `
		globalThis.r2 = {};
		const req = require("http").request(BASE + "/echo", { method: "POST" }, (res) => {
			r2.echoHeader = res.headers["x-echo"];
			let body = "";
			res.on("data", (c) => { body += c.toString(); });
			res.on("end", () => { r2.body = body; });
		});
		req.write("payload");
		req.end();
	`)
	if got := evalStr(t, js, `r2.body`); got != "got:payload" {
		t.Errorf("POST echo = %q", got)
	}
	if got := evalStr(t, js, `r2.echoHeader`); got != "1" {
		t.Errorf("response header = %q", got)
	}
}

// A write issued BEFORE the socket connects must be buffered and flushed on
// connect (not lost), and the connect itself must not block the loop.
func TestNetWriteBeforeConnectBuffered(t *testing.T) {
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
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		fmt.Fprint(conn, "GOT:"+line)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1");
		sock.write("early\n"); // BEFORE 'connect' fires — must be buffered
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; sock.end(); });
		sock.on("close", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `r.reply`); got != "GOT:early\n" {
		t.Fatalf("reply = %q, want GOT:early\\n (pre-connect write lost?)", got)
	}
}

// A clean local socket close (socket.destroy()) must NOT emit a spurious
// 'error' event.
func TestNetCleanCloseNoError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e == nil {
			io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { errored: false };
		const sock = net.connect(PORT, "127.0.0.1", () => {
			sock.write("hi");
			sock.destroy(); // clean local close
		});
		sock.on("error", () => { r.errored = true; });
	`)
	if evalStr(t, js, `String(r.errored)`) == "true" {
		t.Fatalf("clean socket.destroy() emitted a spurious 'error'")
	}
}

// A write issued before an async connect that then FAILS must still fire its
// _write callback (draining the queued write), so the socket's Writable doesn't
// hang forever.
func TestNetWriteBeforeFailedConnectAcked(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	// Port 1 is (almost) always refused.
	if _, err := rt.RunScript(context.Background(), `
		globalThis.r = { wroteCb: false, errored: false, finished: false };
		const net = require("net");
		const sock = net.connect(1, "127.0.0.1");
		sock.write("early", () => { r.wroteCb = true; });
		sock.on("error", () => { r.errored = true; });
		sock.on("finish", () => { r.finished = true; });
		sock.end();
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if evalStr(t, js, `String(r.wroteCb)`) != "true" {
		t.Fatalf("write callback never fired after a failed connect (stranded → Writable hang)")
	}
}

// TestNetSocketEndSendsFIN verifies socket.end() half-closes the write side so a
// Go peer that reads to EOF unblocks and can reply.
func TestNetSocketEndSendsFIN(t *testing.T) {
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
		defer conn.Close()
		// Read the whole request until EOF (the guest's FIN), then reply.
		body, _ := bufio.NewReader(conn).ReadString(0) // reads until EOF
		_ = body
		conn.Write([]byte("REPLY"))
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1", () => {
			sock.write("request-data");
			sock.end(); // must send FIN so the peer's read-to-EOF completes
		});
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; });
		sock.on("end", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, "String(r.reply ?? '')"); got != "REPLY" {
		t.Errorf("no reply after end()/FIN (got %q) — peer likely hung on read", got)
	}
}

// TestSocketSingleClose verifies a Socket (Duplex) emits 'close' exactly once
// over a normal peer-closes-then-we-end lifecycle.
func TestSocketSingleClose(t *testing.T) {
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
		conn.Close() // peer FIN after sending
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { closes: 0 };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.on("data", () => {});
		sock.on("close", () => { r.closes++; });
		sock.on("end", () => sock.end());
		sock.on("error", () => {});
	`)
	if got := evalVal(t, js, "r.closes").Int(); got != 1 {
		t.Errorf("socket emitted 'close' %d times, want exactly 1", got)
	}
}

// TestSocketClosesAfterPeerFin verifies a net.Socket emits 'close' after the
// peer half-closes (FIN) even if the client never calls end() — the writable
// half is auto-ended (allowHalfOpen:false, Node default), so 'finish'/'close'
// fire. Libraries (redis/pg) key reconnect/cleanup on 'close'.
func TestSocketClosesAfterPeerFin(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const net = require("net");
		const server = net.createServer((sock) => { sock.end("bye"); });
		server.listen(0, () => {
			const port = server.address().port;
			const client = net.connect(port, "127.0.0.1");
			client.on("data", () => {});
			client.on("close", () => { r.clientClosed = true; server.close(); });
		});
	`)
	if got := evalStr(t, js, `String(r.clientClosed)`); got != "true" {
		t.Errorf("client 'close' did not fire after peer FIN = %q, want true (allowHalfOpen)", got)
	}
}

// TestServerCloseEmitsCloseOnce verifies net.Server.close() emits 'close' exactly
// once and hands a second close() an ERR_SERVER_NOT_RUNNING callback.
func TestServerCloseEmitsCloseOnce(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { closes: 0 };
		const net = require("net");
		const server = net.createServer(() => {});
		server.on("close", () => { r.closes++; });
		server.listen(0, () => {
			server.close(() => { r.firstCbErr = "none"; });
			server.close((e) => { r.secondErr = (e && e.code) || "none"; });
		});
	`)
	if got := evalStr(t, js, `String(r.closes)`); got != "1" {
		t.Errorf("'close' emitted %q times, want 1", got)
	}
	if got := evalStr(t, js, `String(r.secondErr)`); got != "ERR_SERVER_NOT_RUNNING" {
		t.Errorf("second close() callback err = %q, want ERR_SERVER_NOT_RUNNING", got)
	}
}

// TestNetServerOptionsAndListener: net.createServer([options][, listener]) —
// the two-arg form must keep the connection listener (Node signature). Passing
// options as the only arg previously dropped the handler and, on a connection,
// wedged the event loop (the server socket's pending was never released).
func TestNetServerOptionsAndListener(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, waitDone := startServer(t, js, rt, `
		const net = require("net");
		// Two-arg (options, listener) form: the listener must be kept, not
		// dropped in favor of the options object.
		const server = net.createServer({ pauseOnConnect: false }, (sock) => {
			sock.on("data", (d) => sock.write("srv:" + d.toString()));
		});
		server.listen(0, "127.0.0.1");
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	_ = waitDone

	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "ping")
	buf := make([]byte, 64)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "srv:ping" {
		t.Errorf("server reply = %q, want srv:ping (handler dropped by options arg?)", got)
	}
}
