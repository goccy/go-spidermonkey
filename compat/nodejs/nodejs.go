// Package nodejs is the Node.js compatibility layer (docs/nodejs-compat-plan.md
// Phase 3): the node: core modules (path, events, util, buffer, fs, ...),
// process with Node's nextTick ordering, Buffer, CommonJS require with
// node_modules resolution, and ESM⇄CJS interop — installed explicitly:
//
//	js, _ := spidermonkey.New(spidermonkey.Config{FS: os.DirFS(appDir)})
//	rt, err := nodejs.Install(js)          // installs compat/web too
//	rt.RunScript(ctx, `const _ = require("lodash"); ...`)
//	rt.Wait(ctx)
//
// The standalone ESMLoader stays available for web-only embedders that just
// need pure-ESM npm imports without the Node runtime.
package nodejs

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// jsonModuleSource answers a JSON import with the file's bytes VERBATIM once
// they are known to be JSON.
//
// A JSON module is imported as `import data from "./x.json" with { type:
// "json" }` — the only form Node accepts, an attribute-less JSON import being
// ERR_IMPORT_ATTRIBUTE_MISSING. The ENGINE implements that attribute: it
// JSON-parses whatever the loader returns. So the loader must hand over the raw
// text; wrapping it in `export default JSON.parse("…")` made the engine parse
// that JavaScript as JSON and every attributed JSON import failed with
// "JSON.parse: unexpected character at line 1 column 1" (which is what kept
// @babel/core, whose graph imports .json data modules, from loading at all).
//
// The bytes must be VALIDATED before they are handed over, because the loader
// cannot see the import attribute and the engine treats the same answer two
// different ways: as JSON when the attribute is present, as JavaScript SOURCE
// when it is not. Valid JSON is inert either way — a JSON document contains
// only literals, so as JavaScript it is at worst a block of labelled literals
// with no side effects. Bytes that are NOT valid JSON could be an executable
// expression (`(globalThis.pwned = 1, {})`), and handing those over would run
// them on the attribute-less path, so they are replaced with a source that
// fails as JavaScript AND as JSON. Either way the import errors; in neither
// case does a data file execute.
func jsonModuleSource(src []byte) (string, error) {
	if !json.Valid(src) {
		return `throw new SyntaxError("Unexpected token in JSON module");`, nil
	}
	return string(src), nil
}

// ESMLoader is a spidermonkey.ModuleLoader for PURE-ESM resolution over
// Config.FS: relative/absolute paths (with extension fallbacks) and bare npm
// specifiers via node_modules (exports maps, worker/browser/import
// conditions, node_modules walk-up). A bare or renamed specifier answers
// with a shim re-exporting the resolved file, so the real file registers
// under its own path and ITS relative imports resolve correctly.
//
// CommonJS files are rejected here — that interop needs the full runtime
// (nodejs.Install, whose loader supersedes this one). Access control lives
// in Config.FS.
func ESMLoader(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
	r, err := resolveModule(cfg.FS, specifier, referrer, flavorWebESM)
	if err != nil {
		return "", err
	}
	if r.Core != "" {
		return "", fmt.Errorf("cannot import %q: node core modules need nodejs.Install", specifier)
	}
	src, err := readModuleFile(cfg, r.Path)
	if err != nil {
		return "", err
	}
	switch r.Kind {
	case kindJSON:
		return jsonModuleSource(src)
	case kindCJS:
		return "", fmt.Errorf("cannot import %q: CommonJS module %q needs nodejs.Install", specifier, r.Path)
	}
	if registeredPath(specifier, referrer) == r.Path {
		return string(src), nil
	}
	return esmShimFor(specifier, r.Path, src), nil
}

// registeredPath is the path-shaped normalization of the specifier the
// engine registers the module under (relative specifiers arrive pre-joined
// against the referrer, but handle raw ones too).
func registeredPath(specifier, referrer string) string {
	p := specifier
	switch {
	case strings.HasPrefix(p, "./"), strings.HasPrefix(p, "../"):
		p = path.Join(path.Dir(referrer), p)
	case strings.HasPrefix(p, "/"):
		p = strings.TrimPrefix(p, "/")
	}
	return path.Clean(p)
}

// esmShimFor builds the re-export shim for a module registered under
// specifier whose real source lives at realPath. The module registers under
// the bare/renamed specifier, so the shim's import target climbs back to the
// FS root — keeping the REAL file's registration path, and hence its
// relative imports, intact.
func esmShimFor(specifier, realPath string, src []byte) string {
	up := strings.Repeat("../", strings.Count(specifier, "/"))
	return reexportShim("./" + up + realPath)
}

// reexportShim re-exports everything a module at target exports, INCLUDING a
// default it may or may not have.
//
// `export * from` deliberately does not forward `default`, so a shim has to
// mention it — but `export { default } from target` is a static link that FAILS
// TO COMPILE when the target has no default export. Deciding by scanning the
// target's source for `export default` is what this used to do, and it is
// wrong in both directions: @babel/parser (and any module that merely mentions
// the phrase in a string or a comment — a parser's error messages do) got a
// default re-export it does not have, and every import of the package died with
// "doesn't provide an export named: 'default'".
//
// Reading the default off the namespace object instead is structural rather
// than textual: a namespace's missing property is simply undefined, so the same
// shim is correct whether or not the target has a default, with no source
// inspection at all. The one difference from Node is that importing a default
// that does not exist yields undefined here instead of a link error — a
// forgiving answer where the alternative was a wrong one.
func reexportShim(target string) string {
	return fmt.Sprintf(
		"import * as __ns from %[1]q;\nexport * from %[1]q;\nexport default __ns.default;\n", target)
}

// readModuleFile reads p from Config.FS. Access control lives in the FS (it
// may deny with fs.ErrPermission or hide with fs.ErrNotExist).
func readModuleFile(cfg spidermonkey.Config, p string) ([]byte, error) {
	b, err := readFile(cfg.FS, p)
	if err != nil {
		return nil, fmt.Errorf("load module %q: %w", p, err)
	}
	return b, nil
}
