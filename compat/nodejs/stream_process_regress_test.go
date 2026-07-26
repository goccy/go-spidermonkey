package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestStreamFinishedOnErroredStream verifies finished() attached to an already-
// errored stream reports the error (not a silent success).
func TestStreamFinishedOnErroredStream(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable, finished } = require("stream");
		const rs = new Readable({ read() {} });
		rs.on("error", () => {}); // handler so destroy() doesn't throw
		rs.destroy(new Error("boom"));
		// Attach finished() AFTER the stream already errored.
		process.nextTick(() => {
			finished(rs, (err) => { r.cbErr = err ? err.message : "null"; });
		});
	`)
	if got := evalStr(t, js, `String(r.cbErr ?? "?")`); got != "boom" {
		t.Errorf("finished(erroredStream) callback err = %q, want boom (error swallowed)", got)
	}
}

// Duplex/Transform/PassThrough have their own destroy override; it must store
// the destroy error just like Readable/Writable so a later finished() reports
// it (zlib streams are Transforms — a flush failure must not turn into a
// silent success).
func TestStreamFinishedOnErroredTransform(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Transform, PassThrough, finished } = require("stream");
		const tr = new Transform();
		tr.on("error", () => {});
		tr.destroy(new Error("tr-boom"));
		const pt = new PassThrough();
		pt.on("error", () => {});
		pt.destroy(new Error("pt-boom"));
		process.nextTick(() => {
			finished(tr, (err) => { r.tr = err ? err.message : "null"; });
			finished(pt, (err) => { r.pt = err ? err.message : "null"; });
		});
	`)
	if got := evalStr(t, js, `r.tr + "|" + r.pt`); got != "tr-boom|pt-boom" {
		t.Errorf("finished(erroredDuplex) = %q, want tr-boom|pt-boom", got)
	}
}

// A stream destroyed WITHOUT an error before completing must report
// ERR_STREAM_PREMATURE_CLOSE from finished()/pipeline() (Node semantics) —
// not success. A cleanly ended stream must still report success.
func TestStreamFinishedPrematureClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable, Writable, PassThrough, finished, pipeline } = require("stream");

		// finished() attached BEFORE an error-less destroy.
		const r1 = new Readable({ read() {} });
		finished(r1, (err) => { r.before = err ? err.code : "null"; });
		r1.destroy();

		// finished() attached AFTER an error-less destroy.
		const r2 = new Readable({ read() {} });
		r2.destroy();
		process.nextTick(() => {
			finished(r2, (err) => { r.after = err ? err.code : "null"; });
		});

		// pipeline() whose destination dies mid-flight must fail.
		const src = new Readable({ read() {} });
		const dst = new Writable({ write(c, e, cb) { cb(); } });
		pipeline(src, dst, (err) => { r.pipe = err ? err.code : "null"; });
		src.push("chunk");
		setTimeout(() => dst.destroy(), 5);

		// Clean end/finish still succeeds.
		const ok = new PassThrough();
		ok.resume();
		finished(ok, (err) => { r.clean = err ? String(err.code || err.message) : "null"; });
		ok.end("data");
	`)
	if got := evalStr(t, js, `String(r.before)`); got != "ERR_STREAM_PREMATURE_CLOSE" {
		t.Errorf("finished before no-err destroy = %q, want ERR_STREAM_PREMATURE_CLOSE", got)
	}
	if got := evalStr(t, js, `String(r.after)`); got != "ERR_STREAM_PREMATURE_CLOSE" {
		t.Errorf("finished after no-err destroy = %q, want ERR_STREAM_PREMATURE_CLOSE", got)
	}
	if got := evalStr(t, js, `String(r.pipe)`); got != "ERR_STREAM_PREMATURE_CLOSE" {
		t.Errorf("pipeline with destination destroyed mid-flight = %q, want ERR_STREAM_PREMATURE_CLOSE", got)
	}
	if got := evalStr(t, js, `String(r.clean)`); got != "null" {
		t.Errorf("finished on cleanly ended stream = %q, want null", got)
	}
}

// process.emitWarning must emit the 'warning' event on process (with a proper
// Error carrying name/code), not just print to stderr — process.on("warning")
// listeners (and EventEmitter max-listeners leak detection, which routes
// through emitWarning) never fired.
func TestProcessEmitWarningEvent(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.__w = [];
		process.on("warning", (w) => __w.push(w.name + "|" + w.message + "|" + (w.code ?? "-")));
		process.emitWarning("plain string");
		process.emitWarning("typed", "DeprecationWarning", "DEP0001");
		process.emitWarning(new Error("as error"), { type: "CustomWarning", code: "C1" });

		// The EventEmitter max-listeners leak warning must reach 'warning' too.
		const { EventEmitter } = require("events");
		const e = new EventEmitter();
		e.setMaxListeners(2);
		for (let i = 0; i < 3; i++) e.on("x", () => {});
	`)
	got := evalStr(t, js, `__w.slice(0, 3).join(";")`)
	want := "Warning|plain string|-;DeprecationWarning|typed|DEP0001;CustomWarning|as error|C1"
	if got != want {
		t.Errorf("warning events = %q, want %q", got, want)
	}
	if got := evalStr(t, js, `String(__w.length >= 4 && /listener/i.test(__w[3]))`); got != "true" {
		t.Errorf("max-listeners leak warning did not reach process.on('warning'): %s",
			evalStr(t, js, `JSON.stringify(__w)`))
	}
}
