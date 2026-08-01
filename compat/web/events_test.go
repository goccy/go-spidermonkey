package web_test

import (
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
	"time"
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

// The web `unhandledrejection` event: a promise rejection that reached a
// microtask checkpoint with nothing to handle it, dispatched on globalThis and
// cancelable by preventDefault().

func TestUnhandledRejectionEventFires(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [], types: [] };
		addEventListener("unhandledrejection", (ev) => {
			ev.preventDefault();
			r.types.push(ev.type);
			r.reasons.push(String(ev.reason));
			r.same = ev.promise === globalThis.p;
		});
		globalThis.p = Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `r.reasons.join(",")`); got != "Error: boom" {
		t.Fatalf("reason = %q, want %q", got, "Error: boom")
	}
	if got := evalString(t, js, `r.types.join(",")`); got != "unhandledrejection" {
		t.Fatalf("event type = %q, want %q", got, "unhandledrejection")
	}
	if got := evalString(t, js, `String(r.same)`); got != "true" {
		t.Fatalf("ev.promise identity = %s, want true", got)
	}
}

func TestHandledRejectionDoesNotDispatch(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { count: 0 };
		addEventListener("unhandledrejection", (ev) => { ev.preventDefault(); r.count++; });
		Promise.reject(new Error("caught")).catch(() => {});
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `String(r.count)`); got != "0" {
		t.Fatalf("unhandledrejection dispatched %s times for a handled rejection, want 0", got)
	}
}

// onunhandledrejection is the event-handler attribute form and must work too.
func TestUnhandledRejectionHandlerAttribute(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = {};
		globalThis.onunhandledrejection = (ev) => { ev.preventDefault(); r.reason = String(ev.reason); };
		Promise.reject("attr-boom");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `String(r.reason)`); got != "attr-boom" {
		t.Fatalf("onunhandledrejection reason = %q, want %q", got, "attr-boom")
	}
}
