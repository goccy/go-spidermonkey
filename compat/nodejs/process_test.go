package nodejs_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
