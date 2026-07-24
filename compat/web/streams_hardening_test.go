package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// A transform that throws must error the READABLE side too, so a consumer
// reading transform.readable directly (not via pipeThrough) gets the error
// instead of hanging forever.
func TestTransformStreamErrorPropagatesToReadable(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const ts = new TransformStream({ transform() { throw new Error("boom"); } });
			const w = ts.writable.getWriter();
			w.write(1).catch(() => {});
			const r = ts.readable.getReader();
			try { await r.read(); __c.result = "no-throw"; }
			catch (e) { __c.result = "errored:" + e.message; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.result`); got != "errored:boom" {
		t.Fatalf("reader result = %q, want errored:boom (readable must not hang)", got)
	}
}

// tee() delivers every chunk to both branches and is demand-driven.
func TestTeeDeliversToBothBranches(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const src = new ReadableStream({ start(c) { c.enqueue("x"); c.enqueue("y"); c.close(); } });
			const [a, b] = src.tee();
			const drain = async (s) => { const r = s.getReader(); let out = ""; for (;;) { const { value, done } = await r.read(); if (done) break; out += value; } return out; };
			const [ra, rb] = await Promise.all([drain(a), drain(b)]);
			__c.a = ra; __c.b = rb;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if a, b := evalString(t, js, `__c.a`), evalString(t, js, `__c.b`); a != "xy" || b != "xy" {
		t.Fatalf("tee branches = %q / %q, want xy / xy", a, b)
	}
}

// Aborting a WritableStream rejects the writer's closed promise with the reason
// (it previously resolved it, defeating error handling).
func TestWritableAbortRejectsClosed(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const ws = new WritableStream({});
			const w = ws.getWriter();
			const closed = w.closed.then(() => "resolved", (e) => "rejected:" + e.message);
			await w.abort(new Error("stop"));
			__c.closed = await closed;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.closed`); got != "rejected:stop" {
		t.Fatalf("writer.closed = %q, want rejected:stop", got)
	}
}

// structuredClone keeps two views over one ArrayBuffer sharing a single cloned
// buffer, and an own "__proto__" key stays a data property (no prototype set).
func TestStructuredCloneSharedBufferAndProto(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__c = {};
		const buf = new ArrayBuffer(8);
		const o = { a: new Uint8Array(buf), b: new Uint8Array(buf) };
		const c = structuredClone(o);
		c.a[0] = 42;
		__c.shared = c.b[0]; // 42 only if a and b share one cloned buffer
		const src = JSON.parse('{"__proto__":{"polluted":1}}');
		const cc = structuredClone(src);
		__c.protoClean = Object.getPrototypeOf(cc) === Object.prototype;
		__c.hasOwnProto = Object.prototype.hasOwnProperty.call(cc, "__proto__");
	`)
	if got := evalString(t, js, `String(__c.shared)`); got != "42" {
		t.Errorf("shared buffer clone = %s, want 42 (aliasing lost)", got)
	}
	if got := evalString(t, js, `String(__c.protoClean)`); got != "true" {
		t.Errorf("__proto__ key polluted the clone's prototype")
	}
	if got := evalString(t, js, `String(__c.hasOwnProto)`); got != "true" {
		t.Errorf("__proto__ should be an own data property on the clone")
	}
}

// A timer handle supports unref()/ref() and still clears via clearTimeout.
func TestTimerHandleUnref(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__c = {};
		try {
			const t = setTimeout(() => {}, 100000);
			t.unref().ref();
			clearTimeout(t);
			__c.ok = true;
		} catch (e) { __c.err = String(e && e.message || e); }
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("timer unref/clear threw: %s", got)
	}
	if evalString(t, js, `String(__c.ok)`) != "true" {
		t.Fatalf("timer handle unref/ref/clearTimeout did not complete")
	}
}
