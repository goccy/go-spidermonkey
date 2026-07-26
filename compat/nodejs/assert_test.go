package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestAssertMapSetAndThrowsMatcher pins the assert fixes: deepStrictEqual
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

// TestAssertStructuralSetMap verifies deepStrictEqual matches Set elements and
// Map keys structurally, so equal-shaped object members compare equal.
func TestAssertStructuralSetMap(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const assert = require("assert");
		globalThis.r = {};
		const eq = (name, a, b, wantEqual) => {
			let threw = false;
			try { assert.deepStrictEqual(a, b); } catch { threw = true; }
			r[name] = (threw === !wantEqual);
		};
		eq("setObjEq", new Set([{a:1}]), new Set([{a:1}]), true);
		eq("mapObjKeyEq", new Map([[{k:1}, 2]]), new Map([[{k:1}, 2]]), true);
		eq("setObjNeq", new Set([{a:1}]), new Set([{a:2}]), false);
	`)
	for _, name := range []string{"setObjEq", "mapObjKeyEq", "setObjNeq"} {
		if !evalVal(t, js, "r."+name).Bool() {
			t.Errorf("structural deepStrictEqual case %q incorrect", name)
		}
	}
}

// TestAssertStrictAndMissingMethods verifies assert/strict does STRICT comparison
// (0 == ” throws) and that ifError / notDeepStrictEqual / doesNotReject exist.
func TestAssertStrictAndMissingMethods(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const assert = require("assert");
		const strict = require("assert/strict");
		try { strict.equal(0, ""); r.strictLoose = "passed"; } catch { r.strictLoose = "threw"; }
		r.ifErrorType = typeof assert.ifError;
		try { assert.ifError(null); r.ifErrorNull = "ok"; } catch { r.ifErrorNull = "threw"; }
		try { assert.ifError(new Error("boom")); r.ifErrorErr = "ok"; } catch (e) { r.ifErrorErr = "threw:" + e.message; }
		r.notDeepStrict = typeof assert.notDeepStrictEqual;
		r.doesNotReject = typeof assert.doesNotReject;
	`)
	for _, c := range []struct{ expr, want, msg string }{
		{`r.strictLoose`, "threw", "assert/strict.equal(0,'') should throw (was loose ==)"},
		{`r.ifErrorType`, "function", "assert.ifError missing"},
		{`r.ifErrorNull`, "ok", "ifError(null) should not throw"},
		{`r.ifErrorErr`, "threw:boom", "ifError(err) should throw the error"},
		{`r.notDeepStrict`, "function", "assert.notDeepStrictEqual missing"},
		{`r.doesNotReject`, "function", "assert.doesNotReject missing"},
	} {
		if got := evalStr(t, js, c.expr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.msg, got, c.want)
		}
	}
}
