package nodejs_test

import (
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestTimersPromisesAbort verifies timers/promises setTimeout rejects with
// AbortError when its signal is (already) aborted.
func TestTimersPromisesAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setTimeout: sleep } = require("timers/promises");
			const c = new AbortController(); c.abort();
			try { await sleep(50, "v", { signal: c.signal }); r.outcome = "resolved"; }
			catch (e) { r.outcome = "rejected:" + e.name; }
		})().catch(e => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("aborted timers/promises = %q, want rejected:AbortError", got)
	}
}

// TestTimersPromisesAbortMidWait verifies aborting during the wait rejects and
// doesn't hang the loop.
func TestTimersPromisesAbortMidWait(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan struct{})
	go func() {
		runScript(t, rt, `
			globalThis.r = {};
			(async () => {
				const { setTimeout: sleep } = require("timers/promises");
				const c = new AbortController();
				setTimeout(() => c.abort(), 10);
				try { await sleep(100000, "v", { signal: c.signal }); r.outcome = "resolved"; }
				catch (e) { r.outcome = "rejected:" + e.name; }
			})().catch(e => { r.err = String(e); });
		`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timers/promises abort-mid-wait hung the loop")
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("abort-mid-wait = %q, want rejected:AbortError", got)
	}
}

// TestTimerRefresh verifies Timeout.refresh() re-arms the timer.
func TestTimerRefresh(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { fires: 0 };
		let n = 0;
		const t = setTimeout(function tick() {
			r.fires++;
			if (++n < 3) t.refresh(); // re-arm twice more
		}, 5);
	`)
	if got := evalVal(t, js, "r.fires").Int(); got != 3 {
		t.Errorf("timer fired %d times with refresh, want 3", got)
	}
}

// TestTimersPromisesSetIntervalAbort verifies timers/promises.setInterval honors
// an AbortSignal — the for-await loop throws AbortError instead of hanging.
func TestTimersPromisesSetIntervalAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { setInterval } = require("timers/promises");
		const ac = new AbortController();
		setTimeout(() => ac.abort(), 30);
		(async () => {
			try {
				let count = 0;
				for await (const _ of setInterval(10, "x", { signal: ac.signal })) { if (++count > 500) break; }
				r.result = "ended:" + count;
			} catch (e) { r.result = "threw:" + e.name; }
		})();
	`)
	if got := evalStr(t, js, `String(r.result ?? "HANG")`); got != "threw:AbortError" {
		t.Errorf("setInterval with aborted signal = %q, want threw:AbortError", got)
	}
}

// TestTimersPromisesSetIntervalNoListenerLeak verifies setInterval removes its
// per-tick abort listener on normal resolution, so a long loop on a shared
// AbortController doesn't accumulate listeners.
func TestTimersPromisesSetIntervalNoListenerLeak(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { setInterval } = require("timers/promises");
		const ac = new AbortController();
		(async () => {
			let count = 0;
			for await (const _ of setInterval(1, "x", { signal: ac.signal })) { if (++count >= 15) break; }
			r.leaked = (ac.signal._listeners.get("abort") || []).length;
		})();
	`)
	if got := evalStr(t, js, `String(r.leaked ?? "?")`); got != "0" {
		t.Errorf("abort listeners after 15 ticks + break = %q, want 0 (leak)", got)
	}
}
