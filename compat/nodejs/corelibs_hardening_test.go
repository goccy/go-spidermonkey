package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

func TestEventEmitterMetaEvents(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = { meta: [] };
		const { EventEmitter } = require("events");
		const e = new EventEmitter();
		e.on("newListener", (type) => { __r.meta.push("new:" + type); });
		e.on("removeListener", (type) => { __r.meta.push("rm:" + type); });
		const fn = () => {};
		e.on("data", fn);   // fires newListener:data
		e.off("data", fn);  // fires removeListener:data
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	// Adding the removeListener listener itself fires newListener (Node
	// semantics: newListener fires for every add while a listener exists).
	if got := evalStr(t, js, `__r.meta.join(",")`); got != "new:removeListener,new:data,rm:data" {
		t.Fatalf("meta events = %q, want new:removeListener,new:data,rm:data", got)
	}
}

func TestUtilPromisifyCustom(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		const util = require("util");
		function fn() {}
		fn[util.promisify.custom] = () => Promise.resolve("custom-impl");
		const p = util.promisify(fn);
		p().then((v) => { __r.value = v; });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__r.value ?? ""`); got != "custom-impl" {
		t.Fatalf("promisify.custom = %q, want custom-impl", got)
	}
}

func TestRequireMainIsModuleInEntry(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		__r.isMain = (require.main === module);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if evalStr(t, js, `String(__r.isMain)`) != "true" {
		t.Fatalf("require.main === module should be true in the entry script")
	}
}

// require.cache is keyed by the absolute filename that require.resolve returns,
// so cache lookups/deletions by require.resolve(id) work.
func TestRequireCacheKeyedByResolvedPath(t *testing.T) {
	fsys := memfs.New()
	fsys.WriteFile("mod.js", []byte("module.exports = { n: 1 };"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__r = {};
		require("/mod.js");
		const key = require.resolve("/mod.js");
		__r.cached = !!require.cache[key];
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if evalStr(t, js, `String(__r.cached)`) != "true" {
		t.Fatalf("require.cache[require.resolve(id)] missed — key mismatch")
	}
}
