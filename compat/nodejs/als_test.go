package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

func TestAsyncLocalStorage(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { AsyncLocalStorage, AsyncResource } = require("async_hooks");
		const als = new AsyncLocalStorage();
		globalThis.r = {};
		// Synchronous run: getStore inside sees the store.
		r.sync = als.run({ id: 1 }, () => als.getStore().id);
		// Nested run.
		r.nested = als.run({ id: 1 }, () => als.run({ id: 2 }, () => als.getStore().id) + ":" + als.getStore().id);
		// AsyncResource.bind captures context for a deferred callback.
		let captured;
		als.run({ id: 42 }, () => { captured = AsyncResource.bind(() => als.getStore().id); });
		r.bound = captured();  // runs outside the run(), but under captured context
		r.outside = als.getStore();  // undefined outside any run
	`)
	if got := evalStr(t, js, "String(r.sync)"); got != "1" {
		t.Errorf("sync = %q", got)
	}
	if got := evalStr(t, js, "r.nested"); got != "2:1" {
		t.Errorf("nested = %q", got)
	}
	if got := evalStr(t, js, "String(r.bound)"); got != "42" {
		t.Errorf("AsyncResource.bind = %q, want 42", got)
	}
	if got := evalStr(t, js, "String(r.outside)"); got != "undefined" {
		t.Errorf("outside store = %q", got)
	}
}
