package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestAddEventListenerSignalOption covers the WHATWG { signal } option on
// EventTarget.addEventListener: an already-aborted signal prevents the add,
// aborting later removes the listener, and the internal abort hook is cleaned
// up when the listener goes away by other means.
func TestAddEventListenerSignalOption(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// Already-aborted signal: listener is never added.
	if got := evalString(t, js, `
		(() => {
			const et = new EventTarget();
			let fired = 0;
			et.addEventListener("x", () => fired++, { signal: AbortSignal.abort() });
			et.dispatchEvent(new Event("x"));
			return String(fired);
		})()
	`); got != "0" {
		t.Errorf("pre-aborted signal: fired %s times, want 0", got)
	}

	// Aborting removes the listener; earlier dispatches still fire.
	if got := evalString(t, js, `
		(() => {
			const et = new EventTarget();
			const ac = new AbortController();
			let fired = 0;
			et.addEventListener("x", () => fired++, { signal: ac.signal });
			et.dispatchEvent(new Event("x"));
			ac.abort();
			et.dispatchEvent(new Event("x"));
			return String(fired);
		})()
	`); got != "1" {
		t.Errorf("abort removal: fired %s times, want 1", got)
	}

	// removeEventListener detaches the abort hook from the signal.
	if got := evalString(t, js, `
		(() => {
			const et = new EventTarget();
			const ac = new AbortController();
			const cb = () => {};
			et.addEventListener("x", cb, { signal: ac.signal });
			const withHook = (ac.signal._listeners.get("abort") || []).length;
			et.removeEventListener("x", cb);
			const afterRemove = (ac.signal._listeners.get("abort") || []).length;
			return withHook + "," + afterRemove;
		})()
	`); got != "1,0" {
		t.Errorf("abort hook cleanup on remove = %s, want 1,0", got)
	}

	// A once listener firing also detaches the abort hook.
	if got := evalString(t, js, `
		(() => {
			const et = new EventTarget();
			const ac = new AbortController();
			let fired = 0;
			et.addEventListener("x", () => fired++, { once: true, signal: ac.signal });
			et.dispatchEvent(new Event("x"));
			et.dispatchEvent(new Event("x"));
			const hooks = (ac.signal._listeners.get("abort") || []).length;
			ac.abort(); // must be a no-op for the event target now
			return fired + "," + hooks;
		})()
	`); got != "1,0" {
		t.Errorf("once + signal = %s, want 1,0", got)
	}

	// signal combines with once: abort BEFORE the first dispatch wins.
	if got := evalString(t, js, `
		(() => {
			const et = new EventTarget();
			const ac = new AbortController();
			let fired = 0;
			et.addEventListener("x", () => fired++, { once: true, signal: ac.signal });
			ac.abort();
			et.dispatchEvent(new Event("x"));
			return String(fired);
		})()
	`); got != "0" {
		t.Errorf("abort before dispatch: fired %s times, want 0", got)
	}
}
