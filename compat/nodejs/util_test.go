package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

// TestParseArgsNonStrictPositionals verifies util.parseArgs allows positionals
// when strict is false.
func TestParseArgsNonStrictPositionals(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		try {
			const p = util.parseArgs({ strict: false, args: ["foo", "bar"] });
			r.positionals = p.positionals.join(",");
		} catch (e) { r.err = String(e); }
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("parseArgs threw: %s", got)
	}
	if got := evalStr(t, js, "r.positionals"); got != "foo,bar" {
		t.Errorf("non-strict positionals = %q, want foo,bar", got)
	}
}

// TestPromisifyForwardsThis verifies util.promisify preserves the receiver, so a
// promisified method called as obj.pf() runs with this===obj.
func TestPromisifyForwardsThis(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const util = require("util");
		const obj = { val: 42, m(cb) { cb(null, this ? this.val : "no-this"); } };
		obj.pf = util.promisify(obj.m);
		obj.pf().then(v => { r.res = v; }).catch(e => { r.err = String(e && e.message || e); });
	`)
	if got := evalStr(t, js, `String(r.err ?? "")`); got != "" {
		t.Fatalf("promisified call rejected: %s", got)
	}
	if got := evalStr(t, js, `String(r.res)`); got != "42" {
		t.Errorf("promisify dropped the receiver: this.val = %q, want 42", got)
	}
}

// TestInspectTypedArray verifies util.inspect renders a non-Buffer TypedArray as
// `Uint8Array(3) [ 1, 2, 3 ]`, not a plain {0:1,...} object.
func TestInspectTypedArray(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const util = require("util");
		r.u8 = util.inspect(new Uint8Array([1, 2, 3]));
		r.f64 = util.inspect(new Float64Array([1.5]));
	`)
	if got := evalStr(t, js, `r.u8`); got != "Uint8Array(3) [ 1, 2, 3 ]" {
		t.Errorf("inspect(Uint8Array) = %q, want Uint8Array(3) [ 1, 2, 3 ]", got)
	}
	if got := evalStr(t, js, `r.f64`); got != "Float64Array(1) [ 1.5 ]" {
		t.Errorf("inspect(Float64Array) = %q, want Float64Array(1) [ 1.5 ]", got)
	}
}
