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
	"fmt"
	"path"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// ESMLoader is a spidermonkey.ModuleLoader for PURE-ESM resolution over
// Config.FS: relative/absolute paths (with extension fallbacks) and bare npm
// specifiers via node_modules (exports maps, worker/browser/import
// conditions, node_modules walk-up). A bare or renamed specifier answers
// with a shim re-exporting the resolved file, so the real file registers
// under its own path and ITS relative imports resolve correctly.
//
// CommonJS files are rejected here — that interop needs the full runtime
// (nodejs.Install, whose loader supersedes this one). Reads are gated by
// Config.FSAccess.
func ESMLoader(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
	r, err := resolveModule(cfg.FS, specifier, referrer, false)
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
		return "export default (" + string(src) + ");", nil
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
	target := "./" + up + realPath
	shim := fmt.Sprintf("export * from %q;\n", target)
	if hasDefaultExport(src) {
		shim += fmt.Sprintf("export { default } from %q;\n", target)
	}
	return shim
}

// hasDefaultExport is a textual heuristic good enough for dist builds:
// `export * from` cannot forward a default export, so the shim adds it only
// when the target visibly declares one.
func hasDefaultExport(src []byte) bool {
	s := string(src)
	return strings.Contains(s, "export default") || strings.Contains(s, "as default")
}

// readModuleFile reads p from Config.FS under the FSAccess gate.
func readModuleFile(cfg spidermonkey.Config, p string) ([]byte, error) {
	if cfg.FSAccess != nil && !cfg.FSAccess(p, false) {
		return nil, fmt.Errorf("load module %q: permission denied", p)
	}
	b, err := readFile(cfg.FS, p)
	if err != nil {
		return nil, fmt.Errorf("load module %q: %w", p, err)
	}
	return b, nil
}
