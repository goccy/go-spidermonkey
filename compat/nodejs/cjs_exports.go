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
	"fmt"
	"io/fs"
	"regexp"
	"strings"
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

var (
	// exports.NAME = / module.exports.NAME =   (the "=" must not open "==",
	// checked against the byte after the match so chained assignments like
	// exports.a = exports.b = 1 still yield both names)
	cjsExportsDotRE = regexp.MustCompile(`(?:^|[^\w$.])(?:module\s*\.\s*)?exports\s*\.\s*([A-Za-z_$][\w$]*)\s*=`)
	// exports["NAME"] = / module.exports['NAME'] =
	cjsExportsBracketRE = regexp.MustCompile(`(?:^|[^\w$.])(?:module\s*\.\s*)?exports\s*\[\s*(?:"([^"\\]+)"|'([^'\\]+)')\s*\]\s*=`)
	// Object.defineProperty(exports, "NAME", ...) — covers the Babel/TS
	// __esModule marker and getter-based re-exports.
	cjsDefinePropRE = regexp.MustCompile(`Object\s*\.\s*defineProperty\s*\(\s*(?:module\s*\.\s*)?exports\s*,\s*(?:"([^"\\]+)"|'([^'\\]+)')`)
	// module.exports = { ... } — the object-literal keys become named exports.
	cjsModuleExportsObjRE = regexp.MustCompile(`(?:^|[^\w$.])module\s*\.\s*exports\s*=\s*\{`)
	// module.exports = require("...") — a whole-module re-export chain.
	cjsModuleExportsRequireRE = regexp.MustCompile(`(?:^|[^\w$.])module\s*\.\s*exports\s*=\s*require\s*\(\s*(?:"([^"\\]+)"|'([^'\\]+)')\s*\)`)
	// __exportStar(require("..."), exports) — the TypeScript helper (bare or
	// via tslib).
	cjsExportStarRE = regexp.MustCompile(`__exportStar\s*\(\s*require\s*\(\s*(?:"([^"\\]+)"|'([^'\\]+)')\s*\)\s*,\s*exports\s*\)`)
	// var _m = require("...") bindings, for the Babel star-re-export loop.
	cjsRequireVarRE = regexp.MustCompile(`(?:var|let|const)\s+([A-Za-z_$][\w$]*)\s*=\s*require\s*\(\s*(?:"([^"\\]+)"|'([^'\\]+)')\s*\)`)
	// Object.keys(_m).forEach(...) — combined with an exports[key] copy or a
	// defineProperty getter in the callback body, a Babel star re-export.
	cjsKeysForEachRE = regexp.MustCompile(`Object\s*\.\s*keys\s*\(\s*([A-Za-z_$][\w$]*)\s*\)\s*\.\s*forEach`)
)

// cjsExportNames extracts the named exports of one CJS source, following
// re-export chains (module.exports = require(...), __exportStar, the Babel
// Object.keys forEach loop) through the FS. seen keys resolved paths to break
// cycles; depth caps pathological chains.
func (rt *Runtime) cjsExportNames(fsys fs.FS, p string, src []byte, seen map[string]bool, depth int) []string {
	if depth > 8 {
		return nil
	}
	masked := maskCJSSource(src)
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
	addAll := func(ns []string) {
		for _, n := range ns {
			add(n)
		}
	}
	// Re-export chains resolve relative to the analyzed file.
	follow := func(spec string) {
		addAll(rt.cjsReexportNames(fsys, spec, p, seen, depth))
	}

	// assignmentNotComparison rejects a match whose "=" actually opens "=="
	// (exports.foo === x is a comparison, not an export definition).
	assignmentNotComparison := func(end int) bool {
		return end >= len(masked) || masked[end] != '='
	}
	for _, loc := range cjsExportsDotRE.FindAllSubmatchIndex(masked, -1) {
		if assignmentNotComparison(loc[1]) {
			add(string(masked[loc[2]:loc[3]]))
		}
	}
	for _, loc := range cjsExportsBracketRE.FindAllSubmatchIndex(masked, -1) {
		if assignmentNotComparison(loc[1]) {
			name := ""
			if loc[2] >= 0 {
				name = string(masked[loc[2]:loc[3]])
			} else if loc[4] >= 0 {
				name = string(masked[loc[4]:loc[5]])
			}
			add(name)
		}
	}
	for _, m := range cjsDefinePropRE.FindAllSubmatch(masked, -1) {
		add(string(m[1]) + string(m[2]))
	}
	for _, loc := range cjsModuleExportsObjRE.FindAllSubmatchIndex(masked, -1) {
		// loc[1]-1 is the index of the '{' that the pattern ends with.
		addAll(objectLiteralKeys(masked[loc[1]-1:]))
	}
	for _, m := range cjsModuleExportsRequireRE.FindAllSubmatch(masked, -1) {
		follow(string(m[1]) + string(m[2]))
	}
	for _, m := range cjsExportStarRE.FindAllSubmatch(masked, -1) {
		follow(string(m[1]) + string(m[2]))
	}
	// Babel star re-export: Object.keys(_m).forEach(function (key) { ...
	// exports[key] = _m[key] ... }) where _m = require("...").
	requireVars := map[string]string{}
	for _, m := range cjsRequireVarRE.FindAllSubmatch(masked, -1) {
		requireVars[string(m[1])] = string(m[2]) + string(m[3])
	}
	for _, loc := range cjsKeysForEachRE.FindAllSubmatchIndex(masked, -1) {
		v := string(masked[loc[2]:loc[3]])
		spec, ok := requireVars[v]
		if !ok {
			continue
		}
		// Look for the copy/getter inside the callback (a bounded window after
		// the forEach keeps a stray Object.keys elsewhere from matching).
		end := loc[1] + 600
		if end > len(masked) {
			end = len(masked)
		}
		body := string(masked[loc[1]:end])
		if strings.Contains(body, "exports[key]") || strings.Contains(body, "defineProperty(exports, key") ||
			strings.Contains(body, "defineProperty(exports,key") {
			follow(spec)
		}
	}
	return names
}

// cjsReexportNames resolves a re-exported require target and returns ITS
// named exports: core-module export lists come from the runtime's collected
// tables, CJS files recurse into the analyzer.
func (rt *Runtime) cjsReexportNames(fsys fs.FS, spec, parent string, seen map[string]bool, depth int) []string {
	r, err := resolveModule(fsys, spec, parent, true)
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

// maskCJSSource blanks comments, template literals, and regex literals
// (contents replaced by spaces, length preserved) so the export-pattern
// regexes don't match inside them. Ordinary string literals are kept: the
// bracket/defineProperty patterns need their quoted names.
//
// Regex literals need token context (cjs-module-lexer does the same): a "/"
// after a value (identifier, number, ")", "]", string) is division, while a
// "/" after an operator/punctuator/keyword or at the start of input opens a
// regex literal whose body — including character classes and escapes — must
// be skipped, or a backtick inside it would open phantom template masking
// that blanks the rest of the source (losing later exports), and export-
// shaped text inside it would produce phantom exports.
func maskCJSSource(src []byte) []byte {
	out := append([]byte(nil), src...)
	// lastSig is the last significant (non-whitespace, non-comment) byte;
	// 0 means start of input. lastWord is the identifier token that byte
	// ended, for keyword checks (return /re/, typeof /re/, case /re/:, ...).
	var lastSig byte
	var lastWord string
	isIdentByte := func(c byte) bool {
		return c == '_' || c == '$' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || c >= 0x80
	}
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch c {
		case '/':
			if i+1 < len(out) && out[i+1] == '/' { // line comment
				j := i
				for j < len(out) && out[j] != '\n' {
					out[j] = ' '
					j++
				}
				i = j
				continue // comments are transparent: keep lastSig/lastWord
			}
			if i+1 < len(out) && out[i+1] == '*' { // block comment
				j := i
				for j < len(out) && !(out[j] == '*' && j+1 < len(out) && out[j+1] == '/') {
					if out[j] != '\n' {
						out[j] = ' '
					}
					j++
				}
				if j+1 < len(out) {
					out[j], out[j+1] = ' ', ' '
					j++
				}
				i = j
				continue
			}
			if regexCanFollow(lastSig, lastWord) {
				if end, ok := scanRegexLiteral(out, i); ok {
					for j := i; j < end; j++ {
						out[j] = ' '
					}
					i = end - 1
					lastSig, lastWord = ')', "" // a regex is a value
					continue
				}
			}
			lastSig, lastWord = '/', ""
		case '`': // template literal (nested ${} expressions blanked too)
			j := i + 1
			for j < len(out) {
				if out[j] == '\\' {
					out[j] = ' '
					if j+1 < len(out) && out[j+1] != '\n' {
						out[j+1] = ' '
					}
					j += 2
					continue
				}
				if out[j] == '`' {
					out[j] = ' '
					break
				}
				if out[j] != '\n' {
					out[j] = ' '
				}
				j++
			}
			out[i] = ' '
			i = j
			lastSig, lastWord = '"', "" // a template is a value
		case '\'', '"': // skip over ordinary strings (kept, not masked)
			q := c
			j := i + 1
			for j < len(out) && out[j] != q {
				if out[j] == '\\' {
					j++
				}
				j++
			}
			i = j
			lastSig, lastWord = '"', "" // a string is a value
		case ' ', '\t', '\n', '\r', '\v', '\f':
			// whitespace: transparent
		default:
			if isIdentByte(c) {
				j := i
				for j < len(out) && isIdentByte(out[j]) {
					j++
				}
				lastWord = string(out[i:j])
				lastSig = out[j-1]
				i = j - 1
				continue
			}
			lastSig, lastWord = c, ""
		}
	}
	return out
}

// regexPrecedingKeywords are keywords after which a "/" opens a regex literal
// even though the keyword's last byte is an identifier byte.
var regexPrecedingKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true,
	"of": true, "new": true, "delete": true, "void": true, "throw": true,
	"case": true, "do": true, "else": true, "yield": true, "await": true,
}

// regexCanFollow reports whether a "/" at the current position starts a regex
// literal (vs division), given the last significant byte and identifier token.
func regexCanFollow(lastSig byte, lastWord string) bool {
	if lastSig == 0 {
		return true // start of input
	}
	if lastWord != "" && regexPrecedingKeywords[lastWord] {
		return true
	}
	// After a value — identifier/number (any ident byte), ")", "]", or a
	// string/template/regex (recorded as '"' / ')') — "/" is division. After
	// any operator or opening punctuator it starts a regex. "}" is treated as
	// regex-capable: statement-closing braces vastly outnumber object-literal
	// operands of a division in real code (and ${...} templates are already
	// blanked before this runs).
	switch lastSig {
	case ')', ']', '"':
		return false
	}
	if lastSig == '_' || lastSig == '$' || ('a' <= lastSig && lastSig <= 'z') ||
		('A' <= lastSig && lastSig <= 'Z') || ('0' <= lastSig && lastSig <= '9') || lastSig >= 0x80 {
		return false
	}
	return true
}

// scanRegexLiteral scans the regex literal opening at out[start] ('/') and
// returns the index just past its flags. Escapes and character classes are
// honored ("/" inside [...] does not terminate). A newline before the closing
// "/" means it was not a regex literal after all (regexes cannot span lines).
func scanRegexLiteral(out []byte, start int) (end int, ok bool) {
	inClass := false
	j := start + 1
	for j < len(out) {
		switch out[j] {
		case '\\':
			if j+1 < len(out) && (out[j+1] == '\n' || out[j+1] == '\r') {
				return 0, false // regexes cannot span lines, even escaped
			}
			j++ // skip escaped byte
		case '\n', '\r':
			return 0, false
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				j++
				for j < len(out) && ('a' <= out[j] && out[j] <= 'z') {
					j++ // flags
				}
				return j, true
			}
		}
		j++
	}
	return 0, false
}

// objectLiteralKeys reads the top-level keys of the object literal starting
// at s[0] == '{': identifier keys (shorthand, method, or key: value),
// get/set accessors, and quoted-string keys that are valid identifiers.
// Nested objects/arrays/calls and string values are skipped structurally.
func objectLiteralKeys(s []byte) []string {
	if len(s) == 0 || s[0] != '{' {
		return nil
	}
	var keys []string
	depth := 0
	expectKey := true
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			q := c
			j := i + 1
			for j < len(s) && s[j] != q {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if depth == 1 && expectKey {
				key := string(s[i+1 : min(j, len(s))])
				// Only count it as a key when a ':' follows.
				k := j + 1
				for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
					k++
				}
				if k < len(s) && s[k] == ':' {
					keys = append(keys, key)
					expectKey = false
				}
			}
			i = j + 1
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
			if c == '{' && depth == 1 {
				expectKey = true
			} else {
				expectKey = false
			}
		case c == '}' || c == ']' || c == ')':
			depth--
			if depth <= 0 {
				return keys
			}
		case c == ',':
			if depth == 1 {
				expectKey = true
			}
		case depth == 1 && expectKey && (c == '_' || c == '$' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')):
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '$' ||
				('a' <= s[j] && s[j] <= 'z') || ('A' <= s[j] && s[j] <= 'Z') ||
				('0' <= s[j] && s[j] <= '9')) {
				j++
			}
			word := string(s[i:j])
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
				k++
			}
			if (word == "get" || word == "set" || word == "async") && k < len(s) &&
				(s[k] == '_' || s[k] == '$' || ('a' <= s[k]|0x20 && s[k]|0x20 <= 'z') ||
					(word == "async" && s[k] == '*')) {
				// Accessor (get x/set x) or async-method modifier (async x(),
				// async *x()): the NEXT identifier is the key. A "*" after
				// "async" re-enters the loop and falls through to the
				// generator-name identifier. When the word is instead a key
				// itself (get: 1, async: 2, set,) the next byte is ":"/","/"}"
				// and the normal key handling below applies.
				i = k
				continue
			}
			if k < len(s) {
				switch s[k] {
				case ':', '(': // key: value, or method shorthand
					keys = append(keys, word)
					expectKey = false
				case ',', '}': // shorthand property
					keys = append(keys, word)
					expectKey = false
				default:
					expectKey = false
				}
			}
			i = j
			continue
		case c == '.': // spread (or stray dot): not a key
			expectKey = false
		}
		i++
	}
	return keys
}
