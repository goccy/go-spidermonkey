package nodejs

// resolve.go: Node's module-resolution algorithm over an fs.FS — shared by
// the ESM loader (import) and the CJS require op. Covers: core-module names
// (with/without the node: prefix), relative/absolute paths with CJS-style
// extension guessing, bare specifiers with node_modules walk-up from the
// importing file, package.json "exports" (conditions, subpaths) and
// "imports" (#-specifiers), and module-kind classification (.mjs/.cjs/
// extension, package "type", source sniff).

import (
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
}

// coreModules are the node: builtins compat/nodejs implements (js/corelibs.js).
var coreModules = map[string]bool{
	"assert": true, "async_hooks": true, "buffer": true,
	"child_process": true, "cluster": true, "console": true,
	"constants": true, "crypto": true, "diagnostics_channel": true,
	"dns": true, "events": true, "fs": true, "fs/promises": true,
	"http": true, "http2": true, "https": true, "inspector": true,
	"module": true, "net": true, "os": true, "path": true,
	"perf_hooks": true, "process": true, "punycode": true,
	"querystring": true, "readline": true, "stream": true,
	"stream/promises": true, "stream/web": true, "string_decoder": true,
	"timers": true, "timers/promises": true, "tls": true, "tty": true,
	"url": true, "util": true, "v8": true, "vm": true,
	"worker_threads": true, "zlib": true,
}

// Export-condition preference: import (this runtime is Workers/browser-like,
// so those builds win) vs require.
var (
	esmConditions = []string{"worker", "workerd", "browser", "deno", "bun", "import", "module", "default"}
	cjsConditions = []string{"require", "node", "default"}
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

	if strings.HasPrefix(specifier, "#") {
		return resolveHashImport(fsys, specifier, parent, conds)
	}

	spec := specifier
	bare := false
	// file: URLs arrive from pathToFileURL round trips (dynamic import of
	// config files); map them onto the FS namespace.
	if after, ok := strings.CutPrefix(spec, "file://"); ok {
		spec = "/" + strings.TrimLeft(after, "/")
	} else if after, ok := strings.CutPrefix(spec, "file:"); ok {
		spec = "/" + strings.TrimLeft(after, "/")
	}
	switch {
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"):
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
		return resolveBareSpecifier(fsys, spec, parent, conds)
	}
	return resolution{}, fmt.Errorf("cannot resolve %q (from %q)", specifier, parent)
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

func resolveBareSpecifier(fsys fs.FS, specifier, parent string, conds []string) (resolution, error) {
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

func resolvePackage(fsys fs.FS, pkgDir, subpath string, conds []string) (resolution, error) {
	raw, err := fs.ReadFile(fsys, path.Join(pkgDir, "package.json"))
	if err != nil {
		return resolution{}, fmt.Errorf("package %q: %w", pkgDir, err)
	}
	var pkg struct {
		Exports any `json:"exports"`
		Browser any `json:"browser"`
		Module  any `json:"module"`
		Main    any `json:"main"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return resolution{}, fmt.Errorf("package %q: bad package.json: %w", pkgDir, err)
	}
	if pkg.Exports != nil {
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
func resolveHashImport(fsys fs.FS, specifier, parent string, conds []string) (resolution, error) {
	pkgDir, imports, ok := nearestImports(fsys, path.Dir(parent))
	if !ok {
		return resolution{}, fmt.Errorf("cannot resolve %q: no package.json with \"imports\" above %q", specifier, parent)
	}
	v, ok := imports[specifier]
	if !ok {
		return resolution{}, fmt.Errorf("package %q: no imports entry for %q", pkgDir, specifier)
	}
	target, err := resolveConditions(v, conds)
	if err != nil {
		return resolution{}, fmt.Errorf("package %q: imports[%q]: %w", pkgDir, specifier, err)
	}
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		return loadAsFileOrDir(fsys, path.Join(pkgDir, target))
	}
	// An imports target may itself be a bare specifier (a dependency).
	return resolveBareSpecifier(fsys, target, path.Join(pkgDir, "package.json"), conds)
}

// nearestImports walks up from dir to the closest package.json declaring an
// "imports" map.
func nearestImports(fsys fs.FS, dir string) (string, map[string]any, bool) {
	for _, d := range ancestorDirs(dir) {
		raw, err := fs.ReadFile(fsys, path.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Imports map[string]any `json:"imports"`
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
		r.Kind = classifyJS(fsys, p)
	}
	return r, nil
}

var esmSyntax = regexp.MustCompile(`(?m)^\s*(import|export)\b`)

// classifyJS decides ESM vs CJS for a .js file: the nearest package.json
// "type" field when present, else a top-level import/export sniff.
func classifyJS(fsys fs.FS, p string) moduleKind {
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
			return kindESM
		}
		return kindCJS
	}
	if src, err := fs.ReadFile(fsys, p); err == nil && esmSyntax.Match(src) {
		return kindESM
	}
	return kindCJS
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

// resolveExports resolves subpath ("" = the root export) through a
// package.json "exports" value (string, conditions object, or subpath map).
func resolveExports(exports any, subpath string, conds []string) (string, error) {
	key := "."
	if subpath != "" {
		key = "./" + subpath
	}
	if m, ok := exports.(map[string]any); ok && isSubpathMap(m) {
		v, ok := m[key]
		if !ok {
			return "", fmt.Errorf("no exports entry for %q", key)
		}
		return resolveConditions(v, conds)
	}
	if key != "." {
		return "", fmt.Errorf("no exports entry for %q", key)
	}
	return resolveConditions(exports, conds)
}

// isSubpathMap distinguishes {".": ..., "./x": ...} from a conditions map.
func isSubpathMap(m map[string]any) bool {
	for k := range m {
		return strings.HasPrefix(k, ".")
	}
	return false
}

func resolveConditions(v any, conds []string) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		for _, cond := range conds {
			if next, ok := t[cond]; ok {
				if s, err := resolveConditions(next, conds); err == nil {
					return s, nil
				}
			}
		}
		return "", fmt.Errorf("no matching export condition (have %v)", keysOf(t))
	}
	return "", fmt.Errorf("unsupported exports shape %T", v)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
