package nodejs_test

import (
	"bufio"
	"context"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// Regression coverage for allowHalfOpen: the host used to treat a clean peer
// FIN (io.EOF) as a full teardown, deleting the connection's writer — a
// subsequent guest write found no writer and was dropped SILENTLY while its
// callback still fired with no error. With the fix, a peer FIN only ends the
// read half; the write half stays usable until the guest calls end()/destroy(),
// which then completes the close.

// A socket with allowHalfOpen:true must still deliver writes issued AFTER the
// peer half-closed (FIN); end() afterwards completes the connection so the
// peer's read-to-EOF finishes.
func TestSocketAllowHalfOpenWriteAfterPeerFinDelivered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- "accept error: " + err.Error()
			return
		}
		defer conn.Close()
		conn.Write([]byte("hello"))
		conn.(*net.TCPConn).CloseWrite() // FIN our write half; keep reading
		b, _ := io.ReadAll(conn)         // the guest's post-FIN writes land here
		received <- string(b)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { data: "" };
		const sock = net.connect({ port: PORT, host: "127.0.0.1", allowHalfOpen: true });
		sock.setEncoding("utf8");
		sock.on("data", (d) => { r.data += d; });
		sock.on("end", () => {
			r.gotEnd = true;
			// The peer already sent its FIN; our write half must still work.
			sock.write("after-fin:" + r.data, () => { r.writeAcked = true; });
			sock.end();
		});
		sock.on("finish", () => { r.finished = true; });
		sock.on("close", () => { r.closed = true; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	select {
	case got := <-received:
		if got != "after-fin:hello" {
			t.Fatalf("peer received %q after its FIN, want %q (write dropped at host layer?)", got, "after-fin:hello")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("peer never saw the post-FIN write / EOF")
	}
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	for expr, want := range map[string]string{
		"String(r.gotEnd)":     "true",
		"String(r.writeAcked)": "true",
		"String(r.finished)":   "true",
		"String(r.closed)":     "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// The default (allowHalfOpen:false) auto-end path must still close the
// connection FULLY after the peer's FIN: the guest socket runs end→finish→
// close and the peer's read-to-EOF completes.
func TestSocketDefaultAutoEndStillClosesConnAfterPeerFin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peerEOF := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("payload"))
		conn.(*net.TCPConn).CloseWrite()
		io.ReadAll(conn) // returns once the guest's auto-end FIN arrives
		close(peerEOF)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1"); // allowHalfOpen defaults to false
		sock.on("data", () => {});
		sock.on("close", () => { r.closed = true; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	select {
	case <-peerEOF:
	case <-time.After(10 * time.Second):
		t.Fatal("peer read never hit EOF — the auto-end FIN was not sent")
	}
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `String(r.closed)`); got != "true" {
		t.Errorf("socket 'close' did not fire after the default auto-end")
	}
}

// A half-open socket the guest destroys WITHOUT ever writing again must tear
// down cleanly (no stranded connection keeping the event loop alive).
func TestSocketAllowHalfOpenDestroyWithoutWriteReleasesConn(t *testing.T) {
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
		conn.(*net.TCPConn).CloseWrite() // immediate FIN, no data
		io.ReadAll(conn)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	start := time.Now()
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect({ port: PORT, host: "127.0.0.1", allowHalfOpen: true });
		sock.on("data", () => {});
		sock.on("end", () => { r.gotEnd = true; sock.destroy(); });
		sock.on("close", () => { r.closed = true; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("script took %v — half-open socket leaked a loop pending after destroy()", elapsed)
	}
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `String(r.gotEnd)`); got != "true" {
		t.Errorf("'end' did not fire on peer FIN")
	}
	if got := evalStr(t, js, `String(r.closed)`); got != "true" {
		t.Errorf("'close' did not fire after destroy()")
	}
}

// Regression coverage for net.Server.close(): it used to emit 'close' (and run
// its callback) on the next tick unconditionally. Node semantics: close() stops
// accepting immediately, but 'close' — and the callback, which Node registers
// as a once('close') listener — fire only after every tracked server-side
// connection has ended (immediately if there are none).

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

// Regression coverage for socket idle timeouts: net.Socket#setTimeout (and the
// http shims that delegate to it) used to be silent no-ops. Node semantics:
// setTimeout(ms[, cb]) arms an idle timer reset by any activity (data, write,
// connect); after ms of inactivity the socket emits 'timeout' (and fires the
// one-shot cb) WITHOUT destroying the socket; setTimeout(0) disables; the
// timer never keeps the event loop alive on its own.

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
