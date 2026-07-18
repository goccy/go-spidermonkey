package internal

// Reaches a built-in the host never created — Array.prototype, via
// global.Array.prototype — and mutates it through the per-instance JS surface:
// adds a brand-new Go-backed method that every array instance can call, and
// overrides an existing built-in method so its behaviour changes. This exercises
// Get (handles to objects the host did not make) together with
// DefineFunction on an arbitrary target object, all on THIS interpreter's module.

import "testing"

// arrayPrototype returns a handle to Array.prototype on js.
func arrayPrototype(t *testing.T, js *JS) uint64 {
	t.Helper()
	global, err := js.Global()
	if err != nil || global == 0 {
		t.Fatalf("Global: h=%d err=%v", global, err)
	}
	defer js.FreeObject(global)
	arrayCtor := getObject(t, js, global, "Array")
	defer js.FreeObject(arrayCtor)
	return getObject(t, js, arrayCtor, "prototype")
}

func TestArrayPrototypeAddMethod(t *testing.T) {
	js, env := newJS(t)
	proto := arrayPrototype(t, js)
	defer js.FreeObject(proto)

	// A new method available on every array. It returns a host-computed
	// string built from its arguments (host_func_call passes call arguments,
	// not `this`, so the method is defined in terms of what it is given).
	defineFunc(t, js, env, proto, "labeledWith", "Array.prototype.labeledWith", 1, func(args []Value) (Value, error) {
		label := ""
		if len(args) > 0 {
			label = args[0].String()
		}
		return ValueOf("labeled:" + label), nil
	})

	// Reachable on an array literal and on a constructed array alike.
	if got := evalDisplay(t, js, `[9,8,7].labeledWith("x")`); got != "labeled:x" {
		t.Errorf("[].labeledWith = %q, want \"labeled:x\"", got)
	}
	if got := evalDisplay(t, js, `new Array(3).labeledWith("y")`); got != "labeled:y" {
		t.Errorf("new Array().labeledWith = %q, want \"labeled:y\"", got)
	}
	// It is a real own function on the prototype, shared by instances.
	if got := evalDisplay(t, js, `typeof Array.prototype.labeledWith`); got != "function" {
		t.Errorf("typeof Array.prototype.labeledWith = %q, want \"function\"", got)
	}
	// nargs=1 sets the declared arity (.length). It does NOT cap arguments:
	// every JS function is variadic, so extra args are simply ignored by a
	// method that only reads args[0].
	if got := evalDisplay(t, js, `Array.prototype.labeledWith.length`); got != "1" {
		t.Errorf("labeledWith.length = %q, want \"1\" (nargs must set arity)", got)
	}
	if got := evalDisplay(t, js, `[1].labeledWith("z", 2, 3, 4)`); got != "labeled:z" {
		t.Errorf("labeledWith with extra args = %q, want \"labeled:z\" (must stay variadic)", got)
	}
}

func TestArrayPrototypeOverrideMethod(t *testing.T) {
	js, env := newJS(t)

	// Native behaviour first, so the change is provably a change.
	if got := evalDisplay(t, js, `[10,20,30].indexOf(20)`); got != "1" {
		t.Fatalf("native [].indexOf(20) = %q, want \"1\"", got)
	}

	proto := arrayPrototype(t, js)
	defer js.FreeObject(proto)

	// Redefine the built-in indexOf so it returns whatever the host says —
	// here a sentinel that native indexOf could never produce.
	defineFunc(t, js, env, proto, "indexOf", "Array.prototype.indexOf", 1, func(args []Value) (Value, error) {
		return ValueOf(-42), nil
	})

	if got := evalDisplay(t, js, `[10,20,30].indexOf(20)`); got != "-42" {
		t.Errorf("overridden [].indexOf(20) = %q, want \"-42\"", got)
	}
	// Even a value that is genuinely absent now yields the sentinel, proving
	// the native implementation no longer runs.
	if got := evalDisplay(t, js, `[1,2,3].indexOf(999)`); got != "-42" {
		t.Errorf("overridden [].indexOf(999) = %q, want \"-42\"", got)
	}
}
