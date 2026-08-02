package nodejs

// cjs_exports.go: static analysis of CommonJS sources to synthesize ESM named
// exports — the same idea as Node's cjs-module-lexer. When an ES module
// imports a CJS file, Node scans the CJS source for the common export-
// assignment patterns and exposes the detected names as named exports next to
// the default (module.exports). This analyzer covers the patterns that carry
// the bulk of real-world transpiled code:
//
//	exports.NAME = …               module.exports.NAME = …
//	exports["NAME"] = …            module.exports['NAME'] = …
//	Object.defineProperty(exports, "NAME", …)   (Babel/TS getter re-exports)
//	module.exports = { NAME, NAME2: …, "NAME3": …, NAME4() {…} }
//	module.exports = require("…")               (whole-module re-export chain)
//	__exportStar(require("…"), exports)          (TypeScript helper)
//	Object.keys(_m).forEach(… exports[key] = _m[key] …)   (Babel star re-export)
//
// Detected names are filtered to valid JS identifiers, deduped, and "default"
// is excluded (the default export is always module.exports itself, as in
// Node). Re-export chains are followed through the FS with a cycle guard.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// cjsShim is the ESM view of a CJS file: the default export is module.exports
// (evaluated through require, sharing the require cache), plus statically
// detected named exports. Mirrors coreShim for core modules.
func (rt *Runtime) cjsShim(fsys fs.FS, p string, src []byte) string {
	seen := map[string]bool{p: true}
	names := rt.cjsExportNames(fsys, p, src, seen, 0)
	var b strings.Builder
	fmt.Fprintf(&b, "const __cjs_default__ = globalThis.__node_require_path(%q);\nexport default __cjs_default__;\n", p)
	if len(names) > 0 {
		b.WriteString("const __cjs_ns__ = (__cjs_default__ === null || __cjs_default__ === undefined) ? {} : Object(__cjs_default__);\n")
		// Each name is read under its own try/catch, as in Node's CJS translator:
		// a throwing getter (deprecation trap, bundler TDZ guard in a cycle) makes
		// THAT binding undefined instead of aborting the whole module — the
		// default export and every other name must keep working.
		for _, n := range names {
			fmt.Fprintf(&b, "let %s; try { %s = __cjs_ns__[%q]; } catch {}\nexport { %s };\n", n, n, n, n)
		}
	}
	return b.String()
}

// cjsExportNames extracts the named exports of one CJS source, following
// re-export chains (module.exports = require(...), __exportStar, the Babel
// Object.keys forEach loop) through the FS. seen keys resolved paths to break
// cycles; depth caps pathological chains.
func (rt *Runtime) cjsExportNames(fsys fs.FS, p string, src []byte, seen map[string]bool, depth int) []string {
	if depth > 8 {
		return nil
	}
	lex, ok := rt.lexCJS(src)
	if !ok {
		// The engine could not parse it (or is too old to have Reflect.parse):
		// no names rather than guessed ones. The default export still works.
		return nil
	}
	var names []string
	set := map[string]bool{}
	add := func(n string) {
		if n == "" || n == "default" || set[n] || !identRE.MatchString(n) || reservedWords[n] {
			return
		}
		if n == "__cjs_default__" || n == "__cjs_ns__" {
			return // would collide with the shim's own bindings
		}
		set[n] = true
		names = append(names, n)
	}
	for _, n := range lex.Names {
		add(n)
	}
	// A re-export names another module; only the host can resolve it.
	for _, spec := range lex.Reexports {
		for _, n := range rt.cjsReexportNames(fsys, spec, p, seen, depth) {
			add(n)
		}
	}
	return names
}

// cjsLexResult is what js/cjslexer.js reports for one source.
type cjsLexResult struct {
	Names     []string `json:"names"`
	Reexports []string `json:"reexports"`
	Parsed    bool     `json:"parsed"`
}

// lexCJS asks the ENGINE which names a CommonJS source assigns to exports —
// Reflect.parse over the real grammar, not patterns over the text. Returns
// ok=false when the source does not parse or the lexer is unavailable, which
// the caller treats as "no named exports" rather than as a guess.
func (rt *Runtime) lexCJS(src []byte) (cjsLexResult, bool) {
	var out cjsLexResult
	if rt.js == nil {
		return out, false
	}
	fn, err := rt.js.Global().Get("__node_cjs_lex")
	if err != nil || fn == nil || !fn.IsObject() {
		return out, false
	}
	res, err := rt.js.Global().CallMethod("__node_cjs_lex", spidermonkey.ValueOf(string(src)))
	if err != nil || res == nil {
		return out, false
	}
	if err := json.Unmarshal([]byte(res.String()), &out); err != nil {
		return out, false
	}
	return out, out.Parsed
}

// cjsReexportNames resolves a re-exported require target and returns ITS
// named exports: core-module export lists come from the runtime's collected
// tables, CJS files recurse into the analyzer.
func (rt *Runtime) cjsReexportNames(fsys fs.FS, spec, parent string, seen map[string]bool, depth int) []string {
	r, err := resolveModule(fsys, spec, parent, flavorNodeCJS)
	if err != nil {
		return nil
	}
	if r.Core != "" {
		return rt.coreExports[r.Core]
	}
	rt.refineKind(fsys, &r, nil)
	if r.Kind != kindCJS || seen[r.Path] {
		return nil
	}
	seen[r.Path] = true
	src, err := readFile(fsys, r.Path)
	if err != nil {
		return nil
	}
	return rt.cjsExportNames(fsys, r.Path, src, seen, depth+1)
}
