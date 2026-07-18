package spidermonkey

import (
	"context"
	"fmt"
	"io/fs"
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
	return parseModuleResult(raw)
}

// SetModuleLoader installs a custom loader. It is called on a registry miss with
// the interpreter's Config, the resolved module specifier, and the specifier of
// the importing module (referrer); it returns the module's source. It replaces
// the default Config.FS loader. Pass nil to disable loading (imports then fall
// back to the "module not registered" failure).
func (js *JS) SetModuleLoader(loader func(cfg Config, specifier, referrer string) (string, error)) {
	js.env.loader = loader
}

// defaultModuleLoader reads the resolved specifier from Config.FS. Relative
// specifiers arrive already resolved against the referrer, so a plain ReadFile
// suffices; bare specifiers arrive verbatim for the FS to map.
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
