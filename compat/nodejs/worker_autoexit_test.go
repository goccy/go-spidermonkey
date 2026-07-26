package nodejs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// waitNoHang runs rt.Wait with a short timeout and fails the test immediately if
// the loop does not complete — a hung worker (no auto-exit) would otherwise
// block Wait forever.
func waitNoHang(t *testing.T, rt interface {
	Wait(context.Context) error
}, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait did not complete (worker hang / loop leak?): %v", err)
	}
}

// TestWorkerAutoexitNoListener: a worker that posts a message and finishes with
// NO 'message' listener must auto-exit with code 0 (Node parity). Before the
// fix this hung: the agent parked on its inbox forever and rt.Wait never
// returned.
func TestWorkerAutoexitNoListener(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("const { parentPort } = require('worker_threads'); parentPort.postMessage('hi')", { eval: true });
		w.on("message", (m) => { r.msg = m; });
		w.on("error", (e) => { r.err = String(e); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	if got := evalStr(t, js, `String(r.err ?? '')`); got != "" {
		t.Fatalf("unexpected worker error: %s", got)
	}
	if got := evalStr(t, js, `String(r.msg ?? '')`); got != "hi" {
		t.Errorf("message = %q, want hi", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "0" {
		t.Errorf("auto-exit code = %q, want 0", got)
	}
}

// TestWorkerNoAutoexitWithListener: a worker that registers
// parentPort.on('message') must NOT auto-exit — it stays alive, processes a
// message, and only exits when it calls process.exit (driven by main here).
func TestWorkerNoAutoexitWithListener(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = { got: [] };
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.postMessage("ready");
			parentPort.on("message", (m) => {
				if (m === "quit") { process.exit(3); return; }
				parentPort.postMessage("echo:" + m);
			});
		`+"`"+`, { eval: true });
		w.on("message", (m) => {
			r.got.push(m);
			if (m === "ready") { w.postMessage("x"); }
			if (m === "echo:x") { w.postMessage("quit"); }
		});
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	// If the worker had wrongly auto-exited, it would exit 0 before echoing.
	if got := evalStr(t, js, `r.got.join(",")`); got != "ready,echo:x" {
		t.Errorf("messages = %q, want ready,echo:x (worker auto-exited despite listener?)", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "3" {
		t.Errorf("exit code = %q, want 3 (process.exit path)", got)
	}
}

// TestWorkerAutoexitAfterTimer: a worker that schedules a timer then finishes
// its top-level script stays alive until the timer fires, then auto-exits 0.
func TestWorkerAutoexitAfterTimer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			setTimeout(() => { parentPort.postMessage("late"); }, 10);
		`+"`"+`, { eval: true });
		w.on("message", (m) => { r.msg = m; });
		w.on("error", (e) => { r.err = String(e); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	if got := evalStr(t, js, `String(r.err ?? '')`); got != "" {
		t.Fatalf("unexpected worker error: %s", got)
	}
	if got := evalStr(t, js, `String(r.msg ?? '')`); got != "late" {
		t.Errorf("message = %q, want late (timer callback must run before exit)", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "0" {
		t.Errorf("auto-exit code = %q, want 0 after timer drains", got)
	}
}

// TestWorkerProcessExitCodePreserved: an explicit process.exit(7) still yields
// exit code 7 (the idle auto-exit must not clobber it).
func TestWorkerProcessExitCodePreserved(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("const { parentPort } = require('worker_threads'); parentPort.postMessage('bye'); process.exit(7);", { eval: true });
		w.on("message", (m) => { r.msg = m; });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	if got := evalStr(t, js, `String(r.msg ?? '')`); got != "bye" {
		t.Errorf("message = %q, want bye", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "7" {
		t.Errorf("exit code = %q, want 7", got)
	}
}

// TestWorkerAutoexitThrowStillErrors: a throwing worker still emits 'error'
// first and then exits with code 1 — the idle hook after the try/catch is a
// no-op once __wt_reportError has already exited.
func TestWorkerAutoexitThrowStillErrors(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("throw new Error('kaboom')", { eval: true });
		w.on("error", (e) => { r.errMsg = String(e && e.message || e); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	if got := evalStr(t, js, `r.errMsg ?? ''`); !strings.Contains(got, "kaboom") {
		t.Errorf("worker 'error' missing/incorrect: %q", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "1" {
		t.Errorf("exit code = %q, want 1 after uncaught throw", got)
	}
}

// TestWorkerNoAutoexitListenerAfterAwait: a worker that installs its
// parentPort 'message' listener only after several microtask hops (an
// async/await init) must NOT prematurely auto-exit. The idle check must run as
// a true macrotask (after the microtask queue drains) so the listener is
// visible; a microtask-timed check fires mid-chain and drops the message.
func TestWorkerNoAutoexitListenerAfterAwait(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const src = `+"`"+`
			const { parentPort } = require('worker_threads');
			(async () => {
				await Promise.resolve();
				await Promise.resolve();
				await Promise.resolve();
				parentPort.on('message', (m) => {
					parentPort.postMessage('echo:' + m);
					process.exit(0);
				});
				parentPort.postMessage('ready');
			})();
		`+"`"+`;
		const w = new Worker(src, { eval: true });
		w.on("message", (m) => {
			if (m === "ready") w.postMessage("work");
			else r.echo = m;
		});
		w.on("error", (e) => { r.err = String(e); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	waitNoHang(t, rt, 20*time.Second)
	if got := evalStr(t, js, `String(r.err ?? '')`); got != "" {
		t.Fatalf("unexpected worker error: %s", got)
	}
	// The worker must have stayed alive to receive "work" and echo it — proving
	// it did not prematurely exit before installing the async listener.
	if got := evalStr(t, js, `String(r.echo ?? '')`); got != "echo:work" {
		t.Errorf("echo = %q, want echo:work (worker exited before installing its listener?)", got)
	}
	if got := evalStr(t, js, `String(r.code)`); got != "0" {
		t.Errorf("exit code = %q, want 0", got)
	}
}
