package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

// TestBufferSwapRejectsBadLength verifies swap16/32/64 throw on a non-multiple
// length rather than silently corrupting.
func TestBufferSwapRejectsBadLength(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch { r[name] = "threw"; } };
		chk("s16bad", () => Buffer.from([1,2,3]).swap16());
		chk("s32bad", () => Buffer.from([1,2,3,4,5]).swap32());
		chk("s16ok", () => Buffer.from([1,2,3,4]).swap16());
	`)
	if got := evalStr(t, js, "r.s16bad"); got != "threw" {
		t.Errorf("swap16 on length 3 = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.s32bad"); got != "threw" {
		t.Errorf("swap32 on length 5 = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.s16ok"); got != "ok" {
		t.Errorf("swap16 on length 4 = %q, want ok", got)
	}
}

// TestQuerystringMaxKeysCountsDuplicates verifies duplicate keys count against
// the maxKeys cap (can't be bypassed by repeating one key).
func TestQuerystringMaxKeysCountsDuplicates(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const qs = require("querystring");
		globalThis.r = {};
		r.n = qs.parse("a=1&a=2&a=3", "&", "=", { maxKeys: 2 }).a.length;
	`)
	if got := evalStr(t, js, "String(r.n)"); got != "2" {
		t.Errorf("duplicate-key maxKeys cap = %q, want 2", got)
	}
}

// TestReadableFromObjectMode verifies Readable.from yields items unchanged
// (objectMode), not Buffer-coerced.
func TestReadableFromObjectMode(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		(async () => {
			const out = [];
			for await (const x of Readable.from(["hello", "world"])) out.push(typeof x + ":" + x);
			r.items = out.join(",");
		})().catch(e => { r.err = String(e); });
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("error: %s", got)
	}
	if got := evalStr(t, js, "r.items"); got != "string:hello,string:world" {
		t.Errorf("Readable.from items = %q, want string:hello,string:world", got)
	}
}

// TestPipelineDestroysOnError verifies stream.pipeline destroys the source when
// a later stage errors (rather than leaking it open).
func TestPipelineDestroysOnError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable, Writable, pipeline } = require("stream");
		const src = Readable.from(["a", "b", "c"]);
		src.on("close", () => { r.srcClosed = true; });
		const sink = new Writable({ write(chunk, enc, cb) { cb(new Error("sink fail")); } });
		pipeline(src, sink, (err) => { r.err = err ? err.message : "none"; });
	`)
	// Give the loop a moment to propagate destroy.
	if got := evalStr(t, js, "String(r.srcClosed ?? false)"); got != "true" {
		t.Errorf("pipeline did not destroy the source on error (srcClosed=%q)", got)
	}
}

// TestReadableDestroyDuringData verifies destroying a Readable from inside a
// 'data' handler stops further chunks (and no 'data'/'end' after 'close').
func TestReadableDestroyDuringData(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { seen: [], afterClose: [] };
		const { Readable } = require("stream");
		const s = new Readable({ read() {} });
		s.push("a"); s.push("b"); s.push("c"); s.push(null);
		let closed = false;
		s.on("close", () => { closed = true; });
		s.on("end", () => { if (closed) r.afterClose.push("end"); });
		s.on("data", (c) => {
			if (closed) { r.afterClose.push("data:" + c); return; }
			r.seen.push(String(c));
			if (r.seen.length === 1) s.destroy();
		});
	`)
	if got := evalStr(t, js, "r.seen.join(',')"); got != "a" {
		t.Errorf("chunks after destroy-in-data = %q, want just 'a'", got)
	}
	if got := evalStr(t, js, "r.afterClose.join(',')"); got != "" {
		t.Errorf("events fired after close: %q", got)
	}
}

// TestEventsOnceAbort verifies events.once rejects with AbortError when its
// signal aborts before the event fires.
func TestEventsOnceAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { EventEmitter, once } = require("events");
		globalThis.r = {};
		const ee = new EventEmitter();
		const c = new AbortController();
		(async () => {
			try { await once(ee, "x", { signal: c.signal }); r.outcome = "resolved"; }
			catch (e) { r.outcome = "rejected:" + e.name; }
		})();
		c.abort();
	`)
	if got := evalStr(t, js, "r.outcome"); got != "rejected:AbortError" {
		t.Errorf("events.once abort = %q, want rejected:AbortError", got)
	}
}
