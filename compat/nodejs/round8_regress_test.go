package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestAssertMapSetAndThrowsMatcher pins the Round-8 assert fixes: deepStrictEqual
// compares Map/Set contents (not just "both are objects with no own keys"), and
// assert.throws validates its error matcher rather than accepting any throw.
func TestAssertMapSetAndThrowsMatcher(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const assert = require("assert");
		globalThis.r = {};
		const chk = (name, fn, shouldThrow) => {
			let threw = false;
			try { fn(); } catch { threw = true; }
			r[name] = threw === shouldThrow;
		};
		// Unequal Maps/Sets must NOT compare equal.
		chk("mapNeq", () => assert.deepStrictEqual(new Map([["a", 1]]), new Map()), true);
		chk("setNeq", () => assert.deepStrictEqual(new Set([1, 2]), new Set([9])), true);
		// Equal Maps must compare equal (no false positive).
		chk("mapEq", () => assert.deepStrictEqual(new Map([["a", 1]]), new Map([["a", 1]])), false);
		// throws with a wrong error type/regex must fail; with a matching one, pass.
		chk("wrongType", () => assert.throws(() => { throw new TypeError("boom"); }, RangeError), true);
		chk("wrongRegex", () => assert.throws(() => { throw new TypeError("boom"); }, /nope/), true);
		chk("rightType", () => assert.throws(() => { throw new TypeError("boom"); }, TypeError), false);
		chk("rightRegex", () => assert.throws(() => { throw new TypeError("boom"); }, /boom/), false);
	`)
	for _, name := range []string{"mapNeq", "setNeq", "mapEq", "wrongType", "wrongRegex", "rightType", "rightRegex"} {
		if !evalVal(t, js, "r."+name).Bool() {
			t.Errorf("assert case %q behaved incorrectly", name)
		}
	}
}

// TestTextEncoderEncodeInto verifies encodeInto exists and reports read/written.
func TestTextEncoderEncodeInto(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const dest = new Uint8Array(8);
		const res = new TextEncoder().encodeInto("abc", dest);
		r.read = res.read;
		r.written = res.written;
		r.bytes = Array.from(dest.subarray(0, 3)).join(",");
	`)
	if got := evalVal(t, js, "r.read").Int(); got != 3 {
		t.Errorf("read = %d, want 3", got)
	}
	if got := evalVal(t, js, "r.written").Int(); got != 3 {
		t.Errorf("written = %d, want 3", got)
	}
	if got := evalStr(t, js, "r.bytes"); got != "97,98,99" {
		t.Errorf("encoded bytes = %q, want 97,98,99", got)
	}
}

// TestInspectThrowingGetter verifies console.log/util.inspect degrade a throwing
// getter to a placeholder rather than throwing out of the log call.
func TestInspectThrowingGetter(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		try {
			r.out = util.inspect({ a: 1, get bad() { throw new Error("x"); } });
			r.threw = false;
		} catch (e) { r.threw = true; }
	`)
	if evalVal(t, js, "r.threw").Bool() {
		t.Fatal("util.inspect threw on a throwing getter instead of degrading")
	}
	if got := evalStr(t, js, "r.out"); got == "" {
		t.Error("util.inspect produced no output")
	}
}

// TestWorkerSetImmediate verifies setImmediate/setTimeout(fn,0) work inside a
// worker (the Atomics.waitAsync 0ms path returns a string, not a thenable).
func TestWorkerSetImmediate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			let order = [];
			setImmediate(() => {
				order.push("immediate");
				setTimeout(() => { order.push("timeout0"); parentPort.postMessage(order.join(",")); process.exit(0); }, 0);
			});
		`+"`"+`, { eval: true });
		w.on("message", (m) => { r.order = m; });
		w.on("error", (e) => { r.err = String(e); });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, "String(r.err ?? '')"); got != "" {
		t.Fatalf("worker error: %s", got)
	}
	if got := evalStr(t, js, "String(r.order ?? '')"); got != "immediate,timeout0" {
		t.Errorf("worker timer order = %q, want immediate,timeout0", got)
	}
}
