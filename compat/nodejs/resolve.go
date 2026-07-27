package nodejs

// resolve.go: Node's module-resolution algorithm over an fs.FS — shared by
// the ESM loader (import) and the CJS require op. Covers: core-module names
// (with/without the node: prefix), relative/absolute paths with CJS-style
// extension guessing, bare specifiers with node_modules walk-up from the
// importing file, package.json "exports" (conditions, subpaths) and
// "imports" (#-specifiers), and module-kind classification (.mjs/.cjs/
// extension, package "type", source sniff).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

type moduleKind int

const (
	kindESM moduleKind = iota
	kindCJS
	kindJSON
)

// resolution is the outcome of resolving one specifier.
type resolution struct {
	Core string // core-module name ("path", "fs/promises", ...); else empty
	Path string // fs path of the resolved file
	Kind moduleKind
	// KindGuessed marks a Kind that no file extension and no package.json
	// "type" decided — it came from sniffing the source text, which cannot
	// tell an `import` keyword from the same word in a comment or a string.
	// A caller holding an interpreter replaces the guess with the engine's
	// own answer; see Runtime.refineKind.
	KindGuessed bool
}

// coreModules are the node: builtins compat/nodejs implements (js/corelibs.js).
var coreModules = map[string]bool{
	"assert": true, "assert/strict": true, "async_hooks": true,
	"buffer": true, "child_process": true, "cluster": true,
	"console": true, "constants": true, "crypto": true,
	"dgram": true, "diagnostics_channel": true, "dns": true,
	"dns/promises": true,
	"events":       true, "fs": true, "fs/promises": true, "http": true,
	"http2": true, "https": true, "inspector": true,
	"inspector/promises": true, "module": true, "net": true, "os": true,
	"path": true, "path/posix": true, "path/win32": true,
	"perf_hooks": true, "process": true, "punycode": true,
	"querystring": true, "readline": true, "readline/promises": true,
	"stream": true, "stream/consumers": true, "stream/promises": true,
	"stream/web": true, "string_decoder": true, "sys": true,
	"timers": true, "timers/promises": true, "tls": true, "tty": true,
	"url": true, "util": true, "util/types": true, "v8": true, "vm": true,
	"worker_threads": true, "zlib": true,
}

// Enabled export/import conditions. Node semantics: a conditions object is
// iterated IN KEY ORDER and the first key present in the enabled set wins —
// the set only says which conditions are enabled, the package.json's key
// order decides preference. The ESM set additionally enables the
// Workers/browser-style conditions (this runtime is Workers/browser-like, so
// packages shipping such builds can serve them when they list them first).
var (
	esmConditions = map[string]bool{
		"worker": true, "workerd": true, "browser": true, "deno": true,
		"bun": true, "import": true, "module": true, "default": true,
	}
	cjsConditions = map[string]bool{"require": true, "node": true, "default": true}
)

// resolveModule resolves specifier from the module at parent (an fs path, or
// any registered specifier for root modules). cjs selects require-flavored
// export conditions.
func resolveModule(fsys fs.FS, specifier, parent string, cjs bool) (resolution, error) {
	if name, ok := strings.CutPrefix(specifier, "node:"); ok {
		if coreModules[name] {
			return resolution{Core: name}, nil
		}
		return resolution{}, fmt.Errorf("unknown builtin module %q", specifier)
	}
	if coreModules[specifier] {
		return resolution{Core: specifier}, nil
	}
	if fsys == nil {
		return resolution{}, fmt.Errorf("cannot resolve %q: no FS configured", specifier)
	}
	conds := esmConditions
	if cjs {
		conds = cjsConditions
	}
	// The parent may be a registered file: URL specifier (ESM modules register
	// under their canonical file:// URL so import.meta.url is a real URL) or
	// an absolute path; map it onto the rootless fs.FS namespace before any
	// path.Dir arithmetic (node_modules walk-up, hash imports, self-reference).
	parent = strings.TrimPrefix(fileURLToFSPath(parent), "/")

	if strings.HasPrefix(specifier, "#") {
		return resolveHashImport(fsys, specifier, parent, conds)
	}

	spec := specifier
	bare := false
	// file: URLs arrive from pathToFileURL round trips (dynamic import of
	// config files) and from the engine's referrer-joining of relative imports
	// against file:// registrations; map them onto the FS namespace.
	spec = fileURLToFSPath(spec)
	switch {
	case spec == "." || spec == ".." || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		// Node: '.', './', '..' and '../' all resolve relative to the
		// requiring module's directory.
		spec = path.Join(path.Dir(parent), spec)
	case strings.HasPrefix(spec, "/"):
		spec = strings.TrimPrefix(spec, "/")
	default:
		bare = true
	}
	// The engine pre-joins relative specifiers against the referrer, so a
	// path-shaped string ("lib/util.js") is indistinguishable from a bare
	// one here: files win, then the node_modules walk-up.
	if r, err := loadAsFileOrDir(fsys, path.Clean(spec)); err == nil {
		return r, nil
	}
	if bare {
		// Node tries package self-reference (require('myname/sub') inside the
		// package named "myname") before the node_modules walk-up.
		if r, ok := resolveSelfReference(fsys, spec, parent, conds); ok {
			return r, nil
		}
		return resolveBareSpecifier(fsys, spec, parent, conds)
	}
	return resolution{}, fmt.Errorf("cannot resolve %q (from %q)", specifier, parent)
}

// fileURLToFSPath maps a file: URL ("file:///a/b", or the engine's path-joined
// "file:/a/b" form) onto the absolute-path shape the resolver works with; any
// other string passes through unchanged. URL paths are percent-DECODED (the
// inverse of fsPathToFileURLPath): "file:///my%20dir/x.mjs" names the fs path
// "/my dir/x.mjs".
func fileURLToFSPath(s string) string {
	if after, ok := strings.CutPrefix(s, "file://"); ok {
		return "/" + percentDecode(strings.TrimLeft(after, "/"))
	}
	if after, ok := strings.CutPrefix(s, "file:"); ok {
		return "/" + percentDecode(strings.TrimLeft(after, "/"))
	}
	return s
}

// fsPathToFileURLPath percent-encodes an fs path for use inside a file:// URL,
// with Node's pathToFileURL rules: "/" and the RFC 3986 unreserved plus
// path-safe sub-delims stay literal; space, "%", "#", "?", controls, and
// non-ASCII bytes (the path is UTF-8) are percent-encoded. Encoding "%" keeps
// the mapping bijective, so an encode/decode round trip returns the exact
// original path (no double-encoding).
func fsPathToFileURLPath(p string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/' || c == '-' || c == '.' || c == '_' || c == '~' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9'):
			b.WriteByte(c)
		case c == '!' || c == '$' || c == '&' || c == '\'' || c == '(' || c == ')' ||
			c == '*' || c == '+' || c == ',' || c == ';' || c == '=' || c == ':' || c == '@':
			b.WriteByte(c) // sub-delims etc. valid literally in a URL path
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	return b.String()
}

// percentDecode reverses UTF-8 percent-encoding ("%20" -> " ", "%E3%83%87..."
// -> the UTF-8 bytes). A "%" not followed by two hex digits passes through
// literally, so raw (never-encoded) paths are unchanged.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok1 := unhexDigit(s[i+1]); ok1 {
				if lo, ok2 := unhexDigit(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// loadAsFileOrDir applies the CJS file-resolution ladder to p: exact file,
// extension guesses, then directory (package.json main / index files).
func loadAsFileOrDir(fsys fs.FS, p string) (resolution, error) {
	for _, cand := range []string{p, p + ".js", p + ".json", p + ".mjs", p + ".cjs"} {
		if isFile(fsys, cand) {
			return finish(fsys, cand)
		}
	}
	if info, err := fs.Stat(fsys, p); err == nil && info.IsDir() {
		if raw, err := fs.ReadFile(fsys, path.Join(p, "package.json")); err == nil {
			var pkg struct {
				Main   any `json:"main"`
				Module any `json:"module"`
			}
			if json.Unmarshal(raw, &pkg) == nil {
				for _, entry := range []string{strField(pkg.Module), strField(pkg.Main)} {
					if entry != "" {
						if r, err := loadAsFileOrDir(fsys, path.Join(p, entry)); err == nil {
							return r, nil
						}
					}
				}
			}
		}
		for _, idx := range []string{"index.js", "index.json", "index.mjs", "index.cjs"} {
			if cand := path.Join(p, idx); isFile(fsys, cand) {
				return finish(fsys, cand)
			}
		}
	}
	return resolution{}, fmt.Errorf("cannot resolve %q", p)
}

// resolveSelfReference implements Node's package self-reference (v12.16+): a
// bare specifier whose package-name prefix matches the "name" of the nearest
// package.json above the importing file resolves through that package's
// "exports" map. Following Node's trySelf, only the NEAREST package.json is
// consulted, and self-reference applies only when it declares both "name" and
// "exports"; on any mismatch or resolution failure the normal node_modules
// walk-up proceeds.
func resolveSelfReference(fsys fs.FS, specifier, parent string, conds map[string]bool) (resolution, bool) {
	name, subpath := splitBare(specifier)
	for _, d := range ancestorDirs(path.Dir(parent)) {
		raw, err := fs.ReadFile(fsys, path.Join(d, "package.json"))
		if err != nil {
			continue
		}
		// The nearest package.json IS the package scope: stop walking here.
		var pkg struct {
			Name    string          `json:"name"`
			Exports json.RawMessage `json:"exports"`
		}
		if json.Unmarshal(raw, &pkg) != nil || pkg.Name != name || !hasJSONValue(pkg.Exports) {
			return resolution{}, false
		}
		target, err := resolveExports(pkg.Exports, subpath, conds)
		if err != nil {
			return resolution{}, false
		}
		r, err := loadAsFileOrDir(fsys, path.Join(d, target))
		if err != nil {
			return resolution{}, false
		}
		return r, true
	}
	return resolution{}, false
}

func resolveBareSpecifier(fsys fs.FS, specifier, parent string, conds map[string]bool) (resolution, error) {
	name, subpath := splitBare(specifier)
	for _, dir := range ancestorDirs(path.Dir(parent)) {
		pkgDir := path.Join(dir, "node_modules", name)
		if _, err := fs.Stat(fsys, pkgDir); err != nil {
			continue
		}
		return resolvePackage(fsys, pkgDir, subpath, conds)
	}
	return resolution{}, fmt.Errorf("cannot find package %q (imported from %q)", name, parent)
}

// ancestorDirs lists dir and each parent up to the FS root ("." last).
func ancestorDirs(dir string) []string {
	dir = path.Clean(dir)
	var out []string
	for {
		out = append(out, dir)
		if dir == "." || dir == "/" {
			return out
		}
		dir = path.Dir(dir)
	}
}

func resolvePackage(fsys fs.FS, pkgDir, subpath string, conds map[string]bool) (resolution, error) {
	raw, err := fs.ReadFile(fsys, path.Join(pkgDir, "package.json"))
	if err != nil {
		return resolution{}, fmt.Errorf("package %q: %w", pkgDir, err)
	}
	var pkg struct {
		Exports json.RawMessage `json:"exports"`
		Browser any             `json:"browser"`
		Module  any             `json:"module"`
		Main    any             `json:"main"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return resolution{}, fmt.Errorf("package %q: bad package.json: %w", pkgDir, err)
	}
	if hasJSONValue(pkg.Exports) {
		target, err := resolveExports(pkg.Exports, subpath, conds)
		if err != nil {
			return resolution{}, fmt.Errorf("package %q: %w", pkgDir, err)
		}
		return loadAsFileOrDir(fsys, path.Join(pkgDir, target))
	}
	if subpath != "" {
		return loadAsFileOrDir(fsys, path.Join(pkgDir, subpath))
	}
	// A string "browser" field wins in this sandboxed, Workers-like runtime:
	// packages use it to point at builds free of V8/tty/OS dependencies
	// (depd, debug, ...). The object (per-file remap) form is ignored.
	for _, entry := range []string{strField(pkg.Browser), strField(pkg.Module), strField(pkg.Main)} {
		if entry != "" {
			if r, err := loadAsFileOrDir(fsys, path.Join(pkgDir, entry)); err == nil {
				return r, nil
			}
		}
	}
	return loadAsFileOrDir(fsys, pkgDir) // index.* ladder
}

// resolveHashImport resolves a package-internal "#..." specifier through the
// importing package's "imports" field.
func resolveHashImport(fsys fs.FS, specifier, parent string, conds map[string]bool) (resolution, error) {
	pkgDir, imports, ok := nearestImports(fsys, path.Dir(parent))
	if !ok {
		return resolution{}, fmt.Errorf("cannot resolve %q: no package.json with \"imports\" above %q", specifier, parent)
	}
	target, err := resolveSubpathMap(imports, specifier, conds)
	if err != nil {
		return resolution{}, fmt.Errorf("package %q: no imports entry for %q", pkgDir, specifier)
	}
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		return loadAsFileOrDir(fsys, path.Join(pkgDir, target))
	}
	// An imports target may itself be a bare specifier (a dependency).
	return resolveBareSpecifier(fsys, target, path.Join(pkgDir, "package.json"), conds)
}

// nearestImports walks up from dir to the closest package.json declaring an
// "imports" map.
func nearestImports(fsys fs.FS, dir string) (string, map[string]json.RawMessage, bool) {
	for _, d := range ancestorDirs(dir) {
		raw, err := fs.ReadFile(fsys, path.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Imports map[string]json.RawMessage `json:"imports"`
		}
		if json.Unmarshal(raw, &pkg) == nil && pkg.Imports != nil {
			return d, pkg.Imports, true
		}
	}
	return "", nil, false
}

// finish classifies a resolved file.
func finish(fsys fs.FS, p string) (resolution, error) {
	r := resolution{Path: p}
	switch path.Ext(p) {
	case ".json":
		r.Kind = kindJSON
	case ".mjs":
		r.Kind = kindESM
	case ".cjs":
		r.Kind = kindCJS
	default:
		r.Kind, r.KindGuessed = classifyJS(fsys, p)
	}
	return r, nil
}

var esmSyntax = regexp.MustCompile(`(?m)^\s*(import|export)\b`)

// classifyJS decides ESM vs CJS for a .js file: the nearest package.json
// "type" field when present, else a top-level import/export sniff. The second
// result reports that the answer came from that sniff and is therefore only a
// guess — the caller replaces it with the engine's answer where it can.
func classifyJS(fsys fs.FS, p string) (moduleKind, bool) {
	for _, d := range ancestorDirs(path.Dir(p)) {
		raw, err := fs.ReadFile(fsys, path.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &pkg) != nil || pkg.Type == "" {
			continue
		}
		if pkg.Type == "module" {
			return kindESM, false
		}
		return kindCJS, false
	}
	if src, err := fs.ReadFile(fsys, p); err == nil && esmSyntax.Match(src) {
		return kindESM, true
	}
	return kindCJS, true
}

// refineKind replaces a source-sniffed ESM/CJS guess with the engine's own
// answer: it compiles the source both ways and reports which one it parses as,
// which is Node's actual detection rule and the exact question the sniff is
// approximating. Only a guess is touched — an explicit .mjs/.cjs extension or a
// package.json "type" always wins, as it must.
//
// src is the already-read source when the caller has it, else nil to read it.
// Anything that goes wrong leaves the guess in place: classification must not
// start failing because a bridge call did.
func (rt *Runtime) refineKind(fsys fs.FS, r *resolution, src []byte) {
	if !r.KindGuessed || rt.js == nil {
		return
	}
	if src == nil {
		var err error
		if src, err = readFile(fsys, r.Path); err != nil {
			return
		}
	}
	isModule, err := rt.js.SourceIsModule(string(src))
	if err != nil {
		return
	}
	r.Kind = kindCJS
	if isModule {
		r.Kind = kindESM
	}
	r.KindGuessed = false
}

// strField reads a package.json field that SHOULD be a string but is
// sometimes false/other JSON (math-intrinsics sets "main": false).
func strField(v any) string {
	s, _ := v.(string)
	return s
}

func readFile(fsys fs.FS, p string) ([]byte, error) {
	if fsys == nil {
		return nil, fmt.Errorf("no FS configured")
	}
	return fs.ReadFile(fsys, p)
}

func isFile(fsys fs.FS, p string) bool {
	if !fs.ValidPath(p) {
		return false
	}
	info, err := fs.Stat(fsys, p)
	return err == nil && !info.IsDir()
}

// splitBare splits "pkg/sub/path" (or "@scope/pkg/sub") into package name
// and subpath.
func splitBare(specifier string) (name, subpath string) {
	parts := strings.Split(specifier, "/")
	n := 1
	if strings.HasPrefix(specifier, "@") && len(parts) > 1 {
		n = 2
	}
	return strings.Join(parts[:n], "/"), strings.Join(parts[n:], "/")
}

// hasJSONValue reports whether a raw package.json field is present and not
// JSON null ("exports": null means no exports).
func hasJSONValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// resolveExports resolves subpath ("" = the root export) through a
// package.json "exports" value (string, conditions object, or subpath map).
func resolveExports(exports json.RawMessage, subpath string, conds map[string]bool) (string, error) {
	key := "."
	if subpath != "" {
		key = "./" + subpath
	}
	if m, ok := rawObject(exports); ok && isSubpathMap(m) {
		t, err := resolveSubpathMap(m, key, conds)
		if err != nil {
			return "", fmt.Errorf("no exports entry for %q", key)
		}
		return t, nil
	}
	if key != "." {
		return "", fmt.Errorf("no exports entry for %q", key)
	}
	return resolveConditions(exports, conds)
}

// rawObject decodes raw as a JSON object (values kept raw), reporting whether
// it is one.
func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(t, &m) != nil {
		return nil, false
	}
	return m, true
}

// resolveSubpathMap resolves key against a subpath "exports"/"imports" map,
// honoring exact keys AND a single "*" wildcard (Node subpath patterns, e.g.
// "./*": "./src/*.js" or "#lib/*": "./lib/*.js"). Among matching patterns the
// one with the longest literal prefix (then longest suffix) wins, and the
// captured middle segment is substituted for "*" in the resolved target.
func resolveSubpathMap(m map[string]json.RawMessage, key string, conds map[string]bool) (string, error) {
	if v, ok := m[key]; ok {
		return resolveConditions(v, conds)
	}
	bestPat, bestStar := "", ""
	bestPrefixLen, bestSuffixLen := -1, -1
	for pat := range m {
		star := strings.IndexByte(pat, '*')
		if star < 0 {
			continue
		}
		prefix, suffix := pat[:star], pat[star+1:]
		if strings.Contains(suffix, "*") { // only a single wildcard is supported
			continue
		}
		// The "*" must capture at least one character (Node: an empty capture is
		// ERR_PACKAGE_PATH_NOT_EXPORTED), hence strictly-greater, not >=.
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) || len(key) <= len(prefix)+len(suffix) {
			continue
		}
		if pl, sl := len(prefix), len(suffix); pl > bestPrefixLen || (pl == bestPrefixLen && sl > bestSuffixLen) {
			bestPat = pat
			bestStar = key[pl : len(key)-sl]
			bestPrefixLen, bestSuffixLen = pl, sl
		}
	}
	if bestPat == "" {
		return "", fmt.Errorf("no entry for %q", key)
	}
	// Reject a captured segment that could escape the package via path traversal
	// (Node throws ERR_INVALID_MODULE_SPECIFIER): no "."/".." path components and
	// no backslashes, so the substituted target can't climb out of the package.
	for _, seg := range strings.Split(bestStar, "/") {
		if seg == "." || seg == ".." || strings.Contains(seg, "\\") {
			return "", fmt.Errorf("invalid subpath %q", key)
		}
	}
	target, err := resolveConditions(m[bestPat], conds)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(target, "*", bestStar), nil
}

// isSubpathMap distinguishes {".": ..., "./x": ...} from a conditions map.
func isSubpathMap(m map[string]json.RawMessage) bool {
	for k := range m {
		return strings.HasPrefix(k, ".")
	}
	return false
}

// resolveConditions resolves an exports/imports target: a string, a fallback
// array, or a conditions object. Node iterates a conditions object's keys IN
// SOURCE ORDER (hence the raw-JSON walk — Go maps would lose the order) and
// takes the first key in the enabled-conditions set whose target resolves;
// nested objects recurse with the same rule.
func resolveConditions(v json.RawMessage, conds map[string]bool) (string, error) {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return "", fmt.Errorf("empty exports target")
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", err
		}
		return s, nil
	case '{':
		dec := json.NewDecoder(bytes.NewReader(t))
		if _, err := dec.Token(); err != nil { // opening '{'
			return "", err
		}
		var have []string
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			key, _ := keyTok.(string)
			var val json.RawMessage
			if err := dec.Decode(&val); err != nil {
				return "", err
			}
			have = append(have, key)
			if conds[key] {
				if s, err := resolveConditions(val, conds); err == nil {
					return s, nil
				}
			}
		}
		return "", fmt.Errorf("no matching export condition (have %v)", have)
	case '[':
		// A fallback array: Node tries each target in order and takes the first
		// that resolves.
		var items []json.RawMessage
		if err := json.Unmarshal(t, &items); err != nil {
			return "", err
		}
		for _, item := range items {
			if s, err := resolveConditions(item, conds); err == nil {
				return s, nil
			}
		}
		return "", fmt.Errorf("no resolvable target in fallback array")
	}
	return "", fmt.Errorf("unsupported exports target %s", string(t))
}
