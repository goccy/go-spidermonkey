package web_test

// The web `unhandledrejection` event: a promise rejection that reached a
// microtask checkpoint with nothing to handle it, dispatched on globalThis and
// cancelable by preventDefault().

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
