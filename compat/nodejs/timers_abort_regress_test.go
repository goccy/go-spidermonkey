package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// timers/promises.setInterval's async iterator only checked signal.aborted
// before the loop: an abort landing while the consumer's for-await body ran
// (between ticks) was missed, because the next tick re-added an 'abort'
// listener to an already-aborted signal (which never fires) — the loop spun
// forever. Every iteration must re-check and reject with the abort reason.
func TestTimersPromisesSetIntervalAbortBetweenTicks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setInterval } = require("timers/promises");
			const ac = new AbortController();
			let ticks = 0;
			try {
				for await (const v of setInterval(5, "tick", { signal: ac.signal })) {
					ticks++;
					// Abort while the consumer body is running: no wait is
					// pending, so only a per-iteration re-check can see it.
					ac.abort();
				}
				r.outcome = "loop-exited-without-throw";
			} catch (e) {
				r.outcome = e && e.name;
				r.ticks = ticks;
			}
		})();
	`)
	if got := evalStr(t, js, `r.outcome`); got != "AbortError" {
		t.Errorf("outcome = %q, want AbortError", got)
	}
	if got := evalStr(t, js, `String(r.ticks)`); got != "1" {
		t.Errorf("ticks before abort surfaced = %s, want 1", got)
	}
}

// The abort reason must propagate (not be replaced by a generic error).
func TestTimersPromisesSetIntervalAbortReason(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setInterval } = require("timers/promises");
			const ac = new AbortController();
			try {
				for await (const v of setInterval(5, null, { signal: ac.signal })) {
					ac.abort(new Error("custom reason"));
				}
			} catch (e) { r.msg = e && e.message; }
		})();
	`)
	if got := evalStr(t, js, `r.msg`); got != "custom reason" {
		t.Errorf("rejection message = %q, want the abort reason", got)
	}
}

// scheduler.wait(ms, { signal }) ignored its options entirely; it must be
// abortable like timers/promises setTimeout (pre-aborted and mid-wait).
func TestSchedulerWaitHonorsAbortSignal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { scheduler } = require("timers/promises");
		// Pre-aborted: reject immediately.
		{
			const ac = new AbortController();
			ac.abort();
			scheduler.wait(5, { signal: ac.signal }).then(
				() => { r.pre = "resolved"; },
				(e) => { r.pre = e && e.name; });
		}
		// Abort mid-wait: reject well before the timer would fire.
		{
			const ac = new AbortController();
			const t0 = Date.now();
			scheduler.wait(10000, { signal: ac.signal }).then(
				() => { r.mid = "resolved"; },
				(e) => { r.mid = e && e.name; r.fast = (Date.now() - t0) < 5000; });
			setTimeout(() => ac.abort(), 10);
		}
		// No signal: still resolves.
		scheduler.wait(5).then(() => { r.plain = "resolved"; });
	`)
	for expr, want := range map[string]string{
		"r.pre":          "AbortError",
		"r.mid":          "AbortError",
		"String(r.fast)": "true",
		"r.plain":        "resolved",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// timers/promises.setImmediate only checked aborted at call time; an abort
// BEFORE the immediate fires must cancel it and reject with an AbortError.
func TestTimersPromisesSetImmediateAbortBeforeFiring(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const tp = require("timers/promises");
		// Abort after scheduling but before the check phase runs it.
		{
			const ac = new AbortController();
			tp.setImmediate("v", { signal: ac.signal }).then(
				(v) => { r.aborted = "resolved:" + v; },
				(e) => { r.aborted = e && e.name; });
			ac.abort();
		}
		// Abort reason propagates.
		{
			const ac = new AbortController();
			tp.setImmediate("v", { signal: ac.signal }).then(
				() => { r.reason = "resolved"; },
				(e) => { r.reason = e && e.message; });
			ac.abort(new Error("stop now"));
		}
		// Un-aborted still resolves with the value.
		{
			const ac = new AbortController();
			tp.setImmediate("ok", { signal: ac.signal }).then((v) => { r.value = v; });
		}
		// No options at all keeps working (resolves with the value).
		tp.setImmediate("bare").then((v) => { r.bare = v; });
	`)
	for expr, want := range map[string]string{
		"r.aborted": "AbortError",
		"r.reason":  "stop now",
		"r.value":   "ok",
		"r.bare":    "bare",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
