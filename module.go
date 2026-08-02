package spidermonkey

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
)

// EvalModule compiles and runs src as an ES module registered under specifier,
// resolving its imports through the module loader (the default Config.FS loader,
// or one set with SetModuleLoader) and draining the job queue. ctx aborts a
// runaway module. Like Eval, a JavaScript-level failure is in Result, not a Go
// error.
func (js *JS) EvalModule(ctx context.Context, specifier, src string) (ModuleResult, error) {
	raw, err := js.raw.EvalModuleContext(ctx, specifier, src)
	if err != nil {
		return ModuleResult{}, err
	}
	return parseModuleResultFor(js, raw)
}

// ModuleLoader resolves a module specifier to its source. It receives the
// interpreter's Config, the resolved module specifier, and the specifier of
// the importing module (referrer).
type ModuleLoader func(cfg Config, specifier, referrer string) (string, error)

// ModuleType is what an import ASKED for through its import attributes:
// ModuleTypeJSON for `with { type: "json" }`, ModuleTypeUnknown for an import
// that wrote no attribute at all. The distinction is not cosmetic — Node
// requires the attribute for a JSON module and reports
// ERR_IMPORT_ATTRIBUTE_MISSING without it — and it cannot be recovered from
// the specifier, which is why it travels with the load request.
type ModuleType string

const (
	ModuleTypeUnknown    ModuleType = "unknown"
	ModuleTypeJavaScript ModuleType = "js"
	ModuleTypeJSON       ModuleType = "json"
)

// ModuleRequest is everything the engine knows about one import: what to load,
// who imported it, and what type the import declared.
type ModuleRequest struct {
	Specifier string
	Referrer  string
	Type      ModuleType
}

// ModuleLoaderFor resolves a module the way ModuleLoader does, with the
// import's declared type available. Install it with SetModuleLoaderFor; a
// loader installed this way supersedes the plain one.
type ModuleLoaderFor func(cfg Config, req ModuleRequest) (string, error)

// SetModuleLoader installs the fallback loader — the one consulted when no
// resolver registered with RegisterModuleResolver matches the specifier. It
// is called on a registry miss and returns the module's source. It replaces
// the default Config.FS loader. Pass nil to disable fallback loading (imports
// then fall back to the "module not registered" failure).
func (js *JS) SetModuleLoader(loader ModuleLoader) {
	js.env.loader = loader
}

// SetModuleLoaderFor installs the fallback loader in its richer form, which
// also receives the import's declared type (see ModuleRequest). It replaces
// any loader set with SetModuleLoader.
func (js *JS) SetModuleLoaderFor(loader ModuleLoaderFor) {
	js.env.loaderFor = loader
}

// RegisterModuleResolver installs loader for module specifiers beginning with
// prefix (e.g. "node:"), so several packages can each claim a specifier
// namespace without clobbering one another or the fallback loader. The
// longest matching registered prefix wins; specifiers matching no prefix go
// to the fallback (SetModuleLoader, or the default Config.FS loader). An
// empty prefix sets the fallback, exactly like SetModuleLoader. Registering
// nil removes the prefix's resolver. Not safe to call concurrently with
// evaluation.
func (js *JS) RegisterModuleResolver(prefix string, loader ModuleLoader) {
	if prefix == "" {
		js.env.loader = loader
		return
	}
	rs := js.env.resolvers
	for i := range rs {
		if rs[i].prefix == prefix {
			if loader == nil {
				js.env.resolvers = append(rs[:i], rs[i+1:]...)
			} else {
				rs[i].load = loader
			}
			return
		}
	}
	if loader == nil {
		return
	}
	rs = append(rs, prefixResolver{prefix: prefix, load: loader})
	sort.SliceStable(rs, func(i, j int) bool { return len(rs[i].prefix) > len(rs[j].prefix) })
	js.env.resolvers = rs
}

// prefixResolver is one RegisterModuleResolver entry. hostEnv.resolvers keeps
// them sorted longest-prefix-first so a linear scan finds the winner.
type prefixResolver struct {
	prefix string
	load   ModuleLoader
}

// defaultModuleLoader reads the resolved specifier from Config.FS. Access
// control lives in the FS (it may deny with fs.ErrPermission), so the loader
// simply reads. Relative specifiers arrive already resolved against the
// referrer, so a plain ReadFile suffices; bare specifiers arrive verbatim for
// the FS to map.
func defaultModuleLoader(cfg Config, specifier, referrer string) (string, error) {
	if cfg.FS == nil {
		return "", fmt.Errorf("cannot load module %q: no FS configured", specifier)
	}
	b, err := fs.ReadFile(cfg.FS, specifier)
	if err != nil {
		return "", fmt.Errorf("load module %q: %w", specifier, err)
	}
	return string(b), nil
}
