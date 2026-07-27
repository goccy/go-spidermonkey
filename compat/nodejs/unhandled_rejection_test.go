package nodejs_test

// process.on('unhandledRejection') — a rejection that reaches a microtask
// checkpoint with nothing to handle it.
//
// These are visible only to the engine: an async function's promise is created
// by the engine, so no host- or guest-side Promise wrapper ever sees it. The
// engine reports them to the embedder, and the event loop delivers them at each
// checkpoint.

import (
	"context"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestUnhandledRejectionFires(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => { r.reasons.push(String(reason)); });
		Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.reasons.join(",")`); got != "Error: boom" {
		t.Fatalf("unhandledRejection reasons = %q, want %q", got, "Error: boom")
	}
}

// The case no Promise wrapper can see: the rejected promise is the one the
// engine creates for the async function itself.
func TestUnhandledRejectionFromAsyncFunction(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => { r.reasons.push(String(reason)); });
		(async () => { throw new Error("async-boom"); })();
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `r.reasons.join(",")`); got != "Error: async-boom" {
		t.Fatalf("unhandledRejection reasons = %q, want %q", got, "Error: async-boom")
	}
}

// A rejection the guest handles within the same microtask checkpoint is NOT
// unhandled. This is the assertion that says the report waits for the
// checkpoint instead of firing the moment a promise rejects — including for a
// handler attached in a later MICROtask, which still lands inside the same
// checkpoint.
func TestRejectionHandledInSameCheckpointDoesNotFire(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { count: 0 };
		process.on("unhandledRejection", () => { r.count++; });
		Promise.reject(new Error("caught")).catch(() => {});
		const chained = Promise.reject(new Error("chained"));
		Promise.resolve().then(() => chained.catch(() => {}));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "0" {
		t.Fatalf("unhandledRejection fired %s times for handled rejections, want 0", got)
	}
}

// A handler attached in a later MACROtask is too late — the checkpoint has
// already passed, and the rejection was unhandled when it was reached. This is
// Node's behaviour, and it is what makes the event meaningful at all.
func TestRejectionHandledInALaterTaskStillFires(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { count: 0 };
		process.on("unhandledRejection", () => { r.count++; });
		const late = Promise.reject(new Error("late"));
		setTimeout(() => late.catch(() => {}), 0);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "1" {
		t.Fatalf("unhandledRejection fired %s times, want 1", got)
	}
}

// The rejected promise itself is the handler's second argument, and it is the
// same object the guest holds — not a copy.
func TestUnhandledRejectionCarriesThePromise(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.on("unhandledRejection", (reason, promise) => { r.same = promise === globalThis.p; });
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
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.same)`); got != "true" {
		t.Fatalf("handler received promise identity = %s, want true", got)
	}
}

// With no 'unhandledRejection' listener, Node routes the rejection to
// 'uncaughtException' with origin 'unhandledRejection'.
func TestUnhandledRejectionFallsBackToUncaughtException(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.on("uncaughtException", (err, origin) => { r.origin = origin; r.code = err.code; });
		Promise.reject(new Error("boom"));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.origin)`); got != "unhandledRejection" {
		t.Fatalf("uncaughtException origin = %q, want %q", got, "unhandledRejection")
	}
}

// A rejection raised from inside an unhandledRejection handler is itself
// reported: the checkpoint repeats until a pass finds nothing.
func TestUnhandledRejectionFromWithinHandler(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		globalThis.r = { reasons: [] };
		process.on("unhandledRejection", (reason) => {
			r.reasons.push(String(reason));
			if (r.reasons.length === 1) Promise.reject("second");
		});
		Promise.reject("first");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := evalStr(t, js, `r.reasons.join(",")`)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("reasons = %q, want both the original and the handler's rejection", got)
	}
}
