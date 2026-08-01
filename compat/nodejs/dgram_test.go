package nodejs_test

import (
	"context"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"net"
	"testing"
	"testing/fstest"
	"time"
)

func TestDgramReceivesFromGo(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, waitDone := startServer(t, js, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		globalThis.received = null;
		sock.on("message", (msg, rinfo) => {
			globalThis.received = msg.toString();
			globalThis.rinfoPort = rinfo.port;
		});
		sock.bind(0, "127.0.0.1", () => {
			globalThis.PORT = sock.address().port;
		});
		globalThis.__server = { close: () => sock.close() };
	`)
	_ = waitDone

	conn, err := net.Dial("udp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "hello udp")

	// Poll for delivery on the loop.
	deadline := waitForCondition(t, js, `globalThis.received !== null`)
	if !deadline {
		t.Fatal("datagram not received")
	}
	if got := evalStr(t, js, `String(received)`); got != "hello udp" {
		t.Errorf("received = %q", got)
	}
}

func TestDgramSendToGo(t *testing.T) {
	// Go UDP server; the guest sends a datagram to it.
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _, err := conn.ReadFromUDP(buf)
		if err == nil {
			got <- string(buf[:n])
		}
	}()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		sock.bind(0, "127.0.0.1", () => {
			sock.send("ping from guest", PORT, "127.0.0.1", () => sock.close());
		});
	`)
	select {
	case msg := <-got:
		if msg != "ping from guest" {
			t.Errorf("Go received %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Go server received nothing")
	}
}

// waitForCondition polls the guest expression while the server's background
// Wait loop drives delivery (the UDP datagram arrives as a posted task).
func waitForCondition(t *testing.T, js *spidermonkey.JS, expr string) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if evalVal(t, js, expr).Bool() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return evalVal(t, js, expr).Bool()
}

// TestDgramUDP6Family verifies a udp6 socket reports family "IPv6".
func TestDgramUDP6Family(t *testing.T) {
	// Send a datagram to a guest udp6 socket from Go and check rinfo.family.
	js, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const dgram = require("dgram");
			globalThis.r = {};
			const sock = dgram.createSocket("udp6");
			sock.on("message", (msg, rinfo) => { r.family = rinfo.family; sock.close(); });
			sock.bind(0, "::1", () => { globalThis.__PORT = sock.address().port; });
		`)
		done <- err
	}()
	// Wait for bind, then send a udp6 datagram.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := evalStr(t, js, "String(globalThis.__PORT ?? '')"); p != "" {
			conn, err := net.Dial("udp6", "[::1]:"+p)
			if err == nil {
				conn.Write([]byte("hi"))
				conn.Close()
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("udp6 message not received")
	}
	if got := evalStr(t, js, "String(r.family ?? '')"); got != "IPv6" {
		t.Errorf("udp6 rinfo.family = %q, want IPv6", got)
	}
}

// A udp4 socket sending to "localhost" must resolve in the IPv4 family, and
// send() must accept a LIST of parts and deliver their concatenation as one
// datagram.
//
// Both were broken together, and the visible symptom was a HANG rather than a
// failure: "localhost" resolved to ::1, the send from the v4 socket errored, so
// the 'message' event never arrived, so nothing closed the socket and the event
// loop never went idle. That is the shape of 32 quarantined dgram tests.
func TestDgramResolvesInSocketFamilyAndSendsParts(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:      fstest.MapFS{},
		Listen:  func(network, addr string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	r, err := rt.RunScript(ctx, `
		const dgram = require("dgram");
		globalThis.log = [];
		const socket = dgram.createSocket("udp4");
		socket.on("message", (msg) => { log.push("msg:" + msg.toString()); socket.close(); });
		socket.on("error", (e) => log.push("error:" + e.message));
		socket.bind(() => socket.send(["foo", "bar", "baz"], socket.address().port, "localhost"));
	`)
	if err != nil {
		t.Fatalf("the run did not finish (%v) — the datagram never arrived, so nothing closed the socket", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the loop only ended at the deadline", elapsed)
	}
	v, err := js.Eval(context.Background(), `globalThis.log.join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Value.String(); got != "msg:foobarbaz" {
		t.Errorf("events = %q, want \"msg:foobarbaz\"", got)
	}
}

// udpRecvServer opens a Go UDP listener and returns its port plus a channel that
// receives the payload of each datagram it reads.
func udpRecvServer(t *testing.T) (int, chan string) {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, got
}

// send() before bind() must implicitly bind to an ephemeral port and deliver the
// datagram (Node behavior), without spuriously emitting 'error'. The script
// closes the socket in the send callback so the event loop drains.
func TestDgramSendAutoBindsBeforeBind(t *testing.T) {
	port, got := udpRecvServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))

	runScript(t, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		globalThis.r = { errEmitted: "null" };
		sock.on("error", (e) => { r.errEmitted = String(e); });
		// send() with no prior bind() must auto-bind, not fail "socket closed".
		sock.send("auto-bind", PORT, "127.0.0.1", (e) => { r.cbErr = e ? String(e) : "null"; sock.close(); });
	`)
	select {
	case msg := <-got:
		if msg != "auto-bind" {
			t.Errorf("Go received %q, want auto-bind", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Go server received nothing (auto-bind send failed)")
	}
	if got := evalStr(t, js, `String(r.cbErr)`); got != "null" {
		t.Errorf("send callback error = %q, want null", got)
	}
	if got := evalStr(t, js, `String(r.errEmitted)`); got != "null" {
		t.Errorf("unexpected 'error' emit on normal auto-bind send: %q", got)
	}
}

// address() reports the actual bound local address/port/family from the Go conn,
// not a hardcoded value.
func TestDgramAddressReportsRealBinding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		globalThis.r = {};
		sock.bind(0, "127.0.0.1", () => {
			const a = sock.address();
			r.address = a.address;
			r.family = a.family;
			r.portPositive = a.port > 0;
			sock.close();
		});
	`)
	if got := evalStr(t, js, `r.address`); got != "127.0.0.1" {
		t.Errorf("address = %q, want 127.0.0.1", got)
	}
	if got := evalStr(t, js, `r.family`); got != "IPv4" {
		t.Errorf("family = %q, want IPv4", got)
	}
	if got := evalStr(t, js, `String(r.portPositive)`); got != "true" {
		t.Errorf("bound port not positive")
	}
}

// connect()/remoteAddress()/disconnect() work as connected-UDP: send with no
// destination goes to the connected peer, and remoteAddress reflects it;
// disconnect() clears it and remoteAddress() then throws.
func TestDgramConnectedSocket(t *testing.T) {
	port, got := udpRecvServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))

	runScript(t, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		globalThis.r = {};
		sock.connect(PORT, "127.0.0.1", () => {
			const ra = sock.remoteAddress();
			r.remotePort = ra.port;
			r.remoteAddr = ra.address;
			// send() with no destination goes to the connected peer.
			sock.send("connected-hi", (e) => {
				r.sendErr = e ? String(e) : "null";
				sock.disconnect();
				try { sock.remoteAddress(); r.afterDisconnect = "no-throw"; }
				catch (err) { r.afterDisconnect = err.code; }
				sock.close();
			});
		});
	`)
	select {
	case msg := <-got:
		if msg != "connected-hi" {
			t.Errorf("Go received %q, want connected-hi", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Go server received nothing on connected send")
	}
	if got := evalStr(t, js, `String(r.remotePort)`); got != evalStr(t, js, `String(PORT)`) {
		t.Errorf("remoteAddress port = %s, want %s", got, evalStr(t, js, `String(PORT)`))
	}
	if got := evalStr(t, js, `r.remoteAddr`); got != "127.0.0.1" {
		t.Errorf("remoteAddress address = %q", got)
	}
	if got := evalStr(t, js, `String(r.sendErr)`); got != "null" {
		t.Errorf("connected send error = %q", got)
	}
	if got := evalStr(t, js, `r.afterDisconnect`); got != "ERR_SOCKET_DGRAM_NOT_CONNECTED" {
		t.Errorf("remoteAddress after disconnect = %q, want ERR_SOCKET_DGRAM_NOT_CONNECTED", got)
	}
}

// The multicast / socket-option methods exist and do not throw (they are
// documented no-ops where the host bridge can't reach the fd); buffer-size
// getters return numbers.
func TestDgramMulticastAndOptionMethods(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const dgram = require("dgram");
		const sock = dgram.createSocket("udp4");
		globalThis.r = {};
		sock.bind(0, "127.0.0.1", () => {
			try {
				sock.setBroadcast(true);
				sock.setTTL(64);
				sock.setMulticastTTL(1);
				sock.setMulticastLoopback(true);
				sock.setMulticastInterface("0.0.0.0");
				sock.addMembership("224.0.0.114");
				sock.dropMembership("224.0.0.114");
				sock.setRecvBufferSize(1 << 16);
				sock.setSendBufferSize(1 << 16);
				r.recvBuf = sock.getRecvBufferSize();
				r.sendBuf = sock.getSendBufferSize();
				r.ok = true;
			} catch (e) {
				r.ok = false;
				r.err = String(e);
			}
			sock.close();
		});
	`)
	if got := evalStr(t, js, `String(r.ok)`); got != "true" {
		t.Fatalf("option methods threw: %s", evalStr(t, js, `String(r.err)`))
	}
	if got := evalStr(t, js, `String(typeof r.recvBuf === "number" && r.recvBuf > 0)`); got != "true" {
		t.Errorf("getRecvBufferSize did not return a positive number")
	}
	if got := evalStr(t, js, `String(typeof r.sendBuf === "number" && r.sendBuf > 0)`); got != "true" {
		t.Errorf("getSendBufferSize did not return a positive number")
	}
}
