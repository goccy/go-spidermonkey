package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
