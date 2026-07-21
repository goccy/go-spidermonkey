package spidermonkey_test

// Coverage of the composition primitives: NewFunction (a Go closure as a
// standalone guest function — FuncOf) and Object.New (constructing a guest
// class from Go). Together they let a host API be assembled entirely from Go.

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestNewFunctionIsCallableEverywhere(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	add, err := js.NewFunction("add", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(args[0].Float() + args[1].Float()), nil
	})
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}
	defer add.Free()

	// Callable directly from Go.
	if v, err := add.Call(spidermonkey.ValueOf(20), spidermonkey.ValueOf(22)); err != nil || v.Int() != 42 {
		t.Errorf("direct Call = %v (%v), want 42", v, err)
	}

	// Composable as an object property, callable from the guest.
	if err := js.Global().Set("add", add); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v := evalValue(t, js, `add(1, 2) + add.name.length`); v.Int() != 6 { // 3 + len("add")
		t.Errorf("guest call = %v, want 6", v.Int())
	}

	// Passable as a callback argument with identity preserved.
	callTwice := evalValue(t, js, `(f => f(1, 2) + f(3, 4))`).Object()
	defer callTwice.Free()
	if v, err := callTwice.Call(add); err != nil || v.Int() != 10 {
		t.Errorf("callback use = %v (%v), want 10", v, err)
	}

	// Several anonymous functions never collide, even with the same name.
	one, _ := js.NewFunction("f", func(spidermonkey.Config, []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(1), nil
	})
	two, _ := js.NewFunction("f", func(spidermonkey.Config, []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(2), nil
	})
	defer one.Free()
	defer two.Free()
	v1, err1 := one.Call()
	v2, err2 := two.Call()
	if err1 != nil || err2 != nil || v1.Int() != 1 || v2.Int() != 2 {
		t.Errorf("same-name functions collided: %v/%v (%v/%v)", v1, v2, err1, err2)
	}
}

func TestObjectNewConstructsClasses(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// A guest class: js_call alone cannot instantiate it ([[Call]] on a class
	// throws); New performs [[Construct]].
	cls := evalValue(t, js, `(class Point { constructor(x, y) { this.x = x; this.y = y; } sum() { return this.x + this.y; } })`).Object()
	defer cls.Free()

	inst, err := cls.New(spidermonkey.ValueOf(40), spidermonkey.ValueOf(2))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	obj := inst.Object()
	if obj == nil {
		t.Fatalf("New did not return an object")
	}
	defer obj.Free()
	if v, err := obj.CallMethod("sum"); err != nil || v.Int() != 42 {
		t.Errorf("instance.sum() = %v (%v), want 42", v, err)
	}

	// Builtins construct too.
	mapCls := evalValue(t, js, `Map`).Object()
	defer mapCls.Free()
	m, err := mapCls.New()
	if err != nil {
		t.Fatalf("new Map(): %v", err)
	}
	mObj := m.Object()
	defer mObj.Free()
	if _, err := mObj.CallMethod("set", spidermonkey.ValueOf("k"), spidermonkey.ValueOf(7)); err != nil {
		t.Fatalf("map.set: %v", err)
	}
	if v, err := mObj.CallMethod("get", spidermonkey.ValueOf("k")); err != nil || v.Int() != 7 {
		t.Errorf("map.get = %v (%v), want 7", v, err)
	}

	// A non-constructor is a clean error, not a crash.
	arrow := evalValue(t, js, `(() => 1)`).Object()
	defer arrow.Free()
	if _, err := arrow.New(); err == nil {
		t.Errorf("New on an arrow function: want an error")
	}

	// A guest throw during construction surfaces as an error.
	boom := evalValue(t, js, `(class { constructor() { throw new Error("boom"); } })`).Object()
	defer boom.Free()
	if _, err := boom.New(); err == nil {
		t.Errorf("throwing constructor: want an error")
	}
}
