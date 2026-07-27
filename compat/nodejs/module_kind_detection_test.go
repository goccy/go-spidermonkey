package nodejs_test

// Classifying a .js file that no package.json "type" covers is the engine's
// job, not a regex's. The engine compiles the source both ways — as a module
// and inside the CommonJS wrapper — and reports which one it parses as, which
// is Node's own rule. A syntax sniff cannot match it: a line-anchored
// import/export match misses a minified single-line bundle, and no regex can
// tell those keywords from the same words inside a comment or a string.

import (
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
