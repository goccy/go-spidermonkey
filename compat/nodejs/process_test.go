package nodejs_test

import (
	"bytes"
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestProcessExitCleanTermination verifies process.exit() is reported as a clean
// termination (no evaluation error) with the exit code recorded, and that it
// cannot be swallowed by an uncaughtException handler.
func TestProcessExitCleanTermination(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `console.log("bye"); process.exit(3);`)
	if err != nil {
		t.Fatalf("RunScript returned error for a clean exit: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("process.exit surfaced as an evaluation error: %v", r.Error)
	}
	if !rt.Exited() || rt.ExitCode() != 3 {
		t.Fatalf("Exited=%v ExitCode=%d, want true/3", rt.Exited(), rt.ExitCode())
	}
}

func TestProcessExitNotSwallowedByHandler(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `
		process.on("uncaughtException", () => { globalThis.__swallowed = true; });
		process.nextTick(() => process.exit(0));
	`)
	if err != nil {
		t.Fatalf("RunScript error: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("unexpected eval error: %v", r.Error)
	}
	if !rt.Exited() {
		t.Fatal("process.exit in nextTick was swallowed by the uncaughtException handler")
	}
}

// TestReusedRuntimeExitDoesNotMaskError verifies a process.exit() in one run does
// not make a later run's genuine error read as a clean exit on a reused Runtime.
func TestReusedRuntimeExitDoesNotMaskError(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `process.exit(0);`); err != nil {
		t.Fatalf("first run: %v", err)
	}
	r, err := rt.RunScript(context.Background(), `throw new Error("real failure");`)
	if err != nil {
		t.Fatalf("second run returned a Go error: %v", err)
	}
	if r.Error == nil {
		t.Fatal("second run's genuine throw was masked as a clean exit by the prior process.exit")
	}
	if rt.Exited() {
		t.Error("Exited() still true after a run that did not call process.exit")
	}
}

// TestProcessExitAndBeforeExitEvents verifies process 'beforeExit' fires when the
// loop drains and 'exit' fires on termination.
func TestProcessExitAndBeforeExitEvents(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { before: 0, exit: 0 };
		process.on("beforeExit", () => { r.before++; });
		process.on("exit", (code) => { r.exit++; r.code = code; });
	`)
	if got := evalVal(t, js, "r.before").Int(); got < 1 {
		t.Errorf("beforeExit fired %d times, want >= 1", got)
	}
	if got := evalVal(t, js, "r.exit").Int(); got != 1 {
		t.Errorf("exit fired %d times, want exactly 1", got)
	}
}

// TestStdoutIsWritable verifies process.stdout is a Writable stream: pipe works
// and write(data, cb) invokes the callback.
func TestStdoutIsWritable(t *testing.T) {
	var out strings.Builder
	js, rt := newRuntime(t, spidermonkey.Config{Stdout: &out})
	runScript(t, rt, `
		globalThis.r = {};
		r.hasOn = typeof process.stdout.on === "function";
		r.hasOnce = typeof process.stdout.once === "function";
		process.stdout.write("hello ", () => { r.cbFired = true; });
		const { Readable } = require("stream");
		Readable.from(["piped"]).pipe(process.stdout);
	`)
	if !evalVal(t, js, "r.hasOn").Bool() || !evalVal(t, js, "r.hasOnce").Bool() {
		t.Error("process.stdout is not a stream (missing on/once)")
	}
	if !evalVal(t, js, "r.cbFired").Bool() {
		t.Error("process.stdout.write callback not fired")
	}
	if got := out.String(); !strings.Contains(got, "hello ") || !strings.Contains(got, "piped") {
		t.Errorf("stdout = %q, want it to contain 'hello ' and 'piped'", got)
	}
}

// TestStdoutBinaryWrite verifies process.stdout.write(Buffer) emits the exact
// bytes, not a UTF-8-corrupted round-trip.
func TestStdoutBinaryWrite(t *testing.T) {
	var out bytes.Buffer
	js, rt := newRuntime(t, spidermonkey.Config{Stdout: &out})
	runScript(t, rt, `process.stdout.write(Buffer.from([0xff, 0xfe, 0xfd, 0x00, 0x41]));`)
	_ = js
	got := out.Bytes()
	want := []byte{0xff, 0xfe, 0xfd, 0x00, 0x41}
	if !bytes.Equal(got, want) {
		t.Errorf("stdout bytes = %v, want %v (binary corrupted)", got, want)
	}
}

// process.chdir() moves the working directory, and a RELATIVE path resolves
// against it — the two halves have to arrive together. chdir used to throw
// outright; making it succeed while every relative read still resolved against
// the filesystem root would have been worse than refusing it, so fs paths go
// through one resolver.
func TestProcessChdirAndRelativePaths(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{
		"app/sub/file.txt": {Data: []byte("contents")},
		"app/other.txt":    {Data: []byte("other")},
	}})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = [];
		r.push("start:" + process.cwd());
		process.chdir("/app");
		r.push("abs:" + process.cwd());
		r.push("read-rel:" + fs.readFileSync("other.txt", "utf8"));
		process.chdir("sub");
		r.push("rel:" + process.cwd());
		r.push("read-after:" + fs.readFileSync("file.txt", "utf8"));
		r.push("read-abs:" + fs.readFileSync("/app/other.txt", "utf8"));
		try { process.chdir("/nope"); r.push("missing:NO-THROW"); }
		catch (e) { r.push("missing:" + e.code); }
		try { process.chdir("/app/other.txt"); r.push("file:NO-THROW"); }
		catch (e) { r.push("file:" + e.code); }
	`)
	want := "start:/ | abs:/app | read-rel:other | rel:/app/sub | read-after:contents | " +
		"read-abs:other | missing:ENOENT | file:ENOTDIR"
	if got := evalStr(t, js, `r.join(" | ")`); got != want {
		t.Errorf("chdir behaviour =\n %s\nwant\n %s", got, want)
	}
}

// process.memoryUsage()/cpuUsage()/resourceUsage() used to be all-zero stubs.
// They must now report real numbers from the Go host: heapUsed/heapTotal must
// be real and non-zero so leak-guards and ratios work.
func TestProcessMemoryUsageReal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const m = process.memoryUsage();
		r.shape = ["rss","heapTotal","heapUsed","external","arrayBuffers"].every(k => typeof m[k] === "number");
		r.heapUsed = m.heapUsed > 0;
		r.heapTotal = m.heapTotal > 0;
		r.rss = m.rss > 0;
		r.ratio = Number.isFinite(m.heapUsed / m.heapTotal);
		r.rssFn = typeof process.memoryUsage.rss === "function" && process.memoryUsage.rss() > 0;
	`)
	for _, expr := range []string{"r.shape", "r.heapUsed", "r.heapTotal", "r.rss", "r.ratio", "r.rssFn"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}

// cpuUsage() returns cumulative microseconds; cpuUsage(prev) returns the delta.
func TestProcessCPUUsageDelta(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const a = process.cpuUsage();
		r.shape = typeof a.user === "number" && typeof a.system === "number";
		r.nonzero = a.user > 0 || a.system > 0;
		// A delta against a prior reading must be >= 0 (monotonic).
		const d = process.cpuUsage(a);
		r.deltaNonNeg = d.user >= 0 && d.system >= 0;
	`)
	for _, expr := range []string{"r.shape", "r.nonzero", "r.deltaNonNeg"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}

// resourceUsage() returns the ~16-field shape with real CPU/maxRSS fields.
func TestProcessResourceUsageShape(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const u = process.resourceUsage();
		const fields = ["userCPUTime","systemCPUTime","maxRSS","sharedMemorySize",
			"unsharedDataSize","unsharedStackSize","minorPageFault","majorPageFault",
			"swappedOut","fsRead","fsWrite","ipcSent","ipcReceived","signalsCount",
			"voluntaryContextSwitches","involuntaryContextSwitches"];
		r.allPresent = fields.every(k => typeof u[k] === "number");
		r.fieldCount = Object.keys(u).length;
		r.maxRSS = u.maxRSS > 0;
	`)
	for _, expr := range []string{"r.allPresent", "r.maxRSS"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
	if got := evalStr(t, js, `r.fieldCount`); got != "16" {
		t.Errorf("resourceUsage field count = %s, want 16", got)
	}
}

// Node's test suite judges most tests through assertions registered on 'exit'
// (common.mustCall). If a throw from an 'exit' listener were swallowed, every
// such test would report a false PASS — so the whole Node conformance baseline
// rests on this.
func TestExitListenerThrowIsReported(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{FS: fstest.MapFS{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	r, err := rt.RunScript(context.Background(), `
		let called = false;
		process.on("exit", () => {
			if (!called) throw new Error("mustCall: function was not called");
		});
	`)
	got := ""
	if err != nil {
		got = err.Error()
	} else if r.Error != nil {
		got = r.Error.Error()
	}
	if !strings.Contains(got, "mustCall") {
		t.Fatalf("a throwing 'exit' listener was not reported (got %q); every "+
			"mustCall-based Node test would falsely pass", got)
	}
}

// A 'beforeExit' listener that throws must surface as a JavaScript error from
// RunScript. It used to crash the HOST: Wait consulted the completion value of
// the eval that fires the event without checking whether that eval threw, and a
// thrown script has no completion value, so the read was a nil-interface
// dereference — a guest exception taking the process down with it.
func TestBeforeExitListenerThrows(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{FS: fstest.MapFS{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	r, err := rt.RunScript(context.Background(), `
		process.on("beforeExit", () => { throw new Error("boom"); });
	`)
	if r.Error == nil && err == nil {
		t.Fatal("a throwing beforeExit listener reported success")
	}
	got := ""
	if err != nil {
		got = err.Error()
	} else {
		got = r.Error.Error()
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("error %q does not mention the thrown message", got)
	}
}

// process.on('unhandledRejection') — a rejection that reaches a microtask
// checkpoint with nothing to handle it.
//
// These are visible only to the engine: an async function's promise is created
// by the engine, so no host- or guest-side Promise wrapper ever sees it. The
// engine reports them to the embedder, and the event loop delivers them at each
// checkpoint.

func TestUnhandledRejectionFires(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => { r.reasons.push(String(reason)); });
		Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.reasons.join(",")`); got != "Error: boom" {
		t.Fatalf("unhandledRejection reasons = %q, want %q", got, "Error: boom")
	}
}

// The case no Promise wrapper can see: the rejected promise is the one the
// engine creates for the async function itself.
func TestUnhandledRejectionFromAsyncFunction(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => { r.reasons.push(String(reason)); });
		(async () => { throw new Error("async-boom"); })();
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.reasons.join(",")`); got != "Error: async-boom" {
		t.Fatalf("unhandledRejection reasons = %q, want %q", got, "Error: async-boom")
	}
}

// A rejection the guest handles within the same microtask checkpoint is NOT
// unhandled. This is the assertion that says the report waits for the
// checkpoint instead of firing the moment a promise rejects — including for a
// handler attached in a later MICROtask, which still lands inside the same
// checkpoint.
func TestRejectionHandledInSameCheckpointDoesNotFire(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { count: 0 };
		process.on("unhandledRejection", () => { r.count++; });
		Promise.reject(new Error("caught")).catch(() => {});
		const chained = Promise.reject(new Error("chained"));
		Promise.resolve().then(() => chained.catch(() => {}));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "0" {
		t.Fatalf("unhandledRejection fired %s times for handled rejections, want 0", got)
	}
}

// A handler attached in a later MACROtask is too late — the checkpoint has
// already passed, and the rejection was unhandled when it was reached. This is
// Node's behaviour, and it is what makes the event meaningful at all.
func TestRejectionHandledInALaterTaskStillFires(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { count: 0 };
		process.on("unhandledRejection", () => { r.count++; });
		const late = Promise.reject(new Error("late"));
		setTimeout(() => late.catch(() => {}), 0);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "1" {
		t.Fatalf("unhandledRejection fired %s times, want 1", got)
	}
}

// The rejected promise itself is the handler's second argument, and it is the
// same object the guest holds — not a copy.
func TestUnhandledRejectionCarriesThePromise(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.on("unhandledRejection", (reason, promise) => { r.same = promise === globalThis.p; });
		globalThis.p = Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.same)`); got != "true" {
		t.Fatalf("handler received promise identity = %s, want true", got)
	}
}

// With no 'unhandledRejection' listener, Node routes the rejection to
// 'uncaughtException' with origin 'unhandledRejection'.
func TestUnhandledRejectionFallsBackToUncaughtException(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.on("uncaughtException", (err, origin) => { r.origin = origin; r.code = err.code; });
		Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.origin)`); got != "unhandledRejection" {
		t.Fatalf("uncaughtException origin = %q, want %q", got, "unhandledRejection")
	}
}

// A rejection raised from inside an unhandledRejection handler is itself
// reported: the checkpoint repeats until a pass finds nothing.
func TestUnhandledRejectionFromWithinHandler(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => {
			r.reasons.push(String(reason));
			if (r.reasons.length === 1) Promise.reject("second");
		});
		Promise.reject("first");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := evalStr(t, js, `r.reasons.join(",")`)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("reasons = %q, want both the original and the handler's rejection", got)
	}
}
