package nodejs_test

import (
	"context"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTimersPromisesAbort verifies timers/promises setTimeout rejects with
// AbortError when its signal is (already) aborted.
func TestTimersPromisesAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setTimeout: sleep } = require("timers/promises");
			const c = new AbortController(); c.abort();
			try { await sleep(50, "v", { signal: c.signal }); r.outcome = "resolved"; }
			catch (e) { r.outcome = "rejected:" + e.name; }
		})().catch(e => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("aborted timers/promises = %q, want rejected:AbortError", got)
	}
}

// TestTimersPromisesAbortMidWait verifies aborting during the wait rejects and
// doesn't hang the loop.
func TestTimersPromisesAbortMidWait(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan struct{})
	go func() {
		runScript(t, rt, `
			globalThis.r = {};
			(async () => {
				const { setTimeout: sleep } = require("timers/promises");
				const c = new AbortController();
				setTimeout(() => c.abort(), 10);
				try { await sleep(100000, "v", { signal: c.signal }); r.outcome = "resolved"; }
				catch (e) { r.outcome = "rejected:" + e.name; }
			})().catch(e => { r.err = String(e); });
		`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timers/promises abort-mid-wait hung the loop")
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("abort-mid-wait = %q, want rejected:AbortError", got)
	}
}

// TestTimerRefresh verifies Timeout.refresh() re-arms the timer.
func TestTimerRefresh(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { fires: 0 };
		let n = 0;
		const t = setTimeout(function tick() {
			r.fires++;
			if (++n < 3) t.refresh(); // re-arm twice more
		}, 5);
	`)
	if got := evalVal(t, js, "r.fires").Int(); got != 3 {
		t.Errorf("timer fired %d times with refresh, want 3", got)
	}
}

// TestTimersPromisesSetIntervalAbort verifies timers/promises.setInterval honors
// an AbortSignal — the for-await loop throws AbortError instead of hanging.
func TestTimersPromisesSetIntervalAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { setInterval } = require("timers/promises");
		const ac = new AbortController();
		setTimeout(() => ac.abort(), 30);
		(async () => {
			try {
				let count = 0;
				for await (const _ of setInterval(10, "x", { signal: ac.signal })) { if (++count > 500) break; }
				r.result = "ended:" + count;
			} catch (e) { r.result = "threw:" + e.name; }
		})();
	`)
	if got := evalStr(t, js, `String(r.result ?? "HANG")`); got != "threw:AbortError" {
		t.Errorf("setInterval with aborted signal = %q, want threw:AbortError", got)
	}
}

// TestTimersPromisesSetIntervalNoListenerLeak verifies setInterval removes its
// per-tick abort listener on normal resolution, so a long loop on a shared
// AbortController doesn't accumulate listeners.
func TestTimersPromisesSetIntervalNoListenerLeak(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { setInterval } = require("timers/promises");
		const ac = new AbortController();
		(async () => {
			let count = 0;
			for await (const _ of setInterval(1, "x", { signal: ac.signal })) { if (++count >= 15) break; }
			r.leaked = (ac.signal._listeners.get("abort") || []).length;
		})();
	`)
	if got := evalStr(t, js, `String(r.leaked ?? "?")`); got != "0" {
		t.Errorf("abort listeners after 15 ticks + break = %q, want 0 (leak)", got)
	}
}

// An earlier timer's callback can cancel a later timer that is due in the SAME
// tick — Node guarantees the cancelled callback does not run. The loop takes
// all due timers as a batch, so the fix must let clearTimeout reach a sibling
// already removed from the pending map.
func TestClearTimeoutCancelsSameTickSibling(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		let b;
		// a is registered first and, when it runs, cancels b — which is due in
		// the same tick and already taken into the loop's due batch.
		setTimeout(() => { __order.push("a"); clearTimeout(b); }, 0);
		b = setTimeout(() => { __order.push("b"); }, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "a" {
		t.Fatalf("order = %q, want just \"a\" (b must be cancelled by a)", got)
	}
}

// Inside a callback, setImmediate fires before a setTimeout(0) scheduled
// alongside it: the immediate runs in this turn's check phase, while the timer
// waits for the next turn's timers phase.
func TestImmediateBeforeTimeoutInsideCallback(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		setTimeout(() => {
			setTimeout(() => { __order.push("timeout"); }, 0);
			setImmediate(() => { __order.push("immediate"); });
		}, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "immediate,timeout" {
		t.Fatalf("order = %q, want immediate,timeout", got)
	}
}

// Clearing a timer that already fired in the SAME tick must be a harmless
// no-op, not a double-free of its (already-freed) callback handle.
func TestClearAlreadyFiredSameTickTimer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		// a fires first (and its handle is freed); b then clears a — already fired.
		const a = setTimeout(() => { __order.push("a"); }, 0);
		setTimeout(() => { clearTimeout(a); __order.push("b"); }, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "a,b" {
		t.Fatalf("order = %q, want a,b (clearing a fired timer must not crash)", got)
	}
}

// TestUnrefTimerLetsLoopExit verifies an unref'd timer does not, on its own, keep
// the loop alive: a loop whose only remaining work is an unref'd interval must
// reach idle and return, rather than blocking until the context deadline.
func TestUnrefTimerLetsLoopExit(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const t = setInterval(() => {}, 30000);
			t.unref();
		`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not exit with only an unref'd interval armed (hang)")
	}
}

// TestUnrefServerLetsLoopExit verifies an unref'd listening server does not keep
// the loop alive on its own: with nothing else pending, the loop reaches idle.
func TestUnrefServerLetsLoopExit(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const http = require("http");
			const server = http.createServer(() => {});
			server.listen(0, "127.0.0.1");
			server.unref();
		`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not exit with only an unref'd server listening (hang)")
	}
}

// TestTimerThrowRoutedToUncaught verifies a throw in a timer callback is routed
// to the uncaughtException handler and does NOT tear down the whole loop — the
// interval keeps firing afterwards, matching Node.
func TestTimerThrowRoutedToUncaught(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { ticks: 0, caught: 0 };
		process.on("uncaughtException", () => { r.caught++; });
		let n = 0;
		const t = setInterval(() => {
			n++;
			if (n === 1) throw new Error("boom");
			r.ticks++;
			if (n >= 3) clearInterval(t);
		}, 5);
	`)
	if got := evalVal(t, js, "r.caught").Int(); got < 1 {
		t.Errorf("uncaughtException not invoked: caught=%d", got)
	}
	if got := evalVal(t, js, "r.ticks").Int(); got < 1 {
		t.Errorf("loop stopped after timer throw: ticks=%d", got)
	}
}

// timers/promises.setInterval's async iterator only checked signal.aborted
// before the loop: an abort landing while the consumer's for-await body ran
// (between ticks) was missed, because the next tick re-added an 'abort'
// listener to an already-aborted signal (which never fires) — the loop spun
// forever. Every iteration must re-check and reject with the abort reason.
func TestTimersPromisesSetIntervalAbortBetweenTicks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setInterval } = require("timers/promises");
			const ac = new AbortController();
			let ticks = 0;
			try {
				for await (const v of setInterval(5, "tick", { signal: ac.signal })) {
					ticks++;
					// Abort while the consumer body is running: no wait is
					// pending, so only a per-iteration re-check can see it.
					ac.abort();
				}
				r.outcome = "loop-exited-without-throw";
			} catch (e) {
				r.outcome = e && e.name;
				r.ticks = ticks;
			}
		})();
	`)
	if got := evalStr(t, js, `r.outcome`); got != "AbortError" {
		t.Errorf("outcome = %q, want AbortError", got)
	}
	if got := evalStr(t, js, `String(r.ticks)`); got != "1" {
		t.Errorf("ticks before abort surfaced = %s, want 1", got)
	}
}

// The abort reason must propagate (not be replaced by a generic error).
func TestTimersPromisesSetIntervalAbortReason(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setInterval } = require("timers/promises");
			const ac = new AbortController();
			try {
				for await (const v of setInterval(5, null, { signal: ac.signal })) {
					ac.abort(new Error("custom reason"));
				}
			} catch (e) { r.msg = e && e.message; }
		})();
	`)
	if got := evalStr(t, js, `r.msg`); got != "custom reason" {
		t.Errorf("rejection message = %q, want the abort reason", got)
	}
}

// scheduler.wait(ms, { signal }) ignored its options entirely; it must be
// abortable like timers/promises setTimeout (pre-aborted and mid-wait).
func TestSchedulerWaitHonorsAbortSignal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { scheduler } = require("timers/promises");
		// Pre-aborted: reject immediately.
		{
			const ac = new AbortController();
			ac.abort();
			scheduler.wait(5, { signal: ac.signal }).then(
				() => { r.pre = "resolved"; },
				(e) => { r.pre = e && e.name; });
		}
		// Abort mid-wait: reject well before the timer would fire.
		{
			const ac = new AbortController();
			const t0 = Date.now();
			scheduler.wait(10000, { signal: ac.signal }).then(
				() => { r.mid = "resolved"; },
				(e) => { r.mid = e && e.name; r.fast = (Date.now() - t0) < 5000; });
			setTimeout(() => ac.abort(), 10);
		}
		// No signal: still resolves.
		scheduler.wait(5).then(() => { r.plain = "resolved"; });
	`)
	for expr, want := range map[string]string{
		"r.pre":          "AbortError",
		"r.mid":          "AbortError",
		"String(r.fast)": "true",
		"r.plain":        "resolved",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// timers/promises.setImmediate only checked aborted at call time; an abort
// BEFORE the immediate fires must cancel it and reject with an AbortError.
func TestTimersPromisesSetImmediateAbortBeforeFiring(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const tp = require("timers/promises");
		// Abort after scheduling but before the check phase runs it.
		{
			const ac = new AbortController();
			tp.setImmediate("v", { signal: ac.signal }).then(
				(v) => { r.aborted = "resolved:" + v; },
				(e) => { r.aborted = e && e.name; });
			ac.abort();
		}
		// Abort reason propagates.
		{
			const ac = new AbortController();
			tp.setImmediate("v", { signal: ac.signal }).then(
				() => { r.reason = "resolved"; },
				(e) => { r.reason = e && e.message; });
			ac.abort(new Error("stop now"));
		}
		// Un-aborted still resolves with the value.
		{
			const ac = new AbortController();
			tp.setImmediate("ok", { signal: ac.signal }).then((v) => { r.value = v; });
		}
		// No options at all keeps working (resolves with the value).
		tp.setImmediate("bare").then((v) => { r.bare = v; });
	`)
	for expr, want := range map[string]string{
		"r.aborted": "AbortError",
		"r.reason":  "stop now",
		"r.value":   "ok",
		"r.bare":    "bare",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// Regression tests for loop-liveness accounting: resources that hold an
// AddPending must be able to release their hold on the event loop via
// unref()/pause() (an OFFSET that nets to zero on close/EOF), so common Node
// patterns exit cleanly instead of hanging. Each fix is checked three ways:
//   - unref/pause lets the loop REACH IDLE (bounded rt.Wait returns nil);
//   - WITHOUT unref the loop STAYS ALIVE (bounded rt.Wait hits its deadline);
//   - ref()/resume() after unref RE-PINS the loop (deadline again).

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
