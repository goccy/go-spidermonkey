package nodejs_test

import (
	"context"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"strings"
	"testing"
	"time"
)

func TestWorkerThreadsEchoAndWorkerData(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// A worker that greets using workerData, then echoes messages back
	// upper-cased. Real parallel agent.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = { messages: [] };
		const w = new Worker(`+"`"+`
			const { parentPort, workerData } = require("worker_threads");
			parentPort.postMessage("ready:" + workerData.name);
			parentPort.on("message", (msg) => {
				if (msg === "stop") { process.exit(0); return; }
				parentPort.postMessage(String(msg).toUpperCase());
			});
		`+"`"+`, { eval: true, workerData: { name: "alice" } });
		w.on("online", () => { r.online = true; });
		w.on("message", (m) => {
			r.messages.push(m);
			if (m === "ready:alice") { w.postMessage("hello"); w.postMessage("world"); }
			if (m === "WORLD") { w.postMessage("stop"); }
		});
		w.on("exit", (code) => { r.exitCode = code; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if !evalVal(t, js, `r.online`).Bool() {
		t.Error("online event did not fire")
	}
	if got := evalStr(t, js, `r.messages.join(",")`); got != "ready:alice,HELLO,WORLD" {
		t.Errorf("messages = %q, want ready:alice,HELLO,WORLD", got)
	}
	if got := evalStr(t, js, `String(r.exitCode)`); got != "0" {
		t.Errorf("exit code = %q", got)
	}
}

// TestWorkerTimerHandleHasUnref verifies a worker realm's setTimeout returns a
// Timeout-like handle: `setTimeout(...).unref()` must not throw (it did when the
// worker returned a bare numeric id).
func TestWorkerTimerHandleHasUnref(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			let ok = false;
			try {
				const t = setTimeout(() => {}, 60000);
				t.unref(); t.ref();
				clearTimeout(t);
				ok = true;
			} catch (e) { ok = "threw: " + e.message; }
			parentPort.postMessage(ok);
			process.exit(0);
		`+"`"+`, { eval: true });
		w.on("message", (m) => { r.ok = m; });
		w.on("error", (e) => { r.err = String(e); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.ok)`); got != "true" {
		t.Errorf("worker setTimeout().unref() = %q, want true", got)
	}
}

func TestWorkerThreadsParallelCompute(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// Fan out to 4 workers each summing a range; verify the total. This runs
	// on 4 real goroutines/realms in parallel.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = { partials: [], total: 0, done: 0 };
		const N = 4;
		const perWorker = 250000;
		for (let i = 0; i < N; i++) {
			const w = new Worker(`+"`"+`
				const { parentPort, workerData } = require("worker_threads");
				let sum = 0;
				for (let n = workerData.start; n < workerData.end; n++) sum += n;
				parentPort.postMessage(sum);
				process.exit(0);
			`+"`"+`, { eval: true, workerData: { start: i * perWorker, end: (i + 1) * perWorker } });
			w.on("message", (partial) => {
				r.partials.push(partial);
				r.total += partial;
				r.done++;
			});
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := evalStr(t, js, `String(r.done)`); got != "4" {
		t.Errorf("completed workers = %q, want 4", got)
	}
	// Sum of 0..999999 = 999999*1000000/2 = 499999500000.
	if got := evalStr(t, js, `String(r.total)`); got != "499999500000" {
		t.Errorf("parallel sum = %q, want 499999500000", got)
	}
}

func TestWorkerThreadsSharedArrayBuffer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// The main thread and worker share an Int32Array over a SharedArrayBuffer;
	// the worker atomically increments a counter the main thread then reads.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const sab = new SharedArrayBuffer(8);
		const view = new Int32Array(sab);
		const w = new Worker(`+"`"+`
			const { parentPort, workerData } = require("worker_threads");
			const view = new Int32Array(workerData.sab);
			for (let i = 0; i < 1000; i++) Atomics.add(view, 0, 1);
			Atomics.store(view, 1, 42);
			parentPort.postMessage("done");
			process.exit(0);
		`+"`"+`, { eval: true, workerData: { sab } });
		w.on("message", () => {
			r.counter = Atomics.load(view, 0);
			r.flag = Atomics.load(view, 1);
		});
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := evalStr(t, js, `String(r.counter)`); got != "1000" {
		t.Errorf("shared counter = %q, want 1000 (SharedArrayBuffer not shared?)", got)
	}
	if got := evalStr(t, js, `String(r.flag)`); got != "42" {
		t.Errorf("shared flag = %q, want 42", got)
	}
}

func TestWorkerThreadsTerminate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.postMessage("started");
			parentPort.on("message", () => {}); // keep alive
		`+"`"+`, { eval: true });
		w.on("message", (m) => { if (m === "started") { r.started = true; w.terminate(); } });
		w.on("exit", (code) => { r.exited = true; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !evalVal(t, js, `r.started`).Bool() {
		t.Error("worker did not start")
	}
	if !evalVal(t, js, `r.exited`).Bool() {
		t.Error("terminate did not fire exit")
	}
}

// terminate() has to stop a worker that is not listening. A worker spinning in
// a synchronous loop never returns to its job queue, so it never reads the
// cooperative sentinel terminate also sends; only the engine-level interrupt
// reaches it. Before that existed, this worker ran until the process died.
func TestWorkerThreadsTerminateSynchronousInfiniteLoop(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.postMessage("spinning");
			while (true) {}
		`+"`"+`, { eval: true });
		w.on("message", (m) => { if (m === "spinning") { r.spinning = true; w.terminate(); } });
		w.on("exit", () => { r.exited = true; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !evalVal(t, js, `r.spinning`).Bool() {
		t.Fatal("worker never reported that it was spinning")
	}
	if !evalVal(t, js, `r.exited`).Bool() {
		t.Error("terminate did not stop a synchronously spinning worker")
	}
}

func TestWorkerTopLevelThrowEmitsError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("throw new Error('boom in worker')", { eval: true });
		w.on("error", (e) => { r.errMsg = String(e && e.message || e); });
		w.on("exit", (code) => { r.exitCode = code; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.errMsg ?? ""`); !strings.Contains(got, "boom in worker") {
		t.Errorf("worker 'error' event missing/incorrect: %q", got)
	}
	if got := evalStr(t, js, `String(r.exitCode)`); got != "1" {
		t.Errorf("worker exit code = %q, want 1 after an uncaught throw", got)
	}
}

// The worker source wrapper must preserve a top-level "use strict" directive:
// an assignment to an undeclared variable must throw (surfacing as 'error').
func TestWorkerPreservesUseStrict(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker('"use strict"; undeclaredGlobalX = 1;', { eval: true });
		w.on("error", (e) => { r.errName = String(e && e.name || ""); r.errMsg = String(e && e.message || ""); });
		w.on("exit", (c) => { r.code = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.errMsg ?? ""`); !strings.Contains(got, "undeclaredGlobalX") {
		t.Fatalf("strict-mode assignment did not throw in worker (use strict not preserved): errMsg=%q", got)
	}
}

// TestWorkerSetImmediate verifies setImmediate/setTimeout(fn,0) work inside a
// worker (the Atomics.waitAsync 0ms path returns a string, not a thenable).
func TestWorkerSetImmediate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			let order = [];
			setImmediate(() => {
				order.push("immediate");
				setTimeout(() => { order.push("timeout0"); parentPort.postMessage(order.join(",")); process.exit(0); }, 0);
			});
		`+"`"+`, { eval: true });
		w.on("message", (m) => { r.order = m; });
		w.on("error", (e) => { r.err = String(e); });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, "String(r.err ?? '')"); got != "" {
		t.Fatalf("worker error: %s", got)
	}
	if got := evalStr(t, js, "String(r.order ?? '')"); got != "immediate,timeout0" {
		t.Errorf("worker timer order = %q, want immediate,timeout0", got)
	}
}

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

// How many live workers an instance can hold, and that teardown stays prompt
// with all of them alive.
//
// Every guest thread's stack is allocated out of the instance's linear memory,
// so the worker ceiling is a memory budget: Config.MaxMemoryBytes divided by
// what a worker actually uses. Two things used to make that budget far smaller
// than it looks — each thread took the LINKED main-stack size (8 MiB) rather
// than a size chosen for it, which also starved the engine's own helper
// threads at teardown-GC time and hung the shutdown outright.

// spawnLiveWorkers starts n workers that come online and then idle forever
// holding a 'message' listener, and reports how many reported in.
func spawnLiveWorkers(t *testing.T, js *spidermonkey.JS, rt interface {
	Wait(context.Context) error
}, n int) {
	t.Helper()
	r, err := js.Eval(context.Background(), fmt.Sprintf(`
		const { Worker } = require("worker_threads");
		globalThis.r = { online: 0 };
		for (let i = 0; i < %d; i++) {
			const w = new Worker(`+"`"+`
				const { parentPort } = require("worker_threads");
				parentPort.postMessage("up");
				parentPort.on("message", (m) => parentPort.postMessage(m));
			`+"`"+`, { eval: true });
			w.unref();
			w.on("message", () => { r.online++; });
		}
	`, n))
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("spawning %d workers threw: %v", n, r.Error)
	}
	// The workers never exit, so the loop only ends on the deadline; a few
	// seconds is ample for them all to report in.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	if got := evalStr(t, js, `String(r.online)`); got != fmt.Sprint(n) {
		t.Fatalf("%s of %d workers came online", got, n)
	}
}

// The regression: teardown with many live workers used to hang forever once
// the count reached the point where thread stacks exhausted linear memory —
// deterministically, from 16 benign idle workers, wedging the instance for the
// process lifetime.
func TestTeardownWithManyLiveWorkers(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	spawnLiveWorkers(t, js, rt, 20)

	done := make(chan error, 1)
	go func() { rt.Close(); done <- js.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("teardown hung with 20 live workers")
	}
}

// The ceiling is a memory budget and nothing else, so the documented knob
// moves it: a count that does not fit the default cap fits a larger one.
func TestWorkerCeilingScalesWithMemoryBudget(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer func() { rt.Close(); js.Close() }()
	spawnLiveWorkers(t, js, rt, 96)
}

// A postMessage transfer list must detach the transferred ArrayBuffer on the
// sender side (its byteLength becomes 0) while the receiver still gets the
// bytes. Covers Worker.postMessage(value, [ab]).
func TestWorkerPostMessageTransferDetachesSender(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.on("message", (msg) => {
				// msg.buf is an ArrayBuffer the main thread transferred; read it back.
				const view = new Uint8Array(msg.buf);
				parentPort.postMessage("recv:" + view.length + ":" + view[0] + "," + view[1] + "," + view[2]);
				process.exit(0);
			});
		`+"`"+`, { eval: true });
		const ab = new Uint8Array([10, 20, 30]).buffer;
		w.on("online", () => {
			w.postMessage({ buf: ab }, [ab]);
			// After transfer the sender's ArrayBuffer is detached: byteLength 0.
			r.senderByteLength = ab.byteLength;
		});
		w.on("message", (m) => { r.workerSaw = m; });
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.senderByteLength)`); got != "0" {
		t.Errorf("sender ArrayBuffer.byteLength after transfer = %q, want 0 (not detached)", got)
	}
	if got := evalStr(t, js, `String(r.workerSaw)`); got != "recv:3:10,20,30" {
		t.Errorf("worker did not receive the transferred bytes: %q", got)
	}
}

// The same, in the worker->main direction: parentPort.postMessage(value, [ab])
// detaches on the worker side and the main thread receives the bytes.
func TestParentPortPostMessageTransferDetachesSender(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			const ab = new Uint8Array([1, 2, 3, 4]).buffer;
			parentPort.postMessage({ buf: ab, before: ab.byteLength }, [ab]);
			// Report the sender-side byteLength AFTER transfer via a second message.
			parentPort.postMessage({ after: ab.byteLength });
			process.exit(0);
		`+"`"+`, { eval: true });
		r.msgs = [];
		w.on("message", (m) => { r.msgs.push(m); });
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(new Uint8Array(r.msgs[0].buf).join(","))`); got != "1,2,3,4" {
		t.Errorf("main did not receive transferred bytes: %q", got)
	}
	if got := evalStr(t, js, `String(r.msgs[0].before)`); got != "4" {
		t.Errorf("worker byteLength before transfer = %q, want 4", got)
	}
	if got := evalStr(t, js, `String(r.msgs[1].after)`); got != "0" {
		t.Errorf("worker byteLength after transfer = %q, want 0 (not detached)", got)
	}
}

// A worker that throws must surface the ORIGINAL error message/name/stack on the
// main thread's 'error' event — not a flattened "Name: message" string.
func TestWorkerErrorPreservesMessageAndStack(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("throw new TypeError('boom')", { eval: true });
		w.on("error", (e) => {
			r.isError = e instanceof Error;
			r.message = e.message;
			r.name = e.name;
			r.hasStack = typeof e.stack === "string" && e.stack.length > 0;
		});
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.isError)`); got != "true" {
		t.Errorf("worker error is not an Error instance")
	}
	if got := evalStr(t, js, `r.message`); got != "boom" {
		t.Errorf("err.message = %q, want exactly \"boom\"", got)
	}
	if got := evalStr(t, js, `r.name`); got != "TypeError" {
		t.Errorf("err.name = %q, want TypeError", got)
	}
	if got := evalStr(t, js, `String(r.hasStack)`); got != "true" {
		t.Errorf("err.stack was not preserved")
	}
}

// A same-thread MessageChannel: port1.postMessage(x) delivers a 'message' on
// port2 and vice versa, usable both EventEmitter-style (on('message') gets the
// data) and DOM-style (onmessage / addEventListener get a { data } event). The
// payload is structured-cloned. Covers the previously-throwing stub.
func TestMessageChannelSameThreadBothStyles(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// runScript drains the full event loop (timers + microtasks), so the
	// queueMicrotask-based deliveries all complete before it returns.
	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { onData: [], onmessageData: null };
		const { port1, port2 } = new MessageChannel();

		// EventEmitter style on port2: receives the raw data.
		port2.on("message", (data) => { r.onData.push(data); });
		// DOM style onmessage on port1: receives a { data } MessageEvent.
		port1.onmessage = (ev) => { r.onmessageData = ev.data; };

		port1.postMessage("to-port2-a");
		port1.postMessage({ n: 42 });
		port2.postMessage("to-port1");
	`)

	if got := evalStr(t, js, `r.onData.length + ":" + typeof r.onData[1]`); got != "2:object" {
		t.Errorf("port2 on('message') deliveries = %q, want 2:object", got)
	}
	if got := evalStr(t, js, `String(r.onData[0])`); got != "to-port2-a" {
		t.Errorf("port2 first message = %q", got)
	}
	if got := evalStr(t, js, `String(r.onData[1].n)`); got != "42" {
		t.Errorf("structured-cloned object not received on port2: %q", got)
	}
	if got := evalStr(t, js, `String(r.onmessageData)`); got != "to-port1" {
		t.Errorf("port1.onmessage data = %q, want to-port1", got)
	}
}

// The payload must be a structured CLONE: mutating the original after sending
// must not change what the receiver got.
func TestMessageChannelStructuredClone(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = {};
		const { port1, port2 } = new MessageChannel();
		const payload = { arr: [1, 2, 3] };
		port2.on("message", (data) => { r.received = data; });
		port1.postMessage(payload);
		payload.arr.push(999); // mutate AFTER post; receiver must not see it
	`)

	if got := evalStr(t, js, `r.received.arr.join(",")`); got != "1,2,3" {
		t.Errorf("payload was not cloned (receiver saw a mutation): %q", got)
	}
}

// addEventListener('message', ...) starts the port and delivers { data } events;
// a message posted before any listener is attached is buffered until the port
// starts, not dropped.
func TestMessagePortBuffersUntilStarted(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { events: [] };
		const { port1, port2 } = new MessageChannel();
		// Post before port2 has any listener: must be buffered.
		port1.postMessage("early");
		// Attach a listener a turn later, then post again.
		queueMicrotask(() => {
			port2.addEventListener("message", (ev) => { r.events.push(ev.data); });
			port1.postMessage("late");
		});
	`)

	if got := evalStr(t, js, `r.events.join(",")`); got != "early,late" {
		t.Errorf("buffered+live delivery = %q, want early,late", got)
	}
}

// close() closes both ends and emits 'close' on each; a post after close is a
// no-op (does not throw).
func TestMessageChannelClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { closed: [], threw: false };
		const { port1, port2 } = new MessageChannel();
		port1.on("close", () => r.closed.push("port1"));
		port2.on("close", () => r.closed.push("port2"));
		port1.close();
		try { port2.postMessage("ignored"); } catch (e) { r.threw = true; }
	`)

	if got := evalStr(t, js, `r.closed.slice().sort().join(",")`); got != "port1,port2" {
		t.Errorf("close did not fire on both ports: %q", got)
	}
	if got := evalStr(t, js, `String(r.threw)`); got != "false" {
		t.Errorf("postMessage after close threw")
	}
}

// The MessageChannel/MessagePort are exported and constructible (the previous
// stub threw "not supported").
func TestMessageChannelExportsConstructible(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const wt = require("worker_threads");
		globalThis.r = {};
		r.hasChannel = typeof wt.MessageChannel === "function";
		r.hasPort = typeof wt.MessagePort === "function";
		const ch = new wt.MessageChannel();
		r.portsAreObjects = (ch.port1 instanceof wt.MessagePort) && (ch.port2 instanceof wt.MessagePort);
	`)
	for expr, want := range map[string]string{
		"String(r.hasChannel)":      "true",
		"String(r.hasPort)":         "true",
		"String(r.portsAreObjects)": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
