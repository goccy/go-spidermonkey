package nodejs_test

import (
	"bytes"
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestConsoleExtended(t *testing.T) {
	var stdout bytes.Buffer
	_, rt := newRuntime(t, spidermonkey.Config{Stdout: &stdout})

	runScript(t, rt, `
		console.group("outer");
		console.log("inside");
		console.groupEnd();
		console.count("x");
		console.count("x");
		console.countReset("x");
		console.count("x");
		console.table([{ a: 1, b: 2 }, { a: 3, b: 4 }]);
	`)
	out := stdout.String()
	if !strings.Contains(out, "outer\n  inside") {
		t.Errorf("group indent missing: %q", out)
	}
	if !strings.Contains(out, "x: 1") || !strings.Contains(out, "x: 2") {
		t.Errorf("count output missing: %q", out)
	}
	if !strings.Contains(out, "(index)") || !strings.Contains(out, "a | b") {
		t.Errorf("table header missing: %q", out)
	}
}

func TestEventsExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const EventEmitter = require("events");
		globalThis.r = {};

		// on() async iterator
		const em = new EventEmitter();
		(async () => {
			const collected = [];
			const iter = EventEmitter.on(em, "tick");
			(async () => {
				for await (const [n] of iter) {
					collected.push(n);
					if (collected.length === 3) { iter.return(); }
				}
			})();
			// Emit after the iterator is set up.
			await Promise.resolve();
			em.emit("tick", 1);
			em.emit("tick", 2);
			em.emit("tick", 3);
			await new Promise((res) => setTimeout(res, 20));
			r.collected = collected.join(",");
		})();

		// statics
		const em2 = new EventEmitter();
		const listener = () => {};
		em2.on("evt", listener);
		r.count = EventEmitter.listenerCount(em2, "evt");
		r.listeners = EventEmitter.getEventListeners(em2, "evt").length;
		r.errorMonitor = typeof EventEmitter.errorMonitor;
	`)
	if got := evalStr(t, js, `r.collected`); got != "1,2,3" {
		t.Errorf("events.on async iterator = %q", got)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "1" {
		t.Errorf("listenerCount = %q", got)
	}
	if got := evalStr(t, js, `r.errorMonitor`); got != "symbol" {
		t.Errorf("errorMonitor = %q", got)
	}
}

func TestUtilExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};

		// parseArgs
		const { values, positionals } = util.parseArgs({
			args: ["--name", "alice", "--verbose", "-c", "5", "file.txt"],
			options: {
				name: { type: "string" },
				verbose: { type: "boolean" },
				count: { type: "string", short: "c" },
			},
			allowPositionals: true,
		});
		r.name = values.name;
		r.verbose = values.verbose;
		r.count = values.count;
		r.positional = positionals.join(",");

		// styleText
		r.styled = util.styleText("red", "x");

		// stripVTControlCharacters
		r.stripped = util.stripVTControlCharacters("\x1b[31mred\x1b[39m");

		// MIMEType
		const mt = new util.MIMEType("text/html; charset=utf-8");
		r.mime = mt.type + "/" + mt.subtype + " " + mt.params.get("charset");
	`)
	for expr, want := range map[string]string{
		"r.name":       "alice",
		"r.verbose":    "true",
		"r.count":      "5",
		"r.positional": "file.txt",
		"r.styled":     "\x1b[31mx\x1b[39m",
		"r.stripped":   "red",
		"r.mime":       "text/html utf-8",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestAliasModules(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.r = {};
		r.assertStrict = typeof require("assert/strict").strictEqual;
		r.pathPosix = require("path/posix").sep;
		r.sys = require("sys") === require("util");
		const consumers = require("stream/consumers");
		r.hasConsumers = typeof consumers.text === "function" && typeof consumers.json === "function";
	`)
	for expr, want := range map[string]string{
		"r.assertStrict": "function",
		"r.pathPosix":    "/",
		"r.sys":          "true",
		"r.hasConsumers": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestStreamConsumers(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { Readable } = require("stream");
		const consumers = require("stream/consumers");
		globalThis.r = {};
		(async () => {
			r.text = await consumers.text(Readable.from(["hello ", "world"]));
			r.json = JSON.stringify(await consumers.json(Readable.from(['{"a":', '1}'])));
			const buf = await consumers.buffer(Readable.from([Buffer.from("ab"), Buffer.from("cd")]));
			r.buffer = buf.toString();
		})().catch((e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("consumers rejected: %s", got)
	}
	for expr, want := range map[string]string{
		"r.text":   "hello world",
		"r.json":   `{"a":1}`,
		"r.buffer": "abcd",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestOSExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const os = require("os");
		globalThis.r = {};
		r.hasConstants = typeof os.constants.signals.SIGINT === "number";
		r.devNull = os.devNull;
		r.eol = JSON.stringify(os.EOL);
		r.userInfo = os.userInfo().username;
	`)
	if got := evalStr(t, js, `r.hasConstants`); got != "true" {
		t.Error("os.constants.signals missing")
	}
	if got := evalStr(t, js, `r.devNull`); got != "/dev/null" {
		t.Errorf("os.devNull = %q", got)
	}
	if got := evalStr(t, js, `r.eol`); got != `"\n"` {
		t.Errorf("os.EOL = %q", got)
	}
}

func TestProcessExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.builtinModule = typeof process.getBuiltinModule("path").join;
		process.ref(); process.unref(); // must not throw
		r.hrtime = Array.isArray(process.hrtime());
		r.hrtimeBig = typeof process.hrtime.bigint() === "bigint";
	`)
	if got := evalStr(t, js, `r.builtinModule`); got != "function" {
		t.Errorf("process.getBuiltinModule = %q", got)
	}
	if got := evalStr(t, js, `r.hrtime`); got != "true" {
		t.Error("process.hrtime not array")
	}
	if got := evalStr(t, js, `r.hrtimeBig`); got != "true" {
		t.Error("process.hrtime.bigint not bigint")
	}
}

func TestVMBasic(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const vm = require("vm");
		globalThis.r = {};
		r.result = vm.runInThisContext("1 + 2 + 3");
		const fn = vm.compileFunction("return a * b", ["a", "b"]);
		r.fn = fn(6, 7);
	`)
	if got := evalStr(t, js, `String(r.result)`); got != "6" {
		t.Errorf("vm.runInThisContext = %q", got)
	}
	if got := evalStr(t, js, `String(r.fn)`); got != "42" {
		t.Errorf("vm.compileFunction = %q", got)
	}
	_ = spidermonkey.Undefined
}

// TestESMJSONImportIsInert verifies a .json module imported through the ESM
// loader is parsed as data (JSON.parse), not evaluated as JavaScript, so an
// executable payload in the file cannot run on import.
func TestESMJSONImportIsInert(t *testing.T) {
	fsys := fstest.MapFS{
		"side.json": {Data: []byte(`(globalThis.__PWNED = 1, {"x": 2})`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	// side.json's bytes form a JS expression with a side effect. Importing it must
	// NOT run that side effect: JSON.parse rejects the non-JSON and the import
	// throws, and __PWNED is never set.
	_, err := rt.RunModule(context.Background(), "main.mjs", `
		globalThis.r = {};
		try {
			await import("./side.json");
			r.imported = true;
		} catch (e) {
			r.threw = true;
		}
		r.pwned = globalThis.__PWNED ?? "unset";
	`)
	if err != nil {
		t.Fatalf("RunModule error: %v", err)
	}
	if got := evalStr(t, js, "String(r.pwned)"); got != "unset" {
		t.Fatalf("ESM .json import executed its payload: __PWNED = %q", got)
	}
	if evalVal(t, js, "!!r.imported").Bool() {
		t.Error("importing non-JSON .json succeeded; expected a parse error")
	}
}

// TestPackageExportsWildcard verifies a package.json "exports" subpath pattern
// ("./*": "./src/*.js") resolves a subpath import by substituting the captured
// segment — the shape used by @noble/hashes, date-fns and many modern packages.
// Without pattern support the exact-key lookup throws MODULE_NOT_FOUND.
func TestPackageExportsWildcard(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("node_modules/wild/package.json", `{"name":"wild","exports":{".":"./index.js","./*":"./src/*.js"}}`)
	write("node_modules/wild/index.js", `module.exports = { root: true };`)
	write("node_modules/wild/src/greet.js", `module.exports = "hello-pattern";`)
	write("node_modules/wild/src/util/pad.js", `module.exports = (s) => "[" + s + "]";`)
	write("secret.js", `module.exports = "SECRET";`) // sits at FS root, outside the package

	js, rt := newRuntime(t, spidermonkey.Config{FS: os.DirFS(dir)})
	runScript(t, rt, `
		globalThis.r = {};
		r.greet = require("wild/greet");             // ./* -> ./src/greet.js
		r.padded = require("wild/util/pad")("x");     // nested capture -> ./src/util/pad.js
		r.root = require("wild").root;                // exact "." entry still works
	`)
	if got := evalStr(t, js, `r.greet`); got != "hello-pattern" {
		t.Errorf("require(wild/greet) = %q, want hello-pattern (exports * pattern)", got)
	}
	if got := evalStr(t, js, `r.padded`); got != "[x]" {
		t.Errorf("require(wild/util/pad) = %q, want [x] (nested * capture)", got)
	}
	if got := evalStr(t, js, `String(r.root)`); got != "true" {
		t.Errorf("require(wild) exact export = %q, want true", got)
	}
	// A "*" capture with ../ must NOT escape the package (path-traversal guard):
	// "./src/*.js" with *="../../../secret" would otherwise path.Join to the root
	// secret.js. Without the guard this loads "SECRET"; with it, it is blocked.
	runScript(t, rt, `
		try { globalThis.__leak = require("wild/../../../secret"); }
		catch (e) { globalThis.__leak = "blocked"; }
	`)
	if got := evalStr(t, js, `String(__leak)`); got != "blocked" {
		t.Errorf("exports * pattern allowed path traversal: %q, want blocked", got)
	}
}

// Classifying a .js file that no package.json "type" covers is the engine's
// job, not a regex's. The engine compiles the source both ways — as a module
// and inside the CommonJS wrapper — and reports which one it parses as, which
// is Node's own rule. A syntax sniff cannot match it: a line-anchored
// import/export match misses a minified single-line bundle, and no regex can
// tell those keywords from the same words inside a comment or a string.

// A minified ESM bundle is one line, so nothing is at the start of a line but
// the first statement. It is still an ES module and must load as one.
func TestMinifiedSingleLineBundleIsESM(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/minified/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		// No "type" field, no newline before `export`: the sniff says CJS.
		"node_modules/minified/index.js": {Data: []byte(`const a=1;const b=2;export const sum=a+b;export default sum;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import def, { sum } from "minified";
		globalThis.r = [sum, def].join("|");
	`)
	if got := evalStr(t, js, `r`); got != "3|3" {
		t.Errorf("minified ESM bundle: %q, want %q", got, "3|3")
	}
}

// A CommonJS file whose comments and strings contain the word `export` at the
// start of a line is still CommonJS. The sniff calls it ESM and the file then
// fails to load at all.
func TestCJSMentioningExportInTextIsStillCJS(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/talky/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/talky/index.js": {Data: []byte(`
// export const notReal = 1;
const doc = ` + "`" + `
export default alsoNotReal;
` + "`" + `;
exports.value = "cjs";
exports.docLength = doc.length;
`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import mod from "talky";
		globalThis.r = mod.value;
	`)
	if got := evalStr(t, js, `r`); got != "cjs" {
		t.Errorf("CJS file mentioning export in a comment/string: %q, want %q", got, "cjs")
	}
}

// require() resolves through the same classifier, so its kind must agree.
func TestRequireAgreesWithEngineClassification(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/talky/package.json": {Data: []byte(`{"main": "./index.js"}`)},
		"node_modules/talky/index.js":     {Data: []byte("const doc = `\nexport default notReal;\n`;\nexports.value = \"cjs\";\n")},
	}
	js, _ := newRuntime(t, spidermonkey.Config{FS: fsys})
	if got := evalStr(t, js, `require("talky").value`); got != "cjs" {
		t.Errorf("require of a CJS file mentioning export: %q, want %q", got, "cjs")
	}
}

// An explicit package.json "type" is authoritative and must never be
// second-guessed by the engine: "type":"commonjs" over a file that WOULD parse
// as a module stays CommonJS, which is exactly how Node reports the error.
func TestDeclaredTypeStillWins(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/declared/package.json": {Data: []byte(`{"main": "./index.js", "type": "commonjs"}`)},
		"node_modules/declared/index.js":     {Data: []byte("exports.value = \"declared-cjs\";\n")},
	}
	js, _ := newRuntime(t, spidermonkey.Config{FS: fsys})
	if got := evalStr(t, js, `require("declared").value`); got != "declared-cjs" {
		t.Errorf(`"type":"commonjs" package: %q, want %q`, got, "declared-cjs")
	}
}

// Regression tests for Node module-resolution semantics: '.'/'..' relative
// classification, package self-reference by name, and export-condition
// matching by object key order.

// require('.') and require('..') resolve relative to the requiring module's
// directory (Node classifies '.', './', '..', '../' all as relative), NOT
// against the FS root or as bare specifiers.
func TestRequireDotResolvesRelativeToModuleDir(t *testing.T) {
	fsys := fstest.MapFS{
		// A decoy at the FS root: the old behavior resolved '.' here.
		"index.js":         {Data: []byte(`module.exports = "ROOT-DECOY";`)},
		"lib/index.js":     {Data: []byte(`module.exports = "lib-index";`)},
		"lib/pkg/index.js": {Data: []byte(`module.exports = "pkg-index";`)},
		"lib/pkg/entry.js": {Data: []byte(`module.exports = { self: require("."), parent: require("..") };`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const e = require("./lib/pkg/entry.js");
		globalThis.r = e.self + "|" + e.parent;
	`)
	if got := evalStr(t, js, `r`); got != "pkg-index|lib-index" {
		t.Errorf("require('.')/require('..') = %q, want \"pkg-index|lib-index\"", got)
	}
}

// import('.') from an ES module follows the same relative classification.
func TestImportDotResolvesRelativeToModuleDir(t *testing.T) {
	fsys := fstest.MapFS{
		"index.js":      {Data: []byte(`module.exports = "ROOT-DECOY";`)},
		"lib/index.js":  {Data: []byte(`module.exports = "lib-index";`)},
		"lib/entry.mjs": {Data: []byte(`const m = await import("."); export const got = m.default;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { got } from "./lib/entry.mjs";
		globalThis.r = got;
	`)
	if got := evalStr(t, js, `r`); got != "lib-index" {
		t.Errorf("import('.') = %q, want \"lib-index\"", got)
	}
}

// Package self-reference (Node v12.16+): inside the package whose nearest
// package.json declares name "myapp" AND an "exports" field, require/import
// of "myapp" or "myapp/subpath" resolves through those exports.
func TestPackageSelfReferenceRequire(t *testing.T) {
	fsys := fstest.MapFS{
		"app/package.json": {Data: []byte(`{
			"name": "myapp",
			"exports": { ".": "./main.js", "./util": "./lib/util.js" }
		}`)},
		"app/main.js":     {Data: []byte(`exports.tag = "myapp-root";`)},
		"app/lib/util.js": {Data: []byte(`exports.double = (x) => x * 2;`)},
		"app/src/feature.js": {Data: []byte(`
			const util = require("myapp/util");
			const root = require("myapp");
			module.exports = root.tag + ":" + util.double(21);
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `globalThis.r = require("./app/src/feature.js");`)
	if got := evalStr(t, js, `r`); got != "myapp-root:42" {
		t.Errorf("self-reference require = %q, want \"myapp-root:42\"", got)
	}
}

func TestPackageSelfReferenceImport(t *testing.T) {
	fsys := fstest.MapFS{
		"app/package.json": {Data: []byte(`{
			"name": "myapp",
			"type": "module",
			"exports": { "./util": "./lib/util.mjs" }
		}`)},
		"app/lib/util.mjs": {Data: []byte(`export const triple = (x) => x * 3;`)},
		"app/src/feature.mjs": {Data: []byte(`
			import { triple } from "myapp/util";
			export const got = triple(5);
		`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { got } from "./app/src/feature.mjs";
		globalThis.r = String(got);
	`)
	if got := evalStr(t, js, `r`); got != "15" {
		t.Errorf("self-reference import = %q, want \"15\"", got)
	}
}

// Self-reference must NOT hijack a bare specifier when the nearest
// package.json's name differs — the node_modules walk-up still applies.
func TestPackageSelfReferenceNameMismatchFallsThrough(t *testing.T) {
	fsys := fstest.MapFS{
		"app/package.json":              {Data: []byte(`{"name": "notdep", "exports": "./main.js"}`)},
		"app/main.js":                   {Data: []byte(`exports.tag = "self";`)},
		"app/feature.js":                {Data: []byte(`module.exports = require("dep").tag;`)},
		"node_modules/dep/package.json": {Data: []byte(`{"name": "dep", "main": "./index.js"}`)},
		"node_modules/dep/index.js":     {Data: []byte(`exports.tag = "from-node-modules";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `globalThis.r = require("./app/feature.js");`)
	if got := evalStr(t, js, `r`); got != "from-node-modules" {
		t.Errorf("name-mismatch fallthrough = %q, want \"from-node-modules\"", got)
	}
}

// Node matches export conditions by the CONDITION OBJECT'S KEY ORDER against
// the enabled-conditions set — not by a fixed engine-side preference list.
// With both "node" and "import" enabled, whichever the package lists first
// wins.
func TestExportConditionsMatchInKeyOrderESM(t *testing.T) {
	fsys := fstest.MapFS{
		// import listed before node: import must win.
		"node_modules/first/package.json": {Data: []byte(`{
			"exports": { ".": { "import": "./import.mjs", "node": "./node.mjs" } }
		}`)},
		"node_modules/first/import.mjs": {Data: []byte(`export const which = "import";`)},
		"node_modules/first/node.mjs":   {Data: []byte(`export const which = "node";`)},
		// node listed before import: node must win.
		"node_modules/second/package.json": {Data: []byte(`{
			"exports": { ".": { "node": "./node.mjs", "import": "./import.mjs" } }
		}`)},
		"node_modules/second/import.mjs": {Data: []byte(`export const which = "import";`)},
		"node_modules/second/node.mjs":   {Data: []byte(`export const which = "node";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { which as a } from "first";
		import { which as b } from "second";
		globalThis.r = a + "|" + b;
	`)
	if got := evalStr(t, js, `r`); got != "import|node" {
		t.Errorf("ESM condition key-order = %q, want \"import|node\"", got)
	}
}

// The full runtime must NOT take a package's browser build: a browser bundle is
// not an equivalent spelling of the node one (@babel/core's cannot load plugins
// at all), and every node API it avoids is present here.
func TestFullRuntimePrefersNodeOverBrowserBuild(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/dual/package.json": {Data: []byte(`{
			"exports": { ".": { "browser": "./browser.mjs", "node": "./node.mjs", "default": "./default.mjs" } }
		}`)},
		"node_modules/dual/browser.mjs": {Data: []byte(`export const which = "browser";`)},
		"node_modules/dual/node.mjs":    {Data: []byte(`export const which = "node";`)},
		"node_modules/dual/default.mjs": {Data: []byte(`export const which = "default";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "main.mjs", `
		import { which } from "dual";
		globalThis.r = which;
	`)
	if got := evalStr(t, js, `r`); got != "node" {
		t.Errorf("dual-build package resolved to %q, want \"node\"", got)
	}
}

func TestExportConditionsMatchInKeyOrderCJS(t *testing.T) {
	fsys := fstest.MapFS{
		// node listed before require: node must win for require().
		"node_modules/ncond/package.json": {Data: []byte(`{
			"exports": { ".": { "node": "./node.cjs", "require": "./require.cjs" } }
		}`)},
		"node_modules/ncond/node.cjs":    {Data: []byte(`module.exports = "node";`)},
		"node_modules/ncond/require.cjs": {Data: []byte(`module.exports = "require";`)},
		// require listed before node: require must win.
		"node_modules/rcond/package.json": {Data: []byte(`{
			"exports": { ".": { "require": "./require.cjs", "node": "./node.cjs" } }
		}`)},
		"node_modules/rcond/node.cjs":    {Data: []byte(`module.exports = "node";`)},
		"node_modules/rcond/require.cjs": {Data: []byte(`module.exports = "require";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `globalThis.r = require("ncond") + "|" + require("rcond");`)
	if got := evalStr(t, js, `r`); got != "node|require" {
		t.Errorf("CJS condition key-order = %q, want \"node|require\"", got)
	}
}

// Disabled conditions are skipped regardless of position, nested condition
// objects recurse with the same key-order rule, and "default" matches when
// reached.
func TestExportConditionsSkipDisabledAndRecurse(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/nest/package.json": {Data: []byte(`{
			"exports": {
				".": {
					"react-server": "./rsc.cjs",
					"node": { "development": "./dev.cjs", "default": "./prod.cjs" },
					"default": "./fallback.cjs"
				}
			}
		}`)},
		"node_modules/nest/rsc.cjs":      {Data: []byte(`module.exports = "rsc";`)},
		"node_modules/nest/dev.cjs":      {Data: []byte(`module.exports = "dev";`)},
		"node_modules/nest/prod.cjs":     {Data: []byte(`module.exports = "prod";`)},
		"node_modules/nest/fallback.cjs": {Data: []byte(`module.exports = "fallback";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	// react-server is not enabled → skipped; node matches → recurse;
	// development is not enabled → skipped; nested default wins.
	runScript(t, rt, `globalThis.r = require("nest");`)
	if got := evalStr(t, js, `r`); got != "prod" {
		t.Errorf("nested condition resolution = %q, want \"prod\"", got)
	}
}

// The standalone ESMLoader is the OTHER half of the condition split: a web-only
// embedding (compat/web, no node core modules) must take the browser/Workers
// build, because a package's node build would import node:buffer and fail.
// jose is the package this is measured on; the fixture reproduces its shape.
func TestStandaloneESMLoaderPrefersBrowserBuild(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/dual/package.json": {Data: []byte(`{
			"exports": { ".": { "browser": "./browser.mjs", "node": "./node.mjs" } }
		}`)},
		"node_modules/dual/browser.mjs": {Data: []byte(`export const which = "browser";`)},
		"node_modules/dual/node.mjs":    {Data: []byte(`import "node:buffer"; export const which = "node";`)},
	}
	js, err := spidermonkey.New(spidermonkey.Config{FS: fsys})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	js.SetModuleLoader(nodejs.ESMLoader)
	r, err := js.EvalModule(context.Background(), "main.mjs", `
		import { which } from "dual";
		globalThis.r = which;
	`)
	if err != nil {
		t.Fatalf("EvalModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	v, err := js.Eval(context.Background(), "globalThis.r")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Value.String(); got != "browser" {
		t.Errorf("web-only loader resolved to %q, want \"browser\"", got)
	}
}

// Regression tests for ESM⇄CJS interop: named imports from CommonJS
// dependencies (Node synthesizes them via cjs-module-lexer-style static
// analysis) and shebang handling in required files.

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

// Regression tests for import.meta.url: modules register under canonical
// file:// URLs so import.meta.url is a real URL (new URL("./x",
// import.meta.url) works, as in Node) in the entry module AND in every
// imported module, while relative imports and require interop keep working.

func TestImportMetaURLIsFileURL(t *testing.T) {
	fsys := fstest.MapFS{
		"app/src/dep.mjs": {Data: []byte(`export const depUrl = import.meta.url;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/app/src/main.mjs", `
		import { depUrl } from "./dep.mjs";
		globalThis.r = {
			url: import.meta.url,
			joined: new URL("./data.txt", import.meta.url).pathname,
			up: new URL("../shared/cfg.json", import.meta.url).pathname,
			depUrl,
		};
	`)
	if got := evalStr(t, js, `r.url`); got != "file:///app/src/main.mjs" {
		t.Errorf("entry import.meta.url = %q, want \"file:///app/src/main.mjs\"", got)
	}
	if got := evalStr(t, js, `r.joined`); got != "/app/src/data.txt" {
		t.Errorf("new URL('./data.txt', import.meta.url).pathname = %q, want \"/app/src/data.txt\"", got)
	}
	if got := evalStr(t, js, `r.up`); got != "/app/shared/cfg.json" {
		t.Errorf("new URL('../shared/cfg.json', import.meta.url).pathname = %q, want \"/app/shared/cfg.json\"", got)
	}
	if got := evalStr(t, js, `r.depUrl`); got != "file:///app/src/dep.mjs" {
		t.Errorf("imported module's import.meta.url = %q, want \"file:///app/src/dep.mjs\"", got)
	}
}

// A bare entry specifier ("main.mjs") registers at the FS root.
func TestImportMetaURLBareEntrySpecifier(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	runModuleOK(t, rt, "main.mjs", `globalThis.r = import.meta.url;`)
	if got := evalStr(t, js, `r`); got != "file:///main.mjs" {
		t.Errorf("import.meta.url = %q, want \"file:///main.mjs\"", got)
	}
}

// Two import spellings of the same file (relative path vs canonical file://
// URL) must dedupe to ONE module instance.
func TestImportPathAndFileURLShareOneInstance(t *testing.T) {
	fsys := fstest.MapFS{
		"app/state.mjs": {Data: []byte(`export const box = { n: 0 };`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/app/main.mjs", `
		import { box as a } from "./state.mjs";
		import { box as b } from "file:///app/state.mjs";
		a.n = 41;
		b.n++;
		globalThis.r = String(a.n) + "|" + String(a === b);
	`)
	if got := evalStr(t, js, `r`); got != "42|true" {
		t.Errorf("path vs file URL dedupe = %q, want \"42|true\"", got)
	}
}

// createRequire(import.meta.url) — the canonical Node idiom — must produce a
// require that resolves relative to the module and shares the CJS cache with
// the ESM⇄CJS interop path.
func TestCreateRequireFromImportMetaURL(t *testing.T) {
	fsys := fstest.MapFS{
		"app/sibling.cjs": {Data: []byte(`exports.counter = { n: 0 };`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/app/main.mjs", `
		import { createRequire } from "node:module";
		import { counter } from "./sibling.cjs";
		const require = createRequire(import.meta.url);
		const viaRequire = require("./sibling.cjs");
		counter.n = 41;
		viaRequire.counter.n++;
		globalThis.r = String(counter.n) + "|" + String(viaRequire.counter === counter);
	`)
	if got := evalStr(t, js, `r`); got != "42|true" {
		t.Errorf("createRequire(import.meta.url) = %q, want \"42|true\"", got)
	}
}

// A module under a directory with a SPACE in its name must get a VALID
// percent-encoded import.meta.url ("file:///my%20dir/main.mjs"), which
// new URL(relative, import.meta.url) can join against and dynamic import can
// load — and the dynamically imported encoded URL must be the SAME instance
// as the statically imported relative specifier.
func TestImportMetaURLPercentEncodesSpaces(t *testing.T) {
	fsys := fstest.MapFS{
		"my dir/peer.mjs": {Data: []byte(`export const box = { n: 0 }; export const peerUrl = import.meta.url;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/my dir/main.mjs", `
		import { box, peerUrl } from "./peer.mjs";
		globalThis.r = {
			url: import.meta.url,
			peerUrl,
			joined: new URL("./peer.mjs", import.meta.url).href,
		};
		box.n = 41;
		import(new URL("./peer.mjs", import.meta.url).href).then((m) => {
			m.box.n++;
			globalThis.rDyn = String(m.box.n) + "|" + String(m.box === box);
		});
	`)
	if got := evalStr(t, js, `r.url`); got != "file:///my%20dir/main.mjs" {
		t.Errorf("import.meta.url = %q, want \"file:///my%%20dir/main.mjs\"", got)
	}
	if got := evalStr(t, js, `r.peerUrl`); got != "file:///my%20dir/peer.mjs" {
		t.Errorf("peer import.meta.url = %q, want \"file:///my%%20dir/peer.mjs\"", got)
	}
	if got := evalStr(t, js, `r.joined`); got != "file:///my%20dir/peer.mjs" {
		t.Errorf("new URL('./peer.mjs', import.meta.url) = %q, want \"file:///my%%20dir/peer.mjs\"", got)
	}
	if got := evalStr(t, js, `rDyn`); got != "42|true" {
		t.Errorf("dynamic import of encoded URL = %q, want \"42|true\" (one shared instance)", got)
	}
}

// The percent-ENCODED and raw spellings of one file must resolve to a single
// module instance (canonicalization keys the registry): a raw-space file URL
// funnels into the canonical encoded registration instead of double-
// evaluating the module.
func TestEncodedAndRawFileURLSpellingsShareOneInstance(t *testing.T) {
	fsys := fstest.MapFS{
		"my dir/state.mjs": {Data: []byte(`export const box = { n: 0 };`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/my dir/main.mjs", `
		import { box as a } from "./state.mjs";
		a.n = 41;
		import("file:///my dir/state.mjs").then((m) => {
			m.box.n++;
			globalThis.r = String(m.box.n) + "|" + String(m.box === a);
		});
	`)
	if got := evalStr(t, js, `r`); got != "42|true" {
		t.Errorf("encoded vs raw file URL spelling = %q, want \"42|true\" (one instance)", got)
	}
}

// A non-ASCII (CJK) path must round-trip: UTF-8 percent-encoding in
// import.meta.url, decoded back to the exact fs path when resolving — with
// no double-encoding on the second hop.
func TestImportMetaURLNonASCIIPathRoundTrip(t *testing.T) {
	fsys := fstest.MapFS{
		"データ/mod.mjs": {Data: []byte(`export const v = "cjk"; export const modUrl = import.meta.url;`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/データ/main.mjs", `
		import { v, modUrl } from "./mod.mjs";
		globalThis.r = { v, modUrl, mine: import.meta.url };
	`)
	if got := evalStr(t, js, `r.mine`); got != "file:///%E3%83%87%E3%83%BC%E3%82%BF/main.mjs" {
		t.Errorf("import.meta.url = %q, want \"file:///%%E3%%83%%87%%E3%%83%%BC%%E3%%82%%BF/main.mjs\"", got)
	}
	if got := evalStr(t, js, `r.modUrl`); got != "file:///%E3%83%87%E3%83%BC%E3%82%BF/mod.mjs" {
		t.Errorf("imported module URL = %q, want the singly-encoded mod.mjs URL", got)
	}
	if got := evalStr(t, js, `r.v`); got != "cjk" {
		t.Errorf("value through CJK path = %q, want \"cjk\"", got)
	}
}

// createRequire with a percent-encoded file: URL (the import.meta.url of a
// module under a space directory) must decode the path and resolve relative
// CJS requires against the real directory, sharing the require cache with
// the ESM interop path.
func TestCreateRequireFromPercentEncodedURL(t *testing.T) {
	fsys := fstest.MapFS{
		"my dir/sib.cjs": {Data: []byte(`exports.counter = { n: 0 };`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/my dir/main.mjs", `
		import { createRequire } from "node:module";
		import { counter } from "./sib.cjs";
		const requireHere = createRequire(import.meta.url);
		const viaMeta = requireHere("./sib.cjs");
		const viaLiteral = createRequire("file:///my%20dir/main.mjs")("./sib.cjs");
		counter.n = 40;
		viaMeta.counter.n++;
		viaLiteral.counter.n++;
		globalThis.r = String(counter.n) + "|" + String(viaMeta.counter === counter) + "|" + String(viaLiteral === viaMeta);
	`)
	if got := evalStr(t, js, `r`); got != "42|true|true" {
		t.Errorf("createRequire on encoded URL = %q, want \"42|true|true\"", got)
	}
}

// Relative imports between nested ESM files keep resolving under file:// URL
// registration, including transitive hops and extension-less specifiers.
func TestRelativeImportsUnderFileURLRegistration(t *testing.T) {
	fsys := fstest.MapFS{
		"app/a.mjs":     {Data: []byte(`import { b } from "./sub/b.mjs"; export const a = "a" + b;`)},
		"app/sub/b.mjs": {Data: []byte(`import { c } from "../c"; export const b = "b" + c;`)},
		"app/c.js":      {Data: []byte(`export const c = "c";`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runModuleOK(t, rt, "/app/main.mjs", `
		import { a } from "./a.mjs";
		globalThis.r = a;
	`)
	if got := evalStr(t, js, `r`); got != "abc" {
		t.Errorf("nested relative imports = %q, want \"abc\"", got)
	}
}

// A JSON module imported with the type attribute — `import data from "./x.json"
// with { type: "json" }`, the only form Node accepts — must yield the parsed
// data. The engine implements the attribute itself and JSON-parses whatever the
// module loader returns, so the loader has to answer with the raw bytes; it used
// to answer with `export default JSON.parse("…")`, which the engine then tried
// to parse AS JSON, and every such import failed. Modern packages hit this
// immediately: @babel/core's graph imports .json data modules, so nothing that
// depends on it could load.
func TestJSONModuleWithTypeAttribute(t *testing.T) {
	fsys := fstest.MapFS{
		"data.json":  {Data: []byte(`{"name": "babel", "list": [1, 2, 3]}`)},
		"array.json": {Data: []byte(`["a", "b"]`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import data from "./data.json" with { type: "json" };
		const arr = (await import("./array.json", { with: { type: "json" } })).default;
		globalThis.result = [data.name, data.list.length, arr.join("")].join("|");
	`)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, "result"); got != "babel|3|ab" {
		t.Errorf("result = %q, want \"babel|3|ab\"", got)
	}
}

// The inert-data guarantee must also hold for the attributed form: a .json file
// whose bytes are an executable JavaScript expression must not run, whichever
// way the import spells it.
func TestJSONModuleAttributedImportStaysInert(t *testing.T) {
	fsys := fstest.MapFS{
		"evil.json": {Data: []byte(`(globalThis.__PWNED = 1, {"x": 2})`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	if _, err := rt.RunModule(context.Background(), "main.mjs", `
		globalThis.r = {};
		try {
			await import("./evil.json", { with: { type: "json" } });
			r.imported = true;
		} catch (e) {
			r.threw = true;
		}
		r.pwned = globalThis.__PWNED ?? "unset";
	`); err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if got := evalStr(t, js, "String(r.pwned)"); got != "unset" {
		t.Fatalf("attributed .json import executed its payload: __PWNED = %q", got)
	}
	if evalVal(t, js, "!!r.imported").Bool() {
		t.Error("importing non-JSON .json succeeded; expected a parse error")
	}
}
