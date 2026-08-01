package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
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

// assert.throws / assert.rejects with a validation object apply RegExp
// property values with .test() against the actual (string) property — for ANY
// property, not just message. Regression: object values were deep-equaled, so
// { message: /regexp/ } always failed.
func TestAssertThrowsRegExpValidationObject(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const assert = require("assert");
		const passed = (fn) => { try { fn(); return "pass"; } catch (e) { return e.code || e.name; } };

		const boom = () => { const e = new Error("boom went the engine"); e.code = "ENGINE"; throw e; };

		// RegExp on message matches.
		r.msgRe = passed(() => assert.throws(boom, { message: /went the/ }));
		// RegExp on ANOTHER property (code) matches too.
		r.codeRe = passed(() => assert.throws(boom, { code: /^ENG/ }));
		// Mixed literal + RegExp.
		r.mixed = passed(() => assert.throws(boom, { code: "ENGINE", message: /boom/ }));
		// A non-matching RegExp still fails the assertion.
		r.noMatch = passed(() => assert.throws(boom, { message: /quiet/ }));
		// Literal values keep working.
		r.literal = passed(() => assert.throws(boom, { code: "ENGINE" }));

		(async () => {
			r.rejects = await assert.rejects(async () => boom(), { message: /engine$/ })
				.then(() => "pass", (e) => e.code || e.name);
		})();
	`)
	for expr, want := range map[string]string{
		"r.msgRe":   "pass",
		"r.codeRe":  "pass",
		"r.mixed":   "pass",
		"r.noMatch": "ERR_ASSERTION",
		"r.literal": "pass",
		"r.rejects": "pass",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// assert.deepEqual is LOOSE in Node: primitives via ==, prototypes not
// compared, NaN equal to NaN — while deepStrictEqual stays strict.
// Regression: deepEqual used the strict comparator, so deepEqual(1, "1") and
// null-prototype-vs-plain objects threw.
func TestAssertDeepEqualLooseSemantics(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const assert = require("assert");
		const passed = (fn) => { try { fn(); return "pass"; } catch (e) { return "throw"; } };

		// Loose primitive coercion.
		r.numStr = passed(() => assert.deepEqual(1, "1"));
		r.nested = passed(() => assert.deepEqual({ a: 1, b: [2, 3] }, { a: "1", b: ["2", "3"] }));
		r.nullUndef = passed(() => assert.deepEqual({ a: null }, { a: undefined }));
		// NaN is equal to NaN in loose deepEqual.
		r.nan = passed(() => assert.deepEqual(NaN, NaN));
		r.nanNested = passed(() => assert.deepEqual({ v: NaN }, { v: NaN }));
		// Prototypes are NOT compared: null-prototype vs plain object.
		const np = Object.create(null); np.a = 1;
		r.nullProto = passed(() => assert.deepEqual(np, { a: 1 }));
		// Still detects real differences.
		r.diff = passed(() => assert.deepEqual({ a: 1 }, { a: 2 }));
		r.extraKey = passed(() => assert.deepEqual({ a: 1 }, { a: 1, b: 2 }));
		// Buffers / TypedArrays / Map / Set are content-compared.
		r.buf = passed(() => assert.deepEqual(Buffer.from("ab"), Buffer.from("ab")));
		r.bufDiff = passed(() => assert.deepEqual(Buffer.from("ab"), Buffer.from("ac")));
		r.map = passed(() => assert.deepEqual(new Map([["k", 1]]), new Map([["k", "1"]])));
		r.set = passed(() => assert.deepEqual(new Set([1, 2]), new Set([2, 1])));

		// notDeepEqual gets the loose comparator: loosely-equal values THROW.
		r.notDeep = passed(() => assert.notDeepEqual({ a: 1 }, { a: "1" }));
		r.notDeepOk = passed(() => assert.notDeepEqual({ a: 1 }, { a: 2 }));

		// Loose is not ANYTHING-goes: type tags still gate equality (Node's
		// documented rule), Error name/message are always compared, and an
		// object never loosely equals a primitive.
		r.arrVsObj = passed(() => assert.deepEqual([1, 2], { 0: 1, 1: 2 }));
		r.dateVsObj = passed(() => assert.deepEqual(new Date(0), {}));
		r.errVsObj = passed(() => assert.deepEqual(new Error("x"), {}));
		r.errMsgDiff = passed(() => assert.notDeepEqual(new Error("a"), new Error("b")));
		r.boxVsPrim = passed(() => assert.notDeepEqual(new Number(1), 1));
		r.boxDiff = passed(() => assert.notDeepEqual(new Number(1), new Number(2)));
		r.viewKind = passed(() => assert.notDeepEqual(new Uint8Array([1, 0]), new Uint16Array([1])));

		// deepStrictEqual is unchanged (strict).
		r.strictNumStr = passed(() => assert.deepStrictEqual(1, "1"));
		r.strictProto = passed(() => assert.deepStrictEqual(np, { a: 1 }));
		r.strictSame = passed(() => assert.deepStrictEqual({ a: 1 }, { a: 1 }));
		// assert.strict.deepEqual aliases the strict form.
		r.strictAlias = passed(() => require("assert/strict").deepEqual(1, "1"));
	`)
	for expr, want := range map[string]string{
		"r.numStr":       "pass",
		"r.nested":       "pass",
		"r.nullUndef":    "pass",
		"r.nan":          "pass",
		"r.nanNested":    "pass",
		"r.nullProto":    "pass",
		"r.diff":         "throw",
		"r.extraKey":     "throw",
		"r.buf":          "pass",
		"r.bufDiff":      "throw",
		"r.map":          "pass",
		"r.set":          "pass",
		"r.notDeep":      "throw",
		"r.notDeepOk":    "pass",
		"r.arrVsObj":     "throw",
		"r.dateVsObj":    "throw",
		"r.errVsObj":     "throw",
		"r.errMsgDiff":   "pass",
		"r.boxVsPrim":    "pass",
		"r.boxDiff":      "pass",
		"r.viewKind":     "pass",
		"r.strictNumStr": "throw",
		"r.strictProto":  "throw",
		"r.strictSame":   "pass",
		"r.strictAlias":  "throw",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
