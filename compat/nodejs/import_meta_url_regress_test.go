package nodejs_test

// Regression tests for import.meta.url: modules register under canonical
// file:// URLs so import.meta.url is a real URL (new URL("./x",
// import.meta.url) works, as in Node) in the entry module AND in every
// imported module, while relative imports and require interop keep working.

import (
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
