package nodejs_test

// Regression tests for loop-liveness accounting: resources that hold an
// AddPending must be able to release their hold on the event loop via
// unref()/pause() (an OFFSET that nets to zero on close/EOF), so common Node
// patterns exit cleanly instead of hanging. Each fix is checked three ways:
//   - unref/pause lets the loop REACH IDLE (bounded rt.Wait returns nil);
//   - WITHOUT unref the loop STAYS ALIVE (bounded rt.Wait hits its deadline);
//   - ref()/resume() after unref RE-PINS the loop (deadline again).

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// waitIdle drives the loop with a bounded deadline. It returns nil when the loop
// reaches idle before the deadline, or a non-nil (context) error when the loop
// stays alive for the whole window.
func waitIdle(rt interface {
	Wait(context.Context) error
}, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return rt.Wait(ctx)
}

const (
	idleBudget  = 3 * time.Second        // generous: a correct loop returns well under this
	aliveBudget = 400 * time.Millisecond // a genuinely-alive loop blocks the whole window
)

// goListener accepts and holds TCP connections so a guest net.Socket has a real
// peer to connect to. Connections are closed at test teardown.
func goListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().(*net.TCPAddr).Port
}

// --------------------------------------------------------------- net.Socket

func TestSocketUnrefLetsLoopIdle(t *testing.T) {
	port := goListener(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.r = {};
		const net = require("net");
		const c = net.connect(%d, "127.0.0.1", () => { c.unref(); r.connected = true; });
		c.on("error", (e) => { r.err = String(e); });
	`, port)); err != nil {
		t.Fatal(err)
	}
	// An open, connected-then-unref'd socket must not keep the loop alive.
	if err := waitIdle(rt, idleBudget); err != nil {
		t.Fatalf("socket.unref() did not let the loop idle: %v", err)
	}
	if got := evalStr(t, js, "String(r.connected ?? '')"); got != "true" {
		t.Fatalf("socket never connected (err=%s)", evalStr(t, js, "String(r.err ?? '')"))
	}
}

func TestSocketWithoutUnrefStaysAlive(t *testing.T) {
	port := goListener(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.r = {};
		const net = require("net");
		const c = net.connect(%d, "127.0.0.1", () => { r.connected = true; });
		c.on("error", (e) => { r.err = String(e); });
	`, port)); err != nil {
		t.Fatal(err)
	}
	// Without unref an open socket keeps the loop alive: Wait hits the deadline.
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("open socket without unref() unexpectedly let the loop idle")
	}
	if got := evalStr(t, js, "String(r.connected ?? '')"); got != "true" {
		t.Fatalf("socket never connected (err=%s)", evalStr(t, js, "String(r.err ?? '')"))
	}
}

func TestSocketRefAfterUnrefRepins(t *testing.T) {
	port := goListener(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.r = {};
		const net = require("net");
		const c = net.connect(%d, "127.0.0.1", () => { c.unref(); c.ref(); r.connected = true; });
		c.on("error", (e) => { r.err = String(e); });
	`, port)); err != nil {
		t.Fatal(err)
	}
	// ref() after unref() re-pins the loop, so it stays alive to the deadline.
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("socket.ref() after unref() did not re-pin the loop")
	}
	if got := evalStr(t, js, "String(r.connected ?? '')"); got != "true" {
		t.Fatalf("socket never connected (err=%s)", evalStr(t, js, "String(r.err ?? '')"))
	}
}

// --------------------------------------------------------------- net.Server

func TestNetServerUnrefLetsLoopIdle(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		globalThis.r = {};
		const net = require("net");
		try {
			const srv = net.createServer().listen(0);
			srv.unref();
			r.ok = true;
		} catch (e) { r.threw = String(e); }
	`); err != nil {
		t.Fatal(err)
	}
	// net.Server.unref() must not throw (it was previously missing entirely).
	if got := evalStr(t, js, "String(r.threw ?? '')"); got != "" {
		t.Fatalf("net.Server.unref() threw: %s", got)
	}
	if got := evalStr(t, js, "String(r.ok ?? '')"); got != "true" {
		t.Fatal("net.Server.unref() path did not complete")
	}
	if err := waitIdle(rt, idleBudget); err != nil {
		t.Fatalf("net.Server.unref() did not let the loop idle: %v", err)
	}
}

func TestNetServerWithoutUnrefStaysAlive(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		const net = require("net");
		globalThis.srv = net.createServer().listen(0);
	`); err != nil {
		t.Fatal(err)
	}
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("listening net.Server without unref() unexpectedly let the loop idle")
	}
}

func TestNetServerRefAfterUnrefRepins(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		const net = require("net");
		globalThis.srv = net.createServer().listen(0);
		srv.unref();
		srv.ref();
	`); err != nil {
		t.Fatal(err)
	}
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("net.Server.ref() after unref() did not re-pin the loop")
	}
}

// --------------------------------------------------------------- dgram.Socket

func TestDgramUnrefLetsLoopIdle(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		globalThis.r = {};
		const dgram = require("dgram");
		const s = dgram.createSocket("udp4");
		s.bind(0);
		s.unref();
		r.ok = true;
	`); err != nil {
		t.Fatal(err)
	}
	if got := evalStr(t, js, "String(r.ok ?? '')"); got != "true" {
		t.Fatal("dgram bind/unref path did not complete")
	}
	if err := waitIdle(rt, idleBudget); err != nil {
		t.Fatalf("dgram.unref() did not let the loop idle: %v", err)
	}
}

func TestDgramWithoutUnrefStaysAlive(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		const dgram = require("dgram");
		globalThis.s = dgram.createSocket("udp4");
		s.bind(0);
	`); err != nil {
		t.Fatal(err)
	}
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("bound dgram socket without unref() unexpectedly let the loop idle")
	}
}

func TestDgramRefAfterUnrefRepins(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		const dgram = require("dgram");
		globalThis.s = dgram.createSocket("udp4");
		s.bind(0);
		s.unref();
		s.ref();
	`); err != nil {
		t.Fatal(err)
	}
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("dgram.ref() after unref() did not re-pin the loop")
	}
}

// --------------------------------------------------------------- process.stdin

// blockingPipe returns a reader that never reaches EOF on its own (an io.Pipe
// whose write end is left open) — a stand-in for an interactive stdin. The read
// goroutine started by stdin_start blocks on it; it is reaped at process
// teardown, so the test never closes it (closing would trip EOF handling after
// the runtime is torn down).
func blockingPipe() (io.Reader, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return pr, pw
}

func TestStdinPauseLetsLoopIdle(t *testing.T) {
	pr, _ := blockingPipe()
	js, rt := newRuntime(t, spidermonkey.Config{Stdin: pr})
	if _, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.stdin.on("data", () => {}); // starts (and refs) stdin
		process.stdin.pause();              // releases stdin's hold on the loop
	`); err != nil {
		t.Fatal(err)
	}
	// pause() lets the loop idle even though stdin never reaches EOF.
	if err := waitIdle(rt, idleBudget); err != nil {
		t.Fatalf("process.stdin.pause() did not let the loop idle: %v", err)
	}
}

func TestStdinResumedStaysAliveAndDeliversData(t *testing.T) {
	pr, pw := blockingPipe()
	js, rt := newRuntime(t, spidermonkey.Config{Stdin: pr})
	if _, err := js.Eval(context.Background(), `
		globalThis.r = { data: "" };
		process.stdin.on("data", (d) => { r.data += d.toString(); });
	`); err != nil {
		t.Fatal(err)
	}
	// Deliver a chunk from the host; the write is consumed by the reader goroutine.
	go func() { pw.Write([]byte("hello")) }()
	// A resumed (never-paused) stdin keeps the loop alive: Wait hits the deadline.
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("stdin with a live 'data' listener unexpectedly let the loop idle")
	}
	// ...and the delivered chunk reached the 'data' handler.
	if got := evalStr(t, js, "r.data"); got != "hello" {
		t.Fatalf("stdin 'data' delivered %q, want %q", got, "hello")
	}
}

func TestStdinResumeAfterPauseRepins(t *testing.T) {
	pr, _ := blockingPipe()
	js, rt := newRuntime(t, spidermonkey.Config{Stdin: pr})
	if _, err := js.Eval(context.Background(), `
		process.stdin.on("data", () => {});
		process.stdin.pause();
		process.stdin.resume();
	`); err != nil {
		t.Fatal(err)
	}
	// resume() after pause() re-refs stdin's hold, keeping the loop alive.
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("process.stdin.resume() after pause() did not re-pin the loop")
	}
}

// TestStdinPausedEOFDoesNotStrandOffset: a paused stdin that reaches EOF must
// rebalance its unref offset (the host releases the pending unconditionally),
// so other ref'd work (a listening server) still keeps the loop alive. Before
// the fix the offset was undone only via a stream 'end' event, which a paused
// stdin never emits, underflowing the global count and dropping the server.
func TestStdinPausedEOFDoesNotStrandOffset(t *testing.T) {
	// strings.Reader yields one chunk then EOF, driving stdin to end while paused.
	js, rt := newRuntime(t, spidermonkey.Config{Stdin: strings.NewReader("hi")})
	if _, err := js.Eval(context.Background(), `
		const net = require("net");
		process.stdin.on("data", () => {}); // start + ref stdin
		process.stdin.pause();              // unref (offset applied)
		globalThis.srv = net.createServer().listen(0); // a genuinely ref'd resource
	`); err != nil {
		t.Fatal(err)
	}
	// The listening server must keep the loop alive across the paused-stdin EOF:
	// waitIdle must hit the deadline, not return nil (which would mean the
	// stranded unref offset underflowed the count and dropped the server).
	if err := waitIdle(rt, aliveBudget); err == nil {
		t.Fatal("loop idled while a server was listening — paused-stdin EOF stranded the unref offset")
	}
}
