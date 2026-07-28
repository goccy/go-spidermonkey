package nodejs_test

// Regression tests for Node module-resolution semantics: '.'/'..' relative
// classification, package self-reference by name, and export-condition
// matching by object key order.

import (
	"context"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

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
