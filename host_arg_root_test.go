package spidermonkey_test

// A host op that reads an object-typed argument as a PRIMITIVE must not pin it.
//
// Every non-primitive argument crosses the bridge as a handle, and a handle is
// a persistent GC root: decoding one pins the object and its whole reachable
// backing store. An op that reads the argument with Int/String/Float/Bool never
// sees the handle, so it cannot release one — and a guest reaches that path
// just by passing an object where a number is expected. Left unreleased it is
// unbounded, guest-triggerable memory exhaustion.

import (
	"context"
	"fmt"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// evalOK runs src and fails the test on either a host error or a guest throw —
// a leak surfaces as an out-of-memory throw, which is a Result.Error, not a Go
// error.
func evalOK(t *testing.T, js *spidermonkey.JS, src string) spidermonkey.Value {
	t.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("guest threw: %v", r.Error)
	}
	return r.Value
}

// scalarReadOfObjectArgDoesNotPin drives `calls` iterations of a host op that
// reads args[0] with read, passing a fresh 4 MiB typed array each time. Pinned,
// they add up to well past the cap; released, each iteration frees a block the
// next one takes straight back.
//
// The churn is deliberately just past the cap rather than many times it: linear
// memory never shrinks, so every allocation the guest fails to reuse is
// permanent, and pushing a gigabyte through a 128 MiB window makes the test
// depend on how well dlmalloc recycles rather than on whether the root was
// released.
func scalarReadOfObjectArgDoesNotPin(t *testing.T, read func(spidermonkey.Value)) {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 128 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	if err := js.Global().DefineFunc("take", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		read(args[0])
		return spidermonkey.Undefined(), nil
	}); err != nil {
		t.Fatalf("DefineFunc: %v", err)
	}

	const calls = 48 // 192 MiB of backing store if every argument stays pinned
	src := fmt.Sprintf(`for (let i = 0; i < %d; i++) take(new Uint8Array(4 * 1024 * 1024)); "ok"`, calls)
	evalOK(t, js, src)
}

func TestScalarReadOfObjectArgDoesNotPinIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(spidermonkey.Value)
	}{
		{"Int", func(v spidermonkey.Value) { _ = v.Int() }},
		{"Float", func(v spidermonkey.Value) { _ = v.Float() }},
		{"Bool", func(v spidermonkey.Value) { _ = v.Bool() }},
		{"IsUndefined", func(v spidermonkey.Value) { _ = v.IsUndefined() }},
		{"IsObject", func(v spidermonkey.Value) { _ = v.IsObject() }},
		{"ignored", func(v spidermonkey.Value) {}},
	} {
		t.Run(tc.name, func(t *testing.T) { scalarReadOfObjectArgDoesNotPin(t, tc.read) })
	}
}

// Taking the argument as an object transfers the pin to the op — which is what
// lets an op retain a guest callback — so an op that takes and never frees
// still leaks. That is the documented contract, and this pins it down so the
// dispatcher's release can never quietly start freeing retained handles.
func TestTakingObjectArgTransfersOwnership(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	var held *spidermonkey.Object
	if err := js.Global().DefineFunc("keep", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		held = args[0].Object()
		return spidermonkey.Undefined(), nil
	}); err != nil {
		t.Fatalf("DefineFunc: %v", err)
	}
	evalOK(t, js, `keep({ v: 42 }); "ok"`)
	if held == nil {
		t.Fatal("host op did not receive the object")
	}
	// The guest dropped its only reference; the op's handle must still be the
	// live object, not a released root.
	evalOK(t, js, `for (let i = 0; i < 5000; i++) ({ i }); "gc churn"`)
	v, err := held.Get("v")
	if err != nil {
		t.Fatalf("Get on the retained object: %v", err)
	}
	if v.Int() != 42 {
		t.Fatalf("retained object lost its contents: v = %v, want 42", v.Int())
	}
	if err := held.Free(); err != nil {
		t.Fatalf("Free: %v", err)
	}
}

// A host op returning one of its own arguments must not have that argument
// released out from under the reply.
func TestReturningAnArgumentKeepsItAlive(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	if err := js.Global().DefineFunc("echo", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return args[0], nil
	}); err != nil {
		t.Fatalf("DefineFunc: %v", err)
	}
	v := evalOK(t, js, `const o = { v: 7 }; echo(o) === o ? "same" : "different"`)
	if got := v.String(); got != "same" {
		t.Fatalf("echo(o) === o: %q, want %q", got, "same")
	}
}

// Symbols and bigints cross by handle too, with no *Object to hang Free on, so
// the dispatcher is the only thing that can ever release one. Releasing them is
// therefore new behaviour on a value kind the host cannot inspect: this checks
// it neither corrupts the guest's own symbol nor breaks a repeat caller. (The
// release itself is measured on the object case above; a symbol's payload is
// GC-managed string data, which does not hold linear memory predictably enough
// to assert on.)
func TestOpaqueArgIsReleasedWithoutCorruptingTheGuest(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	if err := js.Global().DefineFunc("take", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(args[0].String()), nil
	}); err != nil {
		t.Fatalf("DefineFunc: %v", err)
	}
	// The guest keeps its own reference to s across the call and past a GC: if
	// releasing the handle disturbed the value itself, identity or the
	// description would not survive.
	got := evalOK(t, js, `
		const s = Symbol("keeper");
		const big = 2n ** 96n;
		for (let i = 0; i < 5000; i++) { take(s); take(big); take(Symbol(i)); ({ i }); }
		s.description === "keeper" && s === s && big === 2n ** 96n ? "intact" : "damaged"`)
	if got.String() != "intact" {
		t.Fatalf("symbol/bigint after repeated host calls: %q, want %q", got.String(), "intact")
	}
}
