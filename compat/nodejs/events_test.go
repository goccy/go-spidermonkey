package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
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

// events.on(emitter, event, { signal }): aborting the signal must REJECT the
// pending (and any later) next() with an AbortError so a for-await consumer
// sees it in catch — not end the iteration cleanly. An already-aborted signal
// throws synchronously. Regression: abort resolved next() with { done: true }.
func TestEventsOnAbortRejectsWithAbortError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { EventEmitter, on } = require("events");

		// Abort while a next() is pending: for-await must land in catch.
		(async () => {
			const ee = new EventEmitter();
			const ac = new AbortController();
			const seen = [];
			setTimeout(() => ee.emit("tick", 1), 5);
			setTimeout(() => ac.abort(), 15);
			try {
				for await (const [v] of on(ee, "tick", { signal: ac.signal })) {
					seen.push(v);
				}
				r.outcome = "ended-cleanly";
			} catch (e) {
				r.outcome = e.name + "/" + e.code;
			}
			r.seen = seen.join(",");
		})();

		// A subsequent next() after the abort also rejects.
		(async () => {
			const ee = new EventEmitter();
			const ac = new AbortController();
			const it = on(ee, "later", { signal: ac.signal });
			ac.abort();
			r.subsequent = await it.next().then(() => "resolved", (e) => e.name + "/" + e.code);
		})();

		// Pre-aborted signal: on() throws synchronously (Node behavior).
		try {
			const ac = new AbortController();
			ac.abort();
			on(new EventEmitter(), "x", { signal: ac.signal });
			r.preAborted = "no-throw";
		} catch (e) {
			r.preAborted = e.name + "/" + e.code;
		}
	`)
	for expr, want := range map[string]string{
		"r.outcome":    "AbortError/ABORT_ERR",
		"r.seen":       "1",
		"r.subsequent": "AbortError/ABORT_ERR",
		"r.preAborted": "AbortError/ABORT_ERR",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// EventEmitter.errorMonitor listeners fire FIRST on every emit('error', ...)
// without counting as handling the error: the regular listeners still run
// afterwards, and with no regular listener the emit still throws. Regression:
// emit() never consulted the errorMonitor symbol, so monitors never fired.
func TestEventEmitterErrorMonitor(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const EventEmitter = require("events");

		// Monitor + regular handler: monitor observes first.
		{
			const ee = new EventEmitter();
			const order = [];
			ee.on(EventEmitter.errorMonitor, (err) => order.push("monitor:" + err.message));
			ee.on("error", (err) => order.push("handler:" + err.message));
			ee.emit("error", new Error("boom"));
			r.order = order.join(",");
		}

		// Monitor only: it observes, but the error is still UNHANDLED (throws).
		{
			const ee = new EventEmitter();
			let monitored = "";
			ee.on(EventEmitter.errorMonitor, (err) => { monitored = err.message; });
			try {
				ee.emit("error", new Error("unhandled"));
				r.unhandled = "no-throw";
			} catch (e) {
				r.unhandled = e.message;
			}
			r.monitored = monitored;
		}

		// The monitor does not affect listenerCount("error") or eventNames().
		{
			const ee = new EventEmitter();
			ee.on(EventEmitter.errorMonitor, () => {});
			r.count = ee.listenerCount("error");
		}
	`)
	for expr, want := range map[string]string{
		"r.order":     "monitor:boom,handler:boom",
		"r.unhandled": "unhandled",
		"r.monitored": "unhandled",
		"r.count":     "0",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
