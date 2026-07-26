package nodejs_test

import (
	"net"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
