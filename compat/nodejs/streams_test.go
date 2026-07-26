package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

// TestAsyncIteratorDestroyNoHang verifies a for-await over a Readable that is
// destroyed doesn't hang (it rejects/ends), and break tears down.
func TestAsyncIteratorDestroyNoHang(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		// destroy() mid-iteration must end the loop, not hang.
		(async () => {
			const rs = new Readable({ read() {} });
			rs.push("a");
			setTimeout(() => rs.destroy(), 5);
			try {
				for await (const c of rs) { r.got = (r.got || "") + c; }
				r.outcome = "ended";
			} catch (e) { r.outcome = "threw:" + (e.code || e.message); }
		})();
		// break must tear down the source.
		(async () => {
			const rs2 = new Readable({ read() {} });
			rs2.push("x"); rs2.push("y");
			for await (const c of rs2) { break; }
			r.broke = rs2.destroyed;
		})();
	`)
	if got := evalStr(t, js, "String(r.outcome ?? 'HANG')"); got == "HANG" {
		t.Error("for-await did not settle after destroy() (hang)")
	}
	if got := evalStr(t, js, "String(r.broke ?? false)"); got != "true" {
		t.Errorf("break did not destroy the source: %q", got)
	}
}

// TestPushAfterDestroyDiscarded verifies push() after destroy() is discarded.
func TestPushAfterDestroyDiscarded(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { afterClose: 0 };
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		let closed = false;
		rs.on("close", () => { closed = true; });
		rs.on("readable", () => { if (closed) { const c = rs.read(); if (c !== null) r.afterClose++; } });
		rs.destroy();
		r.pushRet = rs.push("late"); // must return false, deliver nothing
	`)
	if got := evalStr(t, js, "String(r.pushRet)"); got != "false" {
		t.Errorf("push after destroy returned %q, want false", got)
	}
	if got := evalStr(t, js, "String(r.afterClose)"); got != "0" {
		t.Errorf("data delivered after close: %q", got)
	}
}

// TestPipeResumesPausedSource verifies pipe() flows even a pre-paused source.
func TestPipeResumesPausedSource(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable, Writable } = require("stream");
		const rs = new Readable({ read() {} });
		let out = "";
		const ws = new Writable({ write(chunk, enc, cb) { out += chunk.toString(); cb(); } });
		ws.on("finish", () => { r.out = out; });
		rs.pause(); // explicitly paused BEFORE pipe
		rs.pipe(ws);
		rs.push("hello"); rs.push(null);
	`)
	if got := evalStr(t, js, "String(r.out ?? 'NONE')"); got != "hello" {
		t.Errorf("pipe of a paused source delivered %q, want hello", got)
	}
}

// TestPipeTearsDownSourceOnDestClose verifies that when the destination of a
// pipe() is destroyed mid-stream, the source is unpiped and destroyed (so an
// upstream — e.g. a proxied http body — is released rather than leaked), and no
// write-after-destroy error is emitted.
func TestPipeTearsDownSourceOnDestClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { srcErrors: 0 };
		const { Readable, Writable } = require("stream");
		const src = new Readable({ read() {} });
		src.on("close", () => { r.srcClosed = true; });
		src.on("error", () => { r.srcErrors++; });
		const dest = new Writable({ write(c, e, cb) { cb(); } });
		src.pipe(dest);
		src.push("a");
		// Destination dies mid-stream (e.g. downstream client disconnect).
		setTimeout(() => { dest.destroy(); }, 5);
		// A later chunk must not resurrect the pipe or error the source.
		setTimeout(() => { src.push("b"); }, 15);
	`)
	if got := evalStr(t, js, `String(r.srcClosed)`); got != "true" {
		t.Errorf("source not destroyed after dest close = %q, want true (upstream leak)", got)
	}
	if got := evalStr(t, js, `String(r.srcErrors)`); got != "0" {
		t.Errorf("source emitted %q errors after dest close, want 0", got)
	}
}

// TestReadAfterDestroyReturnsNull verifies Readable.read() returns null once the
// stream is destroyed, rather than delivering more buffered data.
func TestReadAfterDestroyReturnsNull(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.push("a"); rs.push("b");
		rs.destroy();
		r.afterDestroy = rs.read(); // must be null, not "a"
	`)
	if got := evalStr(t, js, `r.afterDestroy === null ? "null" : String(r.afterDestroy)`); got != "null" {
		t.Errorf("read() after destroy() = %q, want null", got)
	}
}

// TestStreamWebTextStreams verifies node:stream/web re-exports the WORKING
// TextEncoderStream/TextDecoderStream (globals), not throwing stubs.
func TestStreamWebTextStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const sw = require("node:stream/web");
		try { new sw.TextDecoderStream("utf-8"); new sw.TextEncoderStream(); r.ok = "ok"; }
		catch (e) { r.ok = "threw:" + e.message; }
	`)
	if got := evalStr(t, js, `r.ok`); got != "ok" {
		t.Errorf("stream/web text streams = %q, want ok (should re-export working globals)", got)
	}
}

// stream.finished() (and stream/promises.finished) must invoke its callback
// even when the stream is ALREADY in its terminal state at attach time, rather
// than waiting forever for an 'end'/'finish'/'close' event that already fired.
// Regression: `await finished(alreadyEndedStream)` used to hang.
func TestFinishedOnAlreadyEndedStream(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.__r = [];
		const stream = require("stream");
		const { finished: finishedP } = require("stream/promises");

		// Readable that has already ended.
		(async () => {
			const r = new stream.Readable({ read(){} });
			r.resume();
			r.push(null);
			await new Promise((res) => setTimeout(res, 10)); // let 'end' fire
			await finishedP(r); // must resolve, not hang
			__r.push("readable-finished");
		})();

		// Writable that has already finished.
		(async () => {
			const w = new stream.Writable({ write(c,e,cb){cb();} });
			w.end();
			await new Promise((res) => setTimeout(res, 10)); // let 'finish' fire
			await finishedP(w); // must resolve, not hang
			__r.push("writable-finished");
		})();

		// Callback form on an already-destroyed stream.
		(() => {
			const r = new stream.Readable({ read(){} });
			r.destroy();
			stream.finished(r, () => __r.push("destroyed-cb"));
		})();
	`)
	got := evalStr(t, js, `[...__r].sort().join(",")`)
	want := "destroyed-cb,readable-finished,writable-finished"
	if got != want {
		t.Fatalf("finished() results = %q, want %q", got, want)
	}
}
