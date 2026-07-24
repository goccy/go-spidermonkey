package nodejs_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
