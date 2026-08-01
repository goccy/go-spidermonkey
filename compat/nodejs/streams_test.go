package nodejs_test

import (
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"net"
	"testing"
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

// TestAsyncIteratorAppliesBackpressure verifies that `for await (const c of
// readable)` reads on demand (paused mode) bounded by highWaterMark, instead of
// flipping the stream into unbounded FLOWING mode and buffering the whole source
// in guest heap while a slow consumer catches up.
//
// Probe: a synthetic objectMode Readable with highWaterMark 2 and 50 chunks,
// consumed by a for-await body that awaits each iteration. The producer records
// how far ahead of the consumer it ever got (its lead). With correct
// backpressure the lead stays ~highWaterMark; the old flowing-mode iterator let
// it reach ~49/50 (the whole stream in memory).
func TestAsyncIteratorAppliesBackpressure(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const TOTAL = 50;
		let produced = 0, consumed = 0, maxLead = 0;
		const rs = new Readable({
			objectMode: true,
			highWaterMark: 2,
			read() {
				// Push until push() signals backpressure (a well-behaved source).
				while (produced < TOTAL) {
					const more = this.push(produced);
					produced++;
					const lead = produced - consumed;
					if (lead > maxLead) maxLead = lead;
					if (!more) return;
				}
				this.push(null);
			},
		});
		(async () => {
			try {
				const got = [];
				for await (const c of rs) {
					consumed++;
					got.push(c);
					// Slow consumer: yield the loop between chunks.
					await new Promise((res) => setTimeout(res, 0));
				}
				r.count = got.length;
				r.ordered = got.join(",") === Array.from({ length: TOTAL }, (_, i) => i).join(",");
				r.maxLead = maxLead;
				r.produced = produced;
			} catch (e) { r.err = String(e); }
		})();
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("iteration errored: %s", got)
	}
	if got := evalStr(t, js, "String(r.count)"); got != "50" {
		t.Errorf("delivered chunk count = %q, want 50", got)
	}
	if got := evalStr(t, js, "String(r.ordered)"); got != "true" {
		t.Errorf("chunks not delivered in order (ordered=%q)", got)
	}
	// The whole point: the producer never got far ahead of the consumer. With the
	// buggy flowing-mode iterator this was ~49; with backpressure it stays near
	// highWaterMark (2). Allow a little slack, but it must be far below TOTAL.
	if got := evalStr(t, js, "String(r.maxLead)"); got != "1" && got != "2" && got != "3" && got != "4" {
		t.Errorf("producer lead = %q, want <=4 (~highWaterMark); a large value means no backpressure", got)
	}
}

// TestAsyncIteratorDeliversAllInOrderAndTerminates verifies a for-await over a
// byte-mode Readable delivers every byte in order and the loop terminates on
// 'end' with {done:true}.
func TestAsyncIteratorDeliversAllInOrderAndTerminates(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		const parts = ["alpha", "-", "beta", "-", "gamma"];
		let i = 0;
		const t = setInterval(() => {
			if (i < parts.length) rs.push(parts[i++]);
			else { clearInterval(t); rs.push(null); }
		}, 1);
		(async () => {
			try {
				let out = "";
				for await (const c of rs) out += c.toString();
				r.out = out;
				r.terminated = true;
			} catch (e) { r.err = String(e); }
		})();
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("iteration errored: %s", got)
	}
	if got := evalStr(t, js, "String(r.terminated)"); got != "true" {
		t.Errorf("loop did not terminate (terminated=%q)", got)
	}
	if got := evalStr(t, js, "r.out"); got != "alpha-beta-gamma" {
		t.Errorf("assembled body = %q, want alpha-beta-gamma", got)
	}
}

// TestAsyncIteratorBreakStopsSource verifies an early `break` stops iteration,
// destroys the source (so an infinite/large upstream is released), and detaches
// the iterator's listeners so nothing leaks — the source must NOT keep producing
// to completion.
func TestAsyncIteratorBreakStopsSource(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const TOTAL = 50;
		let produced = 0;
		const rs = new Readable({
			objectMode: true,
			highWaterMark: 2,
			read() {
				while (produced < TOTAL) {
					const more = this.push(produced);
					produced++;
					if (!more) return;
				}
				this.push(null);
			},
		});
		(async () => {
			let seen = 0;
			for await (const c of rs) {
				if (++seen === 3) break;
				await new Promise((res) => setTimeout(res, 0));
			}
			r.seen = seen;
			r.destroyed = rs.destroyed;
			r.produced = produced;
			r.dataListeners = rs.listenerCount("data");
			r.readableListeners = rs.listenerCount("readable");
		})();
	`)
	if got := evalStr(t, js, "String(r.seen)"); got != "3" {
		t.Errorf("iterations before break = %q, want 3", got)
	}
	if got := evalStr(t, js, "String(r.destroyed)"); got != "true" {
		t.Errorf("break did not destroy the source (destroyed=%q)", got)
	}
	// The source must have stopped well short of TOTAL — it was NOT drained to
	// completion into memory behind the broken-out consumer.
	if got := evalStr(t, js, "String(r.produced < 10)"); got != "true" {
		t.Errorf("source over-produced after break (produced=%s, want <10 of 50)", evalStr(t, js, "String(r.produced)"))
	}
	if got := evalStr(t, js, "String(r.dataListeners)"); got != "0" {
		t.Errorf("'data' listeners left after break = %q, want 0 (leak)", got)
	}
	if got := evalStr(t, js, "String(r.readableListeners)"); got != "0" {
		t.Errorf("'readable' listeners left after break = %q, want 0 (leak)", got)
	}
}

// TestAsyncIteratorErrorRejects verifies an 'error' during iteration rejects the
// pending next() (the for-await throws), rather than ending cleanly.
func TestAsyncIteratorErrorRejects(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.push("first");
		setTimeout(() => rs.destroy(new Error("boom")), 5);
		(async () => {
			try {
				const got = [];
				for await (const c of rs) got.push(c.toString());
				r.outcome = "ended";
				r.got = got.join(",");
			} catch (e) { r.outcome = "threw"; r.msg = e.message; r.got = "first-seen"; }
		})();
	`)
	if got := evalStr(t, js, "String(r.outcome)"); got != "threw" {
		t.Errorf("iteration outcome = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.msg ?? ''"); got != "boom" {
		t.Errorf("rejection message = %q, want boom", got)
	}
}

// TestAsyncIteratorAlreadyEndedYieldsDone verifies iterating a Readable that has
// already ended before iteration begins yields {done:true} immediately (no hang,
// no error).
func TestAsyncIteratorAlreadyEndedYieldsDone(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.resume();          // start it so 'end' can fire
		rs.push("x");
		rs.push(null);
		// Consume + let 'end' fire, THEN iterate the already-ended stream.
		rs.on("data", () => {});
		setTimeout(async () => {
			try {
				let count = 0;
				for await (const c of rs) count++;
				r.count = count;
				r.done = true;
			} catch (e) { r.err = String(e); }
		}, 10);
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("iterating an already-ended stream errored: %s", got)
	}
	if got := evalStr(t, js, "String(r.done)"); got != "true" {
		t.Errorf("iteration over already-ended stream did not finish (done=%q)", got)
	}
	if got := evalStr(t, js, "String(r.count)"); got != "0" {
		t.Errorf("already-ended stream yielded %q chunks, want 0", got)
	}
}

// TestAsyncIteratorPreservesEncoding verifies setEncoding + for-await still
// delivers the decoded string body, including a trailing multi-byte sequence
// split across chunk boundaries (the decoder's end() residual). The paused-mode
// iterator must not drop that final flush.
func TestAsyncIteratorPreservesEncoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.setEncoding("utf8");
		// "é" is 0xc3 0xa9; split it across two pushes so the decoder must hold
		// the tail and flush it. A final incomplete lead byte (0xc3) must surface
		// as U+FFFD on end() — proving the end-of-stream decoder flush is kept.
		rs.push(Buffer.from([0x68, 0x69, 0xc3])); // "hi" + start of "é"
		rs.push(Buffer.from([0xa9]));             // rest of "é"
		rs.push(Buffer.from([0xc3]));             // dangling lead byte
		rs.push(null);
		(async () => {
			try {
				let out = "";
				for await (const c of rs) out += c;
				r.out = out;
				r.codes = Array.from(out).map((ch) => ch.codePointAt(0)).join(",");
			} catch (e) { r.err = String(e); }
		})();
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("encoded iteration errored: %s", got)
	}
	// "hié" + U+FFFD (65533) from the dangling lead byte flushed at end().
	if got := evalStr(t, js, "r.codes"); got != "104,105,233,65533" {
		t.Errorf("decoded codepoints = %q, want 104,105,233,65533 (residual dropped?)", got)
	}
}

// TestReadableFromIsLazilyPulled verifies Readable.from advances its source
// iterator on demand (honoring push() backpressure) rather than eagerly draining
// the whole iterable into the buffer. A large source consumed slowly via
// for-await must stay bounded near highWaterMark.
func TestReadableFromIsLazilyPulled(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { Readable } = require("stream");
		const TOTAL = 50;
		let pulled = 0, consumed = 0, maxLead = 0;
		function* gen(n) {
			for (let i = 0; i < n; i++) {
				pulled++;
				const lead = pulled - consumed;
				if (lead > maxLead) maxLead = lead;
				yield i;
			}
		}
		const rs = Readable.from(gen(TOTAL), { highWaterMark: 2 });
		(async () => {
			try {
				const got = [];
				for await (const c of rs) {
					consumed++;
					got.push(c);
					await new Promise((res) => setTimeout(res, 0));
				}
				r.count = got.length;
				r.ordered = got.join(",") === Array.from({ length: TOTAL }, (_, i) => i).join(",");
				r.maxLead = maxLead;
			} catch (e) { r.err = String(e); }
		})();
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("Readable.from iteration errored: %s", got)
	}
	if got := evalStr(t, js, "String(r.count)"); got != "50" {
		t.Errorf("Readable.from delivered count = %q, want 50", got)
	}
	if got := evalStr(t, js, "String(r.ordered)"); got != "true" {
		t.Errorf("Readable.from items not in order (ordered=%q)", got)
	}
	// Lazy pull keeps the generator within ~highWaterMark of the consumer. The
	// old eager drain pulled all 50 up front (maxLead == 50).
	if got := evalStr(t, js, "String(r.maxLead <= 5)"); got != "true" {
		t.Errorf("Readable.from source lead = %s, want <=5 (~hwm); large means eager drain (no backpressure)", evalStr(t, js, "String(r.maxLead)"))
	}
}

func TestObjectModeStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Readable, Writable, Transform } = require("stream");
		globalThis.r = {};
		// objectMode Readable -> Transform -> Writable, passing objects (not Buffers).
		const src = Readable.from([{n:1},{n:2},{n:3}]);
		const doubler = new Transform({ objectMode: true, transform(o, e, cb){ cb(null, {n:o.n*2}); } });
		const collected = [];
		const sink = new Writable({ objectMode: true, write(o, e, cb){ collected.push(o.n); cb(); } });
		sink.on("finish", () => { r.collected = collected.join(","); });
		src.pipe(doubler).pipe(sink);
	`)
	if got := evalStr(t, js, "r.collected"); got != "2,4,6" {
		t.Errorf("objectMode pipeline = %q, want 2,4,6", got)
	}
}

func TestBackpressure(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Writable } = require("stream");
		globalThis.r = {};
		const pending = [];
		const w = new Writable({ highWaterMark: 4, write(chunk, e, cb){ pending.push(cb); } }); // never auto-drains
		// Write 4 bytes: fills hwm -> next write returns false.
		r.first = w.write(Buffer.from("ab"));   // buffered=2 < 4 -> true
		r.second = w.write(Buffer.from("cd"));  // buffered=4 >= 4 -> false
		r.drained = false;
		w.on("drain", () => { r.drained = true; });
		// Flush the callbacks -> buffered drops -> drain fires.
		pending.forEach((cb) => cb());
	`)
	if got := evalStr(t, js, "String(r.first)"); got != "true" {
		t.Errorf("first write = %q, want true", got)
	}
	if got := evalStr(t, js, "String(r.second)"); got != "false" {
		t.Errorf("second write (over hwm) = %q, want false", got)
	}
	if got := evalStr(t, js, "String(r.drained)"); got != "true" {
		t.Errorf("drain event = %q, want true", got)
	}
}

// Regression coverage for stream event ordering: 'close' must be the LAST
// event a stream emits. The writable state used to be marked finished
// synchronously while 'finish' itself was deferred to a nextTick, so a Duplex
// whose 'end' handler calls end() (the auto-end-after-peer-FIN socket path)
// emitted 'close' BEFORE the deferred 'finish'.

// Plain Writable: end() → 'finish' then 'close', never the reverse; listeners
// attached right after end() still catch 'finish' (deferred emit preserved).
func TestWritableEmitsFinishBeforeClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Writable } = require("stream");
		globalThis.r = { order: [] };
		const ws = new Writable({ write(c, e, cb) { cb(); } });
		ws.end("x");
		// Attached AFTER end() — the deferred 'finish' must still reach them.
		ws.on("finish", () => r.order.push("finish"));
		ws.on("close", () => r.order.push("close"));
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "finish,close" {
		t.Errorf("event order = %q, want finish,close", got)
	}
}

// Duplex whose 'end' handler calls end() — the exact shape of the socket
// auto-end path — must emit end → finish → close, with 'close' last.
func TestDuplexEndInsideEndHandlerEmitsFinishBeforeClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Duplex } = require("stream");
		globalThis.r = { order: [] };
		const d = new Duplex({ read() {}, write(c, e, cb) { cb(); } });
		d.on("end", () => { r.order.push("end"); d.end(); });
		d.on("finish", () => r.order.push("finish"));
		d.on("close", () => r.order.push("close"));
		d.resume();
		d.push(null);
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "end,finish,close" {
		t.Errorf("event order = %q, want end,finish,close ('close' must be last)", got)
	}
}

// Real socket: after the peer's FIN the default auto-end must produce
// end → finish → close on the net.Socket, 'close' strictly last.
func TestSocketAutoEndAfterPeerFinEmitsCloseLast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("data"))
		conn.Close() // FIN → guest auto-ends its writable half
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = { order: [] };
		const sock = net.connect(PORT, "127.0.0.1");
		sock.on("data", () => {});
		sock.on("end", () => r.order.push("end"));
		sock.on("finish", () => r.order.push("finish"));
		sock.on("close", () => r.order.push("close"));
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `r.order.join(",")`); got != "end,finish,close" {
		t.Errorf("socket event order = %q, want end,finish,close ('close' must be last)", got)
	}
}

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
	// With NO uncaughtException handler the throw is fatal, so the tick queued
	// after it never runs and the runtime exits 1 — checked against real Node,
	// which prints the error and dies with `ran = bad`. (The handler case, where
	// later ticks DO run, is TestNextTickUncaughtExceptionHandler.)
	rt.RunScript(context.Background(), `
		globalThis.__ran = [];
		process.nextTick(() => { __ran.push("bad"); throw new Error("boom"); });
		process.nextTick(() => { __ran.push("good"); });
	`)
	if got := evalStr(t, js, `(__ran||[]).join(",")`); got != "bad" {
		t.Fatalf("ticks ran = %q, want bad (an unhandled throw is fatal)", got)
	}
	if !rt.Exited() || rt.ExitCode() != 1 {
		t.Fatalf("exited=%v code=%d, want a fatal exit with code 1", rt.Exited(), rt.ExitCode())
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

// An async Transform must emit output in write order (Node serializes
// _transform: the next chunk waits for the previous callback).
func TestTransformSerializesAsyncOutput(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = { out: [] };
		const { Transform } = require("stream");
		let delay = 30;
		const t = new Transform({
			transform(c, e, cb) { const d = delay; delay -= 10; setTimeout(() => { this.push(c); cb(); }, d); }
		});
		t.on("data", (d) => { __r.out.push(String(d)); });
		t.write("1"); t.write("2"); t.write("3"); t.end();
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.out.join(",")`); got != "1,2,3" {
		t.Fatalf("async transform order = %q, want 1,2,3 (must serialize)", got)
	}
}

// read(size) returns null when fewer than size bytes are buffered and the
// stream hasn't ended (Node's framing-parser contract).
func TestReadableReadSizeReturnsNullWhenShort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		const { Readable } = require("stream");
		const rs = new Readable({ read() {} });
		rs.push(Buffer.from("abc")); // only 3 bytes, not ended
		__r.short = rs.read(8);           // want null
		rs.push(Buffer.from("defghij")); // now 10 total
		__r.full = rs.read(8);            // want 8 bytes
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `String(__r.short)`); got != "null" {
		t.Fatalf("read(8) with 3 buffered = %q, want null", got)
	}
	if got := evalStr(t, js, `__r.full && __r.full.length`); got != "8" {
		t.Fatalf("read(8) with 10 buffered length = %q, want 8", got)
	}
}

// destroy() must invoke the callbacks of still-queued writes (with an error),
// not strand them — otherwise a "wait for N write callbacks" barrier hangs.
func TestWritableDestroyFlushesQueuedCallbacks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = { calls: 0 };
		const { Writable } = require("stream");
		// A slow async _write so chunks queue behind the first.
		const ws = new Writable({ write(c, e, cb) { setTimeout(() => cb(new Error("boom")), 5); } });
		ws.on("error", () => {});
		for (let i = 0; i < 3; i++) ws.write("x" + i, () => { __r.calls++; });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `String(__r.calls)`); got != "3" {
		t.Fatalf("write callbacks fired = %s, want 3 (queued callbacks stranded)", got)
	}
}
