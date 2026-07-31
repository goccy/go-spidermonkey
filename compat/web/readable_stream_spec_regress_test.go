package web_test

// WHATWG Streams / fetch-body conformance regressions:
//   - controller.desiredSize (default and custom queuing strategies), and
//     enqueue-after-close throwing TypeError
//   - reader.closed / writer.closed lifecycle promises
//   - rs.cancel() on a locked stream rejecting without touching the source
//   - pipeTo honoring options.signal (pre-aborted and mid-pipe)
//   - ReadableStream.from(asyncIterable)
//   - ReadableStream bodies on Response/Request (consumption, clone via tee)
//   - structuredClone transfer list detaching ArrayBuffers

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// The MDN-canonical pull pattern — `pull(c) { if (c.desiredSize > 0)
// c.enqueue(x) }` — must make progress (it used to hang forever because
// desiredSize was undefined).
func TestControllerDesiredSizeDrivesPull(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let n = 0;
			const rs = new ReadableStream({
				pull(c) {
					if (c.desiredSize > 0) {
						n++;
						if (n <= 3) c.enqueue("chunk" + n);
						else c.close();
					}
				},
			});
			let out = "";
			for await (const v of rs) out += v;
			__c.out = out;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.out`); got != "chunk1chunk2chunk3" {
		t.Fatalf("pull-driven stream = %q, want chunk1chunk2chunk3", got)
	}
}

func TestControllerDesiredSizeStates(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let ctrl;
			const rs = new ReadableStream({ start(c) { ctrl = c; } });
			__c.initial = ctrl.desiredSize;      // default HWM 1, empty queue
			ctrl.enqueue("a");
			__c.afterEnqueue = ctrl.desiredSize; // 1 - 1 = 0
			ctrl.enqueue("b");
			__c.afterSecond = ctrl.desiredSize;  // 1 - 2 = -1
			const r = rs.getReader();
			await r.read();
			__c.afterRead = ctrl.desiredSize;    // one chunk left: 0
			r.releaseLock();
			ctrl.close();
			__c.afterClose = ctrl.desiredSize;   // 0 once close requested

			let ctrl2;
			new ReadableStream({ start(c) { ctrl2 = c; } });
			ctrl2.error(new Error("x"));
			__c.afterError = ctrl2.desiredSize === null ? "null" : String(ctrl2.desiredSize);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`__c.initial`:      "1",
		`__c.afterEnqueue`: "0",
		`__c.afterSecond`:  "-1",
		`__c.afterRead`:    "0",
		`__c.afterClose`:   "0",
		`__c.afterError`:   "null",
	} {
		if got := evalString(t, js, `String(`+expr+`)`); got != want {
			t.Errorf("%s = %s, want %s", expr, got, want)
		}
	}
}

func TestQueuingStrategiesShapeDesiredSize(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let c1;
			new ReadableStream({ start(c) { c1 = c; } }, new CountQueuingStrategy({ highWaterMark: 4 }));
			__c.countEmpty = c1.desiredSize; // 4
			c1.enqueue("x");
			__c.countOne = c1.desiredSize;   // 3

			let c2;
			new ReadableStream({ start(c) { c2 = c; } }, new ByteLengthQueuingStrategy({ highWaterMark: 16 }));
			c2.enqueue(new Uint8Array(10));
			__c.bytesTen = c2.desiredSize;   // 16 - 10 = 6
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`__c.countEmpty`: "4",
		`__c.countOne`:   "3",
		`__c.bytesTen`:   "6",
	} {
		if got := evalString(t, js, `String(`+expr+`)`); got != want {
			t.Errorf("%s = %s, want %s", expr, got, want)
		}
	}
}

// controller.enqueue after close must throw TypeError (it was a silent no-op).
func TestEnqueueAfterCloseThrowsTypeError(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let ctrl;
			new ReadableStream({ start(c) { ctrl = c; } });
			ctrl.close();
			try { ctrl.enqueue("late"); __c.result = "no-throw"; }
			catch (e) { __c.result = e.constructor.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.result`); got != "TypeError" {
		t.Fatalf("enqueue after close threw %q, want TypeError", got)
	}
}

// reader.closed: resolves undefined on close, rejects with the stream error,
// rejects TypeError on releaseLock.
func TestReaderClosedPromise(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const observe = (p) => p.then((v) => "resolved:" + String(v), (e) => "rejected:" + e.constructor.name);

			let c1;
			const rs1 = new ReadableStream({ start(c) { c1 = c; } });
			const p1 = observe(rs1.getReader().closed);
			c1.close();
			__c.onClose = await p1;

			let c2;
			const rs2 = new ReadableStream({ start(c) { c2 = c; } });
			const p2 = observe(rs2.getReader().closed);
			c2.error(new RangeError("nope"));
			__c.onError = await p2;

			const rs3 = new ReadableStream({});
			const r3 = rs3.getReader();
			const p3 = observe(r3.closed);
			r3.releaseLock();
			__c.onRelease = await p3;

			const rs4 = new ReadableStream({ start(c) { c.close(); } });
			__c.alreadyClosed = await observe(rs4.getReader().closed);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`__c.onClose`:       "resolved:undefined",
		`__c.onError`:       "rejected:RangeError",
		`__c.onRelease`:     "rejected:TypeError",
		`__c.alreadyClosed`: "resolved:undefined",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// writer.closed rejects with TypeError once the writer releases its lock
// (unless close/abort settled it first).
func TestWriterClosedRejectsOnRelease(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const ws = new WritableStream({});
			const w = ws.getWriter();
			const p = w.closed.then((v) => "resolved:" + String(v), (e) => "rejected:" + e.constructor.name);
			w.releaseLock();
			__c.onRelease = await p;

			const ws2 = new WritableStream({});
			const w2 = ws2.getWriter();
			const p2 = w2.closed.then((v) => "resolved:" + String(v), (e) => "rejected:" + e.constructor.name);
			await w2.close();
			w2.releaseLock();
			__c.closeWins = await p2;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.onRelease`); got != "rejected:TypeError" {
		t.Errorf("writer.closed after releaseLock = %q, want rejected:TypeError", got)
	}
	if got := evalString(t, js, `__c.closeWins`); got != "resolved:undefined" {
		t.Errorf("writer.closed after close-then-release = %q, want resolved:undefined", got)
	}
}

// rs.cancel() on a locked stream rejects TypeError and must NOT invoke the
// underlying source's cancel.
func TestCancelLockedStreamRejects(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let sourceCancelled = false;
			const rs = new ReadableStream({ cancel() { sourceCancelled = true; } });
			rs.getReader(); // lock it
			try { await rs.cancel("why"); __c.result = "resolved"; }
			catch (e) { __c.result = e.constructor.name; }
			__c.sourceCancelled = sourceCancelled;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.result`); got != "TypeError" {
		t.Errorf("cancel() on locked stream = %q, want TypeError", got)
	}
	if got := evalString(t, js, `String(__c.sourceCancelled)`); got != "false" {
		t.Errorf("source.cancel invoked on locked-stream cancel; must not be")
	}
}

// pipeTo with an already-aborted signal rejects immediately and transfers
// nothing to the destination.
func TestPipeToPreAbortedSignal(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const written = [];
			const rs = new ReadableStream({ start(c) { c.enqueue("a"); c.close(); } });
			const ws = new WritableStream({ write(chunk) { written.push(chunk); } });
			const ac = new AbortController();
			ac.abort();
			try { await rs.pipeTo(ws, { signal: ac.signal }); __c.result = "resolved"; }
			catch (e) { __c.result = (e && e.name) || e.constructor.name; }
			__c.writtenCount = written.length;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.result`); got != "AbortError" {
		t.Errorf("pre-aborted pipeTo rejected with %q, want AbortError", got)
	}
	if got := evalString(t, js, `String(__c.writtenCount)`); got != "0" {
		t.Errorf("pre-aborted pipeTo wrote %s chunks, want 0", got)
	}
}

// Aborting mid-pipe stops the transfer, cancels the source, aborts the
// destination, and rejects with the abort reason.
func TestPipeToAbortMidPipe(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let sourceCancelled = false;
			let destAborted = false;
			const written = [];
			let ctrl;
			// The source enqueues one chunk and then stays pending forever — only
			// the signal can end the pipe. Its cancel is declared UP FRONT: the
			// standard reads an underlying source's members once, when the stream is
			// constructed, so assigning one afterwards is not observable.
			const rs = new ReadableStream({
				start(c) { ctrl = c; c.enqueue("first"); },
				cancel() { sourceCancelled = true; },
			});
			const ac = new AbortController();
			const ws = new WritableStream({
				write(chunk) {
					written.push(chunk);
					ac.abort(); // abort as soon as the first chunk lands
				},
				abort() { destAborted = true; },
			});
			try { await rs.pipeTo(ws, { signal: ac.signal }); __c.result = "resolved"; }
			catch (e) { __c.result = (e && e.name) || e.constructor.name; }
			__c.written = written.join(",");
			__c.sourceCancelled = sourceCancelled;
			__c.destAborted = destAborted;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.result`); got != "AbortError" {
		t.Errorf("mid-pipe abort rejected with %q, want AbortError", got)
	}
	if got := evalString(t, js, `__c.written`); got != "first" {
		t.Errorf("written = %q, want just the first chunk", got)
	}
	if got := evalString(t, js, `String(__c.sourceCancelled)`); got != "true" {
		t.Errorf("mid-pipe abort must cancel the source")
	}
	if got := evalString(t, js, `String(__c.destAborted)`); got != "true" {
		t.Errorf("mid-pipe abort must abort the destination")
	}
}

// ReadableStream.from pulls an async iterable lazily and forwards cancel to
// iterator.return (so generator finally blocks run).
func TestReadableStreamFrom(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			let produced = 0;
			let cleanedUp = false;
			async function* gen() {
				try {
					for (let i = 1; ; i++) { produced = i; yield "v" + i; }
				} finally { cleanedUp = true; }
			}
			const rs = ReadableStream.from(gen());
			const r = rs.getReader();
			const a = await r.read();
			const b = await r.read();
			__c.values = a.value + "," + b.value;
			// Lazy: an infinite generator must only have produced what was read
			// (at most one chunk of read-ahead).
			__c.produced = produced;
			await r.cancel("done");
			__c.cleanedUp = cleanedUp;

			// A sync iterable works too.
			const all = [];
			for await (const v of ReadableStream.from([1, 2, 3])) all.push(v);
			__c.syncAll = all.join(",");
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.values`); got != "v1,v2" {
		t.Errorf("from(asyncGen) values = %q, want v1,v2", got)
	}
	if got := evalString(t, js, `String(__c.produced <= 3)`); got != "true" {
		t.Errorf("from(asyncGen) not lazy: produced %s values for 2 reads", evalString(t, js, `String(__c.produced)`))
	}
	if got := evalString(t, js, `String(__c.cleanedUp)`); got != "true" {
		t.Errorf("cancel must call iterator.return (generator finally did not run)")
	}
	if got := evalString(t, js, `__c.syncAll`); got != "1,2,3" {
		t.Errorf("from(array) = %q, want 1,2,3", got)
	}
}

// new Response(readableStream): consumption drains the stream, bodyUsed and
// the single-read rule apply, .body hands back the stream, clone() tees.
func TestResponseReadableStreamBody(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const make = () => new ReadableStream({
				start(c) { c.enqueue("hello "); c.enqueue(new TextEncoder().encode("world")); c.close(); },
			});

			const r1 = new Response(make());
			__c.bodyIsStream = r1.body instanceof ReadableStream;
			__c.text = await r1.text();
			__c.usedAfter = r1.bodyUsed;
			try { await r1.text(); __c.second = "resolved"; }
			catch (e) { __c.second = e.constructor.name; }

			const r2 = new Response(make());
			const clone = r2.clone();
			__c.cloneText = await clone.text();
			__c.origText = await r2.text();

			const r3 = new Response(new ReadableStream({
				start(c) { c.enqueue(JSON.stringify({ n: 7 })); c.close(); },
			}));
			__c.jsonN = (await r3.json()).n;

			// A locked stream body cannot be consumed or cloned.
			const r4 = new Response(make());
			r4.body.getReader();
			try { await r4.text(); __c.locked = "resolved"; }
			catch (e) { __c.locked = e.constructor.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`String(__c.bodyIsStream)`: "true",
		`__c.text`:                 "hello world",
		`String(__c.usedAfter)`:    "true",
		`__c.second`:               "TypeError",
		`__c.cloneText`:            "hello world",
		`__c.origText`:             "hello world",
		`String(__c.jsonN)`:        "7",
		`__c.locked`:               "TypeError",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestRequestReadableStreamBody(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const req = new Request("http://example.test/upload", {
				method: "POST",
				body: new ReadableStream({ start(c) { c.enqueue("payload"); c.close(); } }),
			});
			__c.bodyIsStream = req.body instanceof ReadableStream;
			__c.text = await req.text();
			__c.used = req.bodyUsed;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`String(__c.bodyIsStream)`: "true",
		`__c.text`:                 "payload",
		`String(__c.used)`:         "true",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// structuredClone's transfer list: the clone carries the bytes, the source
// ArrayBuffer is detached; invalid transfer entries are DataCloneErrors.
func TestStructuredCloneTransfersArrayBuffer(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const buf = new Uint8Array([1, 2, 3, 4]).buffer;
			const out = structuredClone({ data: buf }, { transfer: [buf] });
			__c.cloneBytes = [...new Uint8Array(out.data)].join(",");
			__c.srcDetachedLen = buf.byteLength;
			try { new Uint8Array(buf); __c.srcView = "ok"; }
			catch (e) { __c.srcView = e.constructor.name; }

			// A transferred buffer that is not reachable from the value is still
			// detached.
			const stray = new ArrayBuffer(8);
			structuredClone("x", { transfer: [stray] });
			__c.strayLen = stray.byteLength;

			// Non-transferable entry -> DataCloneError.
			try { structuredClone({}, { transfer: [{}] }); __c.badEntry = "ok"; }
			catch (e) { __c.badEntry = e.name; }

			// Duplicate entry -> DataCloneError.
			const dup = new ArrayBuffer(4);
			try { structuredClone({}, { transfer: [dup, dup] }); __c.dupEntry = "ok"; }
			catch (e) { __c.dupEntry = e.name; }

			// An already-detached buffer -> DataCloneError.
			const gone = new ArrayBuffer(4);
			gone.transfer();
			try { structuredClone({}, { transfer: [gone] }); __c.detachedEntry = "ok"; }
			catch (e) { __c.detachedEntry = e.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		`__c.cloneBytes`:             "1,2,3,4",
		`String(__c.srcDetachedLen)`: "0",
		`__c.srcView`:                "TypeError",
		`String(__c.strayLen)`:       "0",
		`__c.badEntry`:               "DataCloneError",
		`__c.dupEntry`:               "DataCloneError",
		`__c.detachedEntry`:          "DataCloneError",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// A Response with a null-body status (204/205/304) must refuse a body at
// construction (TypeError) — otherwise a 204 with a stream body reaches the
// wire and aborts the connection.
func TestResponseNullBodyStatusRefusesBody(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	if got := evalString(t, js, `
		(() => {
			const out = [];
			for (const status of [204, 205, 304]) {
				try { new Response("x", { status }); out.push("no-throw"); }
				catch (e) { out.push(e.name); }
			}
			try { new Response(new ReadableStream({ start(c){ c.close(); } }), { status: 204 }); out.push("no-throw"); }
			catch (e) { out.push(e.name); }
			// null body with these statuses stays fine.
			out.push(new Response(null, { status: 204 }).status);
			return out.join(",");
		})()
	`); got != "TypeError,TypeError,TypeError,TypeError,204" {
		t.Errorf("null-body status handling = %q, want TypeError x4 then 204", got)
	}
}

// The queuing strategy's size function runs for every chunk (including one
// handed straight to a pending read), and an invalid size (NaN/negative/
// Infinity) errors the stream with a RangeError per spec.
func TestQueuingStrategySizeValidation(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__r = {};
		// Invalid size: ByteLengthQueuingStrategy over a chunk with no byteLength.
		try {
			const rs = new ReadableStream(
				{ start(c) { c.enqueue("not-a-buffer"); } },
				new ByteLengthQueuingStrategy({ highWaterMark: 16 }));
			__r.invalid = "no-throw";
		} catch (e) { __r.invalid = e.name; }

		// A chunk handed straight to a WAITING read is not measured: the standard
		// fulfils the read request and never reaches the size algorithm, so a
		// throwing size() is not called and the read succeeds. Only a chunk that
		// actually goes on the queue is sized.
		const rs2 = new ReadableStream(
			{ start(c) { globalThis.__c2 = c; } },
			{ highWaterMark: 4, size() { throw new Error("size-boom"); } });
		const rd = rs2.getReader();
		const p = rd.read().then((r) => { __r.fastPathValue = r.value; }, (e) => { __r.fastPathErr = e.message; });
		try { __c2.enqueue("x"); __r.fastPath = "no-throw"; } catch (e) { __r.fastPath = e.message; }
		// The NEXT chunk has no read waiting for it, so it is queued and measured —
		// and the throwing size() errors the stream then.
		try { __c2.enqueue("y"); __r.queued = "no-throw"; } catch (e) { __r.queued = e.message; }
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.invalid`); got != "RangeError" {
		t.Errorf("invalid byteLength size = %q, want RangeError", got)
	}
	if got := evalString(t, js, `__r.fastPath + "|" + __r.fastPathValue`); got != "no-throw|x" {
		t.Errorf("chunk handed to a waiting read = %q, want no-throw|x (size is not called)", got)
	}
	if got := evalString(t, js, `__r.queued`); got != "size-boom" {
		t.Errorf("throwing size() on a queued chunk = %q, want size-boom", got)
	}
}

// writer.close() on a TextDecoderStream whose readable side was cancelled
// must resolve (the held tail is dropped) — the enqueue-after-close TypeError
// used to reject it.
func TestTextDecoderStreamCloseAfterCancel(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const tds = new TextDecoderStream();
			const wr = tds.writable.getWriter();
			// NOT awaited: a transform's readable starts with backpressure on, so a
			// write does not complete until something reads. Awaiting it here would
			// wait for a reader that this test deliberately never provides.
			wr.write(new Uint8Array([0xE2, 0x82])).catch(() => {}); // incomplete sequence held
			await tds.readable.cancel("done reading");
			// Cancelling a transform's readable ERRORS its writable, so closing what
			// is already errored fails — the held tail has nowhere to go, and
			// reporting that rather than hanging is the point.
			try { await wr.close(); __c.outcome = "resolved"; }
			catch (e) { __c.outcome = e.name; }
		})().catch((e) => { __c.err = String(e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("close after cancel threw outside the guard: %s", got)
	}
	if got := evalString(t, js, `String(__c.outcome)`); got != "TypeError" {
		t.Errorf("close after cancel = %q, want TypeError (the stream is already errored)", got)
	}
}
