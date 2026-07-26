package nodejs_test

// Regression coverage for allowHalfOpen: the host used to treat a clean peer
// FIN (io.EOF) as a full teardown, deleting the connection's writer — a
// subsequent guest write found no writer and was dropped SILENTLY while its
// callback still fired with no error. With the fix, a peer FIN only ends the
// read half; the write half stays usable until the guest calls end()/destroy(),
// which then completes the close.

import (
	"io"
	"net"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
