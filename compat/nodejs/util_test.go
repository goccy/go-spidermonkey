package nodejs_test

import (
	"bytes"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"strings"
	"testing"
	"testing/fstest"
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

// util.getCallSites (Node >= 22) reports the current call stack as structured
// frames. Node's own test harness destructures it from node:util, and tracing
// libraries use it; without it those tests fail with "getCallSites is not a
// function" before reaching what they meant to check.
func TestUtilGetCallSites(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	runScript(t, rt, `
		const { getCallSites } = require("util");
		function inner() { return getCallSites(); }
		function outer() { return inner(); }
		const sites = outer();
		globalThis.r = {
			n: sites.length,
			first: sites[0] && sites[0].functionName,
			second: sites[1] && sites[1].functionName,
			hasLine: !!(sites[0] && Number.isInteger(sites[0].lineNumber)),
			hasCol: !!(sites[0] && Number.isInteger(sites[0].columnNumber)),
			limited: getCallSites(1).length,
		};
	`)
	if got := evalStr(t, js, `String(r.first)`); got != "inner" {
		t.Errorf("first frame = %q, want \"inner\" (this function's own frame must be dropped)", got)
	}
	if got := evalStr(t, js, `String(r.second)`); got != "outer" {
		t.Errorf("second frame = %q, want \"outer\"", got)
	}
	if !evalVal(t, js, `r.hasLine && r.hasCol`).Bool() {
		t.Error("frames carry no line/column")
	}
	if got := evalStr(t, js, `String(r.limited)`); got != "1" {
		t.Errorf("getCallSites(1) returned %s frames, want 1", got)
	}
}

// util.parseArgs used to ignore tokens:true. With it set, the result must also
// carry a `tokens` array in Node's shape (option/positional/option-terminator).
func TestParseArgsTokens(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		const { values, positionals, tokens } = util.parseArgs({
			args: ["--name=foo", "-v", "pos1", "--", "after"],
			options: { name: { type: "string" }, verbose: { type: "boolean", short: "v" } },
			allowPositionals: true,
			tokens: true,
		});
		r.values = JSON.stringify(values);
		r.positionals = positionals.join(",");
		r.kinds = tokens.map(t => t.kind).join(",");
		const nameTok = tokens.find(t => t.name === "name");
		r.nameInline = nameTok.inlineValue;
		r.nameValue = nameTok.value;
		r.nameRaw = nameTok.rawName;
		const vTok = tokens.find(t => t.name === "verbose");
		r.vValue = String(vTok.value);
		r.vInline = String(vTok.inlineValue);
		const term = tokens.find(t => t.kind === "option-terminator");
		r.termIndex = term.index;
		const posTok = tokens.filter(t => t.kind === "positional").map(t => t.value).join(",");
		r.posValues = posTok;

		// Without tokens:true, no tokens key is present.
		const plain = util.parseArgs({ args: [], options: {} });
		r.noTokens = ("tokens" in plain);
	`)
	for expr, want := range map[string]string{
		"r.values":      `{"name":"foo","verbose":true}`,
		"r.positionals": "pos1,after",
		"r.kinds":       "option,option,positional,option-terminator,positional",
		"r.nameInline":  "true",
		"r.nameValue":   "foo",
		"r.nameRaw":     "--name",
		"r.vValue":      "undefined",
		"r.vInline":     "undefined",
		"r.termIndex":   "3",
		"r.posValues":   "pos1,after",
		"r.noTokens":    "false",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// util.deprecate used to be the identity function. It must now return a wrapper
// that calls through and emits a one-time DeprecationWarning via emitWarning.
func TestUtilDeprecateEmitsOnce(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = { warnings: [] };
		process.on("warning", (w) => r.warnings.push(w.name + ":" + w.message + ":" + (w.code || "")));
		const add = util.deprecate((a, b) => a + b, "add() is deprecated, use plus()", "DEP0001");
		r.call1 = add(2, 3);
		r.call2 = add(10, 20);
		r.call3 = add(1, 1);
	`)
	// The warning is emitted on nextTick; runScript drains it.
	for expr, want := range map[string]string{
		"r.call1": "5",
		"r.call2": "30",
		"r.call3": "2",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
	if got := evalStr(t, js, `r.warnings.length`); got != "1" {
		t.Errorf("deprecation warning count = %s, want 1 (emitted once)", got)
	}
	if got := evalStr(t, js, `r.warnings[0]`); got != "DeprecationWarning:add() is deprecated, use plus():DEP0001" {
		t.Errorf("warning = %q", got)
	}
}

// util.styleText must accept every style Node's util.inspect.colors lists.
// It THROWS on an unknown one, so a missing entry is not a cosmetic gap: it
// turns a library's colourized output into an exception. @babel/code-frame asks
// for "bgRed" while formatting a syntax error, and the TypeError replaced the
// error Babel was actually reporting — which is how this was found.
func TestStyleTextKnowsEveryNodeColor(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	runScript(t, rt, `
		const { styleText } = require("util");
		globalThis.r = { bad: [], sample: "" };
		const styles = [
			"reset", "bold", "dim", "italic", "underline", "blink", "inverse",
			"hidden", "strikethrough", "doubleunderline", "framed", "overlined",
			"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
			"gray", "grey", "blackBright", "redBright", "greenBright",
			"yellowBright", "blueBright", "magentaBright", "cyanBright",
			"whiteBright", "bgBlack", "bgRed", "bgGreen", "bgYellow", "bgBlue",
			"bgMagenta", "bgCyan", "bgWhite", "bgGray", "bgGrey", "bgBlackBright",
			"bgRedBright", "bgGreenBright", "bgYellowBright", "bgBlueBright",
			"bgMagentaBright", "bgCyanBright", "bgWhiteBright",
		];
		for (const s of styles) {
			try { styleText(s, "x"); } catch (e) { r.bad.push(s); }
		}
		r.sample = styleText(["bgRed", "white"], "x");
	`)
	if got := evalStr(t, js, `r.bad.join(",")`); got != "" {
		t.Errorf("styleText rejected: %s", got)
	}
	// The composed form wraps the text in both styles, innermost last.
	if got := evalStr(t, js, `JSON.stringify(r.sample)`); !strings.Contains(got, "41m") || !strings.Contains(got, "37m") {
		t.Errorf("styleText([\"bgRed\",\"white\"]) = %s, want both SGR codes", got)
	}
}

// util.inspect / console.log used to ignore Symbol.for('nodejs.util.inspect.custom'),
// so objects with a custom-inspect method logged as "{}". The hook must now be
// honored for both util.inspect and console.log formatting.
func TestUtilInspectCustomHook(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		const custom = Symbol.for("nodejs.util.inspect.custom");
		r.symbolExported = util.inspect.custom === custom;

		// String result is used verbatim.
		const obj = { secret: 42, [custom]() { return "CustomRepr"; } };
		r.stringResult = util.inspect(obj);

		// Non-string result is inspected in turn.
		const wrap = { [custom]() { return { shown: true }; } };
		r.nonString = util.inspect(wrap);

		// The hook receives (depth, opts, inspect).
		let sawArgs = null;
		const probe = { [custom](depth, opts, inspect) { sawArgs = [typeof depth, typeof opts, typeof inspect]; return "ok"; } };
		util.inspect(probe);
		r.args = (sawArgs || []).join(",");

		// A custom error still renders via its hook, not as {}.
		class MyErr extends Error { [custom]() { return "MyErr<custom>"; } }
		r.errInspect = util.inspect(new MyErr("x"));
	`)
	for expr, want := range map[string]string{
		"r.symbolExported": "true",
		"r.stringResult":   "CustomRepr",
		"r.nonString":      "{ shown: true }",
		"r.args":           "number,object,function",
		"r.errInspect":     "MyErr<custom>",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// console.log must route object formatting through the custom-inspect hook too.
func TestConsoleLogUsesCustomInspect(t *testing.T) {
	var stdout bytes.Buffer
	js, rt := newRuntime(t, spidermonkey.Config{Stdout: &stdout})
	_ = js
	runScript(t, rt, `
		const custom = Symbol.for("nodejs.util.inspect.custom");
		const logger = { level: "info", [custom]() { return "Pino<level=info>"; } };
		console.log(logger);
		console.log("prefix", logger);
	`)
	got := stdout.String()
	if want := "Pino<level=info>\nprefix Pino<level=info>\n"; got != want {
		t.Errorf("console.log output = %q, want %q", got, want)
	}
}
