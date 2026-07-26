package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestEventsOnceAbort verifies events.once rejects with AbortError when its
// signal aborts before the event fires.
func TestEventsOnceAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { EventEmitter, once } = require("events");
		globalThis.r = {};
		const ee = new EventEmitter();
		const c = new AbortController();
		(async () => {
			try { await once(ee, "x", { signal: c.signal }); r.outcome = "resolved"; }
			catch (e) { r.outcome = "rejected:" + e.name; }
		})();
		c.abort();
	`)
	if got := evalStr(t, js, "r.outcome"); got != "rejected:AbortError" {
		t.Errorf("events.once abort = %q, want rejected:AbortError", got)
	}
}
