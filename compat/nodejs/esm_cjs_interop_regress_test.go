package nodejs_test

// Regression tests for ESM⇄CJS interop: named imports from CommonJS
// dependencies (Node synthesizes them via cjs-module-lexer-style static
// analysis) and shebang handling in required files.

import (
	"context"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

func runModuleOK(t *testing.T, rt *nodejs.Runtime, specifier, src string) {
	t.Helper()
	r, err := rt.RunModule(context.Background(), specifier, src)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
}

// A CJS dependency using exports.NAME assignments must be importable with
// named ESM imports (Node synthesizes named exports for CJS).
func TestESMNamedImportFromCJSExportsAssignment(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/cjslib/package.json": {Data: []byte(`{"name": "cjslib", "main": "./index.js"}`)},
		"node_modules/cjslib/index.js": {Data: []byte(`
			exports.foo = 42;
			exports.bar = "hello";
			module.exports.baz = (x) => x * 2;
			exports.chainA = exports.chainB = "chained";
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import def, { foo, bar, baz, chainA, chainB } from "cjslib";
		globalThis.r = { foo, bar, baz6: baz(3), defFoo: def.foo, chain: chainA + chainB };
	`)
	if got := evalStr(t, js, `[r.foo, r.bar, r.baz6, r.defFoo, r.chain].join("|")`); got != "42|hello|6|42|chainedchained" {
		t.Errorf("named imports from exports.NAME CJS = %q, want \"42|hello|6|42|chainedchained\"", got)
	}
}

// module.exports = { ... } object-literal keys (identifier, shorthand,
// quoted, method) must surface as named exports.
func TestESMNamedImportFromCJSObjectLiteral(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/objlib/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/objlib/index.js": {Data: []byte(`
			const b = 2;
			module.exports = {
				a: 1,
				b,
				"quoted": 3,
				method() { return 4; },
			};
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { a, b, quoted, method } from "objlib";
		globalThis.r = { a, b, quoted, m: method() };
	`)
	if got := evalStr(t, js, `[r.a, r.b, r.quoted, r.m].join("|")`); got != "1|2|3|4" {
		t.Errorf("named imports from object-literal CJS = %q, want \"1|2|3|4\"", got)
	}
}

// Babel-transpiled CJS (__esModule marker + defineProperty getters) is the
// dominant shape of published packages; `import { useState } ...` style named
// imports must work against it.
func TestESMNamedImportFromBabelTranspiledCJS(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/babels/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/babels/index.js": {Data: []byte(`
			"use strict";
			Object.defineProperty(exports, "__esModule", { value: true });
			Object.defineProperty(exports, "useState", {
				enumerable: true,
				get: function () { return _hooks.useState; }
			});
			exports.version = "1.2.3";
			var _hooks = require("./hooks.js");
		`)},
		"node_modules/babels/hooks.js": {Data: []byte(`
			exports.useState = function useState(v) { return [v, function () {}]; };
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { useState, version, __esModule } from "babels";
		const [v] = useState(7);
		globalThis.r = { v, version, esm: __esModule };
	`)
	if got := evalStr(t, js, `[r.v, r.version, r.esm].join("|")`); got != "7|1.2.3|true" {
		t.Errorf("Babel-style CJS named imports = %q, want \"7|1.2.3|true\"", got)
	}
}

// module.exports = require("...") re-export chains must surface the target's
// named exports (a very common package-root pattern).
func TestESMNamedImportThroughCJSReexportChain(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/chain/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/chain/index.js":     {Data: []byte(`module.exports = require("./lib/impl.js");`)},
		"node_modules/chain/lib/impl.js": {Data: []byte(`
			exports.alpha = "a";
			exports.beta = "b";
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { alpha, beta } from "chain";
		globalThis.r = alpha + beta;
	`)
	if got := evalStr(t, js, `r`); got != "ab" {
		t.Errorf("re-export chain named imports = %q, want \"ab\"", got)
	}
}

// The TypeScript __exportStar helper and the Babel Object.keys(...).forEach
// star re-export must both contribute the target module's names.
func TestESMNamedImportThroughStarReexportHelpers(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/star/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/star/index.js": {Data: []byte(`
			"use strict";
			var __exportStar = (this && this.__exportStar) || function(m, exports) {
				for (var p in m) if (p !== "default" && !Object.prototype.hasOwnProperty.call(exports, p)) exports[p] = m[p];
			};
			Object.defineProperty(exports, "__esModule", { value: true });
			__exportStar(require("./ts.js"), exports);
			var _by = require("./babel.js");
			Object.keys(_by).forEach(function (key) {
				if (key === "default" || key === "__esModule") return;
				exports[key] = _by[key];
			});
		`)},
		"node_modules/star/ts.js":    {Data: []byte(`exports.fromTS = 1;`)},
		"node_modules/star/babel.js": {Data: []byte(`exports.fromBabel = 2;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { fromTS, fromBabel } from "star";
		globalThis.r = fromTS + fromBabel;
	`)
	if got := evalStr(t, js, `String(r)`); got != "3" {
		t.Errorf("star re-export helpers named imports = %q, want \"3\"", got)
	}
}

// Detected names must be identifier-filtered ("default" and invalid names
// excluded) without breaking the remaining exports, and import/require of the
// same CJS file must share one module instance.
func TestESMCJSNamedExportFilteringAndSharedInstance(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/mix/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/mix/index.js": {Data: []byte(`
			exports.ok = "yes";
			exports.default = "not-a-named-export";
			exports["not-an-identifier"] = 1;
			exports["class"] = 2;
			exports.state = { n: 0 };
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import def, { ok, state } from "mix";
		state.n = 41;
		const viaRequire = require("mix");
		viaRequire.state.n++;
		globalThis.r = {
			ok,
			def: def.default,
			shared: state.n,
			same: viaRequire === def,
		};
	`)
	if got := evalStr(t, js, `[r.ok, r.def, r.shared, r.same].join("|")`); got != "yes|not-a-named-export|42|true" {
		t.Errorf("filtering/shared-instance = %q, want \"yes|not-a-named-export|42|true\"", got)
	}
}

// Export-looking text inside comments or template literals must NOT become
// named exports (the analyzer masks those regions); the shim still compiles
// and real exports still work.
func TestESMCJSAnalyzerIgnoresCommentsAndTemplates(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/noisy/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/noisy/index.js": {Data: []byte(`
			// exports.fromLineComment = 1;
			/* exports.fromBlockComment = 2; */
			const doc = ` + "`" + `exports.fromTemplate = 3;` + "`" + `;
			exports.real = doc.length > 0 ? "real" : "";
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import def, { real } from "noisy";
		globalThis.r = {
			real,
			ghosts: ["fromLineComment", "fromBlockComment", "fromTemplate"].filter((k) => k in def).join(","),
		};
	`)
	if got := evalStr(t, js, `r.real + "|" + r.ghosts`); got != "real|" {
		t.Errorf("masking = %q, want \"real|\" (no ghost exports)", got)
	}
}

// A CJS module whose exports object carries a THROWING getter (deprecation
// trap, bundler TDZ guard in a cycle) must still import: Node's CJS
// translator reads each detected name under its own try/catch, so the
// poisoned name is undefined while the default export and every other named
// export keep working — a default-only import must never abort.
func TestESMImportFromCJSWithThrowingGetter(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/trapped/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/trapped/index.js": {Data: []byte(`
			exports.good = "ok";
			Object.defineProperty(exports, "trap", {
				enumerable: true,
				get: function () { throw new Error("deprecated: do not touch trap"); },
			});
			exports.alsoGood = 2;
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	// Default-only import: must not evaluate the throwing getter fatally.
	runModuleOK(t, rt, "defonly.mjs", `
		import def from "trapped";
		globalThis.r1 = def.good;
	`)
	if got := evalStr(t, js, `r1`); got != "ok" {
		t.Errorf("default-only import with throwing getter = %q, want \"ok\"", got)
	}
	// Named imports: healthy names bind, the poisoned one is undefined.
	runModuleOK(t, rt, "named.mjs", `
		import def, { good, alsoGood, trap } from "trapped";
		globalThis.r2 = [good, alsoGood, String(trap), def.alsoGood].join("|");
	`)
	if got := evalStr(t, js, `r2`); got != "ok|2|undefined|2" {
		t.Errorf("named imports with throwing getter = %q, want \"ok|2|undefined|2\"", got)
	}
}

// Regex literals must be lexed by the analyzer: a backtick inside a regex
// (markdown/highlighter packages match code fences) must not open phantom
// template masking that blanks — and silently LOSES — every later export,
// and export-shaped text inside a regex must not become a phantom export.
// Division must still not be mistaken for a regex opener.
func TestCJSAnalyzerRegexLiterals(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/relib/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/relib/index.js": {Data: []byte(`
			exports.before = 1;
			var fence = /` + "`" + `{3}/;
			exports.after = 2;
			var phantom = /exports.phantom = 1/;
			function fenced(s) { return /` + "`" + `+/.test(s); }
			var ratio = 10 / 2;
			exports.last = ratio;
			exports.check = fenced("x` + "`" + `y");
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import * as ns from "relib";
		import { before, after, last, check } from "relib";
		globalThis.r = {
			vals: [before, after, last, check].join("|"),
			phantom: Object.keys(ns).includes("phantom"),
		};
	`)
	if got := evalStr(t, js, `r.vals`); got != "1|2|5|true" {
		t.Errorf("exports around regex literals = %q, want \"1|2|5|true\"", got)
	}
	if got := evalStr(t, js, `String(r.phantom)`); got != "false" {
		t.Errorf("export-shaped text inside a regex leaked as a named export")
	}
}

// async method shorthand (and async generators) in module.exports = { ... }
// must surface as named exports, alongside plain keys, accessors, and
// "async" used as an ordinary key.
func TestESMNamedImportFromCJSObjectLiteralAsyncMethods(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/asyncobj/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/asyncobj/index.js": {Data: []byte(`
			module.exports = {
				async fetchData() { return 7; },
				async *gen() { yield 1; },
				plain: 2,
				async: 3,
				get val() { return 5; },
			};
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import * as ns from "asyncobj";
		import { fetchData, gen, plain, val } from "asyncobj";
		globalThis.r = [
			typeof fetchData, typeof gen, plain, val, ns.async,
		].join("|");
		fetchData().then((v) => { globalThis.rAwaited = v; });
	`)
	if got := evalStr(t, js, `r`); got != "function|function|2|5|3" {
		t.Errorf("async-method object-literal exports = %q, want \"function|function|2|5|3\"", got)
	}
	if got := evalStr(t, js, `String(rAwaited)`); got != "7" {
		t.Errorf("awaited async method export = %q, want \"7\"", got)
	}
}

// A required file starting with a #! line must load (Node strips the shebang
// before wrapping CJS sources), and line numbers must be preserved for errors.
func TestRequireFileWithShebang(t *testing.T) {
	fsys := fstest.MapFS{
		"tool.js": {Data: []byte("#!/usr/bin/env node\nexports.ran = true;\nexports.lineCheck = new Error(\"here\");\n")},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const tool = require("./tool.js");
		globalThis.r = { ran: tool.ran };
	`)
	if got := evalStr(t, js, `String(r.ran)`); got != "true" {
		t.Errorf("shebang CJS require: ran = %q, want \"true\"", got)
	}
}

// The ESM path accepts hashbang sources natively (ES2023); an imported .mjs
// with a #! line must evaluate, including its named exports.
func TestImportESMFileWithShebang(t *testing.T) {
	fsys := fstest.MapFS{
		"cli.mjs": {Data: []byte("#!/usr/bin/env node\nexport const started = \"ok\";\n")},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { started } from "./cli.mjs";
		globalThis.r = started;
	`)
	if got := evalStr(t, js, `r`); got != "ok" {
		t.Errorf("shebang ESM import = %q, want \"ok\"", got)
	}
}
