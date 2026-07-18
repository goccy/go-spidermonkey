package internal

// Proves the surface is genuinely per-instance: two interpreters built from
// New() share nothing. Each drives its own wasm module (its own linear memory,
// so the C++ g_cx/g_global live in separate memory), which is exactly why the
// bridge can be switched per interpreter without a global module.

import "testing"

func TestPerInstanceIsolation(t *testing.T) {
	a, envA := newJS(t)
	b, _ := newJS(t)

	// Global state set on one interpreter is invisible to the other.
	mustEval(t, a, `globalThis.marker = "A";`)
	mustEval(t, b, `globalThis.marker = "B";`)
	if got := evalDisplay(t, a, `globalThis.marker`); got != "A" {
		t.Errorf("a.marker = %q, want \"A\"", got)
	}
	if got := evalDisplay(t, b, `globalThis.marker`); got != "B" {
		t.Errorf("b.marker = %q, want \"B\"", got)
	}

	// A host-backed method added to a's Array.prototype must not leak into b.
	protoA := arrayPrototype(t, a)
	defer a.FreeObject(protoA)
	defineFunc(t, a, envA, protoA, "onlyOnA", "a.onlyOnA", 0, func(args []Value) (Value, error) {
		return ValueOf(true), nil
	})
	if got := evalDisplay(t, a, `[].onlyOnA()`); got != "true" {
		t.Errorf("a [].onlyOnA() = %q, want \"true\"", got)
	}
	if got := evalDisplay(t, b, `typeof [].onlyOnA`); got != "undefined" {
		t.Errorf("b [].onlyOnA = %q, want \"undefined\" (must not leak across instances)", got)
	}
}
