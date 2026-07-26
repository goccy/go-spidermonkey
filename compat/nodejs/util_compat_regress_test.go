package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// util.types used to expose only 5 predicates; the full commonly-used set must
// exist and use brand checks that a spoofed Symbol.toStringTag cannot fool.
func TestUtilTypesPredicates(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { types } = require("util");
		globalThis.r = { fails: [] };
		const check = (name, positive, negatives) => {
			if (typeof types[name] !== "function") { r.fails.push(name + ":missing"); return; }
			if (positive !== undefined && types[name](positive) !== true) r.fails.push(name + ":false-negative");
			for (const n of negatives) {
				if (types[name](n) === true) r.fails.push(name + ":false-positive");
			}
		};
		const spoofedDate = { [Symbol.toStringTag]: "Date" };
		check("isDate", new Date(), [spoofedDate, "2024-01-01", 5, null]);
		check("isRegExp", /x/, [{ [Symbol.toStringTag]: "RegExp" }, "x", null]);
		check("isMap", new Map(), [new Set(), new WeakMap(), {}, null]);
		check("isSet", new Set(), [new Map(), new WeakSet(), {}]);
		check("isWeakMap", new WeakMap(), [new Map(), {}]);
		check("isWeakSet", new WeakSet(), [new Set(), {}]);
		check("isArrayBuffer", new ArrayBuffer(4), [new Uint8Array(4), new DataView(new ArrayBuffer(4)), {}]);
		check("isAnyArrayBuffer", new ArrayBuffer(4), [new Uint8Array(4), {}]);
		check("isArrayBufferView", new Uint8Array(4), [new ArrayBuffer(4), [], {}]);
		check("isTypedArray", new Float32Array(2), [new DataView(new ArrayBuffer(4)), [], new ArrayBuffer(4)]);
		check("isUint8Array", new Uint8Array(1), [new Int8Array(1), new Uint8ClampedArray(1), Object.create(Uint8Array.prototype)]);
		check("isInt8Array", new Int8Array(1), [new Uint8Array(1)]);
		check("isUint16Array", new Uint16Array(1), [new Int16Array(1)]);
		check("isInt16Array", new Int16Array(1), [new Uint16Array(1)]);
		check("isUint32Array", new Uint32Array(1), [new Int32Array(1)]);
		check("isInt32Array", new Int32Array(1), [new Uint32Array(1)]);
		check("isFloat32Array", new Float32Array(1), [new Float64Array(1)]);
		check("isFloat64Array", new Float64Array(1), [new Float32Array(1)]);
		check("isBigInt64Array", new BigInt64Array(1), [new BigUint64Array(1)]);
		check("isBigUint64Array", new BigUint64Array(1), [new BigInt64Array(1)]);
		check("isDataView", new DataView(new ArrayBuffer(4)), [new Uint8Array(4), {}]);
		check("isPromise", Promise.resolve(), [{ then() {} }, {}]);
		check("isAsyncFunction", async () => {}, [() => {}, function* () {}]);
		check("isGeneratorFunction", function* () {}, [() => {}, async () => {}]);
		check("isGeneratorObject", (function* () {})(), [{}, (async function () {})()]);
		check("isNativeError", new TypeError("x"), [{ message: "x" }, "err"]);
		check("isNumberObject", new Number(5), [5, new String("5")]);
		check("isStringObject", new String("s"), ["s", new Number(1)]);
		check("isBooleanObject", new Boolean(true), [true, new Number(1)]);
		check("isSymbolObject", Object(Symbol("s")), [Symbol("s"), {}]);
		check("isBigIntObject", Object(1n), [1n, new Number(1)]);
		check("isBoxedPrimitive", new Number(1), [1, "s", {}, new Date()]);
		// isProxy is not detectable from JS (needs engine support): always false.
		check("isProxy", undefined, [new Proxy({}, {}), {}]);
		if (typeof SharedArrayBuffer !== "undefined") {
			check("isSharedArrayBuffer", new SharedArrayBuffer(4), [new ArrayBuffer(4)]);
			if (types.isAnyArrayBuffer(new SharedArrayBuffer(4)) !== true) r.fails.push("isAnyArrayBuffer:sab");
			if (types.isArrayBuffer(new SharedArrayBuffer(4)) === true) r.fails.push("isArrayBuffer:sab-false-positive");
		} else {
			check("isSharedArrayBuffer", undefined, [new ArrayBuffer(4)]);
		}
		r.sameAsModule = require("util/types") === types;
	`)
	if got := evalStr(t, js, `r.fails.join(",")`); got != "" {
		t.Errorf("util.types failures: %s", got)
	}
	if got := evalStr(t, js, `String(r.sameAsModule)`); got != "true" {
		t.Errorf("require('util/types') !== util.types")
	}
}

// util.callbackify used to drop `this` and pass falsy rejection reasons raw
// (so `if (err)` in the callback missed them).
func TestUtilCallbackifyReceiverAndFalsyRejection(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		// Receiver forwarding: a callbackified method sees this === obj.
		const obj = {
			tag: "self",
			async who() { return this === obj ? this.tag : "wrong-this"; },
		};
		obj.whoCb = util.callbackify(obj.who);
		obj.whoCb((err, v) => { r.thisResult = err ? "err" : v; });

		// Falsy rejection is wrapped in an Error with code + reason.
		util.callbackify(() => Promise.reject(null))((err) => {
			r.falsyIsError = err instanceof Error;
			r.falsyCode = err && err.code;
			r.falsyReasonIsNull = !!err && err.reason === null;
			r.falsyMsg = err && err.message;
		});
		util.callbackify(() => Promise.reject(0))((err) => {
			r.zeroCode = err && err.code;
			r.zeroReason = !!err && err.reason === 0;
		});

		// A truthy rejection passes through unchanged.
		const original = new Error("boom");
		util.callbackify(() => Promise.reject(original))((err) => {
			r.truthySame = err === original;
		});

		// Resolution still delivers (null, value).
		util.callbackify(async (a, b) => a + b)(2, 3, (err, v) => { r.sum = err ? "err" : v; });

		// Node throws when the last argument is not a function.
		try { util.callbackify(async () => 1)("not-a-cb"); r.noCb = "no-throw"; }
		catch (e) { r.noCb = e.code || e.name; }
	`)
	for expr, want := range map[string]string{
		"r.thisResult":                "self",
		"String(r.falsyIsError)":      "true",
		"r.falsyCode":                 "ERR_FALSY_VALUE_REJECTION",
		"String(r.falsyReasonIsNull)": "true",
		"r.falsyMsg":                  "Promise was rejected with falsy value",
		"r.zeroCode":                  "ERR_FALSY_VALUE_REJECTION",
		"String(r.zeroReason)":        "true",
		"String(r.truthySame)":        "true",
		"String(r.sum)":               "5",
		"r.noCb":                      "ERR_INVALID_ARG_TYPE",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// util.inspect: default depth is 2 (was 4), {depth: null} means unlimited
// (was swallowed by ?? into the default), and a top-level string is quoted
// with single quotes — while util.format("%s")/console-style formatting stays
// unquoted.
func TestUtilInspectDepthAndStringQuoting(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		const deep = { a: { b: { c: { d: { e: 1 } } } } };
		r.defaultDepth = util.inspect(deep);
		r.nullDepth = util.inspect(deep, { depth: null });
		r.zeroDepth = util.inspect(deep, { depth: 0 });
		r.topString = util.inspect("foo");
		r.quoteEscape = util.inspect("it's");
		r.nested = util.inspect({ s: "v" });
		r.formatS = util.format("%s", "foo");
		r.formatBare = util.format("foo");
		r.formatExtra = util.format("x:", "y");
	`)
	for expr, want := range map[string]string{
		// depth 2: values nested deeper than 2 levels collapse to [Object].
		"r.defaultDepth": "{ a: { b: { c: [Object] } } }",
		"r.nullDepth":    "{ a: { b: { c: { d: { e: 1 } } } } }",
		"r.zeroDepth":    "{ a: [Object] }",
		"r.topString":    "'foo'",
		"r.quoteEscape":  `'it\'s'`,
		"r.nested":       "{ s: 'v' }",
		"r.formatS":      "foo",
		"r.formatBare":   "foo",
		"r.formatExtra":  "x: y",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
