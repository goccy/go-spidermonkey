package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// An earlier timer's callback can cancel a later timer that is due in the SAME
// tick — Node guarantees the cancelled callback does not run. The loop takes
// all due timers as a batch, so the fix must let clearTimeout reach a sibling
// already removed from the pending map.
func TestClearTimeoutCancelsSameTickSibling(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		let b;
		// a is registered first and, when it runs, cancels b — which is due in
		// the same tick and already taken into the loop's due batch.
		setTimeout(() => { __order.push("a"); clearTimeout(b); }, 0);
		b = setTimeout(() => { __order.push("b"); }, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "a" {
		t.Fatalf("order = %q, want just \"a\" (b must be cancelled by a)", got)
	}
}

// Inside a callback, setImmediate fires before a setTimeout(0) scheduled
// alongside it: the immediate runs in this turn's check phase, while the timer
// waits for the next turn's timers phase.
func TestImmediateBeforeTimeoutInsideCallback(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		setTimeout(() => {
			setTimeout(() => { __order.push("timeout"); }, 0);
			setImmediate(() => { __order.push("immediate"); });
		}, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "immediate,timeout" {
		t.Fatalf("order = %q, want immediate,timeout", got)
	}
}

// Clearing a timer that already fired in the SAME tick must be a harmless
// no-op, not a double-free of its (already-freed) callback handle.
func TestClearAlreadyFiredSameTickTimer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		// a fires first (and its handle is freed); b then clears a — already fired.
		const a = setTimeout(() => { __order.push("a"); }, 0);
		setTimeout(() => { clearTimeout(a); __order.push("b"); }, 0);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "a,b" {
		t.Fatalf("order = %q, want a,b (clearing a fired timer must not crash)", got)
	}
}
