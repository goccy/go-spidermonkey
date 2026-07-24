package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// push() after push(null) is a producer bug; the stream must error rather than
// deliver a chunk after 'end'.
func TestReadablePushAfterEOFErrors(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.on("data", () => {});
		rs.on("error", (e) => { __r.code = e.code; });
		rs.push("a");
		rs.push(null);
		rs.push("late"); // after EOF
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.code ?? ""`); got != "ERR_STREAM_PUSH_AFTER_EOF" {
		t.Fatalf("push-after-EOF code = %q, want ERR_STREAM_PUSH_AFTER_EOF", got)
	}
}

// write() after end() must not throw synchronously; the error arrives on a
// later tick so a listener still catches it.
func TestWritableWriteAfterEndAsyncError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = { threw: false };
		const { Writable } = require("stream");
		const ws = new Writable({ write(c, e, cb) { cb(); } });
		ws.on("error", (e) => { __r.code = e.code; });
		ws.end();
		try { ws.write("x"); } catch (e) { __r.threw = true; }
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if evalStr(t, js, `String(__r.threw)`) == "true" {
		t.Fatalf("write() after end() threw synchronously; must emit async")
	}
	if got := evalStr(t, js, `__r.code ?? ""`); got != "ERR_STREAM_WRITE_AFTER_END" {
		t.Fatalf("write-after-end code = %q, want ERR_STREAM_WRITE_AFTER_END", got)
	}
}

// Destroying a Transform stops its readable side too: no 'data'/'end' after
// 'close'.
func TestTransformDestroyStopsReadable(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		const { Transform } = require("stream");
		const ts = new Transform({ transform(c, e, cb) { setTimeout(() => cb(null, c), 5); } });
		ts.on("data", () => { __order.push("data"); });
		ts.on("end", () => { __order.push("end"); });
		ts.on("close", () => { __order.push("close"); });
		ts.write("a");
		ts.destroy();
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	// close must appear and no data/end after it.
	order := evalStr(t, js, `__order.join(",")`)
	if order != "close" {
		t.Fatalf("events = %q, want just close (no data/end after destroy)", order)
	}
}

// A throw in one nextTick callback must not drop the ticks queued after it.
func TestNextTickExceptionIsolation(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	// The propagated error is expected; we only care that "good" still ran.
	rt.RunScript(context.Background(), `
		globalThis.__ran = [];
		process.nextTick(() => { __ran.push("bad"); throw new Error("boom"); });
		process.nextTick(() => { __ran.push("good"); });
	`)
	if got := evalStr(t, js, `(__ran||[]).join(",")`); got != "bad,good" {
		t.Fatalf("ticks ran = %q, want bad,good (a throw must not drop later ticks)", got)
	}
}

// An error thrown in a process.nextTick callback is delivered to a
// process.on('uncaughtException') handler, and later ticks still run.
func TestNextTickUncaughtExceptionHandler(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = { ran: [], caught: "" };
		process.on("uncaughtException", (e) => { __r.caught = e.message; });
		process.nextTick(() => { __r.ran.push("bad"); throw new Error("kaboom"); });
		process.nextTick(() => { __r.ran.push("good"); });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.caught`); got != "kaboom" {
		t.Fatalf("uncaughtException handler caught %q, want kaboom", got)
	}
	if got := evalStr(t, js, `__r.ran.join(",")`); got != "bad,good" {
		t.Fatalf("ticks ran = %q, want bad,good", got)
	}
}
