package spidermonkey_test

// End-to-end coverage of the JS-value bridge: completion values arrive typed
// (not stringified), objects and functions cross as handles with identity
// preserved, and the handles are navigable (Get/Set) and callable (Call).

import (
	"context"
	"math"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func evalValue(t *testing.T, js *spidermonkey.JS, src string) spidermonkey.Value {
	t.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	if r.Error != nil {
		t.Fatalf("Eval(%q) threw: %v", src, r.Error)
	}
	return r.Value
}

func TestTypedCompletionValues(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	if v := evalValue(t, js, `1 + 2 * 3`); v.Int() != 7 || v.IsObject() {
		t.Errorf("number: got %#v, want Int 7", v)
	}
	if v := evalValue(t, js, `0.5 + 0.25`); v.Float() != 0.75 {
		t.Errorf("float: got %v, want 0.75", v.Float())
	}
	if v := evalValue(t, js, `"a" + "b"`); v.String() != "ab" {
		t.Errorf("string: got %q, want \"ab\"", v.String())
	}
	if v := evalValue(t, js, `1 === 1`); v.Bool() != true {
		t.Errorf("bool: got %v, want true", v.Bool())
	}
	if v := evalValue(t, js, `null`); v.IsUndefined() || v.Export() != nil || v.String() != "null" {
		t.Errorf("null: got %#v, want null (defined, exports nil)", v)
	}
	if v := evalValue(t, js, `undefined`); !v.IsUndefined() {
		t.Errorf("undefined: got %#v, want undefined", v)
	}
}

func TestObjectCompletionKeepsIdentity(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	v := evalValue(t, js, `globalThis.o = { x: 1 }; o`)
	obj := v.Object()
	if obj == nil {
		t.Fatalf("completion value is not an object: %#v", v)
	}
	defer obj.Free()

	// Read through the handle: the same object the guest holds.
	x, err := obj.Get("x")
	if err != nil {
		t.Fatalf("Get(x): %v", err)
	}
	if x.Int() != 1 {
		t.Errorf("o.x = %v, want 1", x.Int())
	}

	// Mutate through the handle; the guest must see the same mutation —
	// identity, not a copy.
	if err := obj.Set("y", spidermonkey.ValueOf(2)); err != nil {
		t.Fatalf("Set(y): %v", err)
	}
	if got := evalValue(t, js, `o.y`); got.Int() != 2 {
		t.Errorf("guest sees o.y = %v, want 2 (handle must alias the guest object)", got.Int())
	}
}

func TestFunctionCompletionIsCallable(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	v := evalValue(t, js, `(function (a, b) { return a + b; })`)
	fn := v.Object()
	if fn == nil || !fn.IsFunction() {
		t.Fatalf("completion value is not a function: %#v", v)
	}
	defer fn.Free()

	sum, err := fn.Call(spidermonkey.ValueOf(20), spidermonkey.ValueOf(22))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if sum.Int() != 42 {
		t.Errorf("fn(20, 22) = %v, want 42", sum.Int())
	}

	// A guest throw during the call is a *JSError, not a transport failure.
	boom := evalValue(t, js, `(function () { throw new Error("boom"); })`).Object()
	defer boom.Free()
	if _, err := boom.Call(); err == nil {
		t.Errorf("expected the thrown Error to surface from Call")
	}
}

func TestCallMethodOnBuiltin(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	jsonVal, err := js.Global().Get("JSON")
	if err != nil {
		t.Fatalf("Get(JSON): %v", err)
	}
	jsonObj := jsonVal.Object()
	if jsonObj == nil {
		t.Fatalf("JSON is not an object")
	}
	defer jsonObj.Free()

	s, err := jsonObj.CallMethod("stringify", spidermonkey.ValueOf(42))
	if err != nil {
		t.Fatalf("JSON.stringify: %v", err)
	}
	if s.String() != "42" {
		t.Errorf("JSON.stringify(42) = %q, want \"42\"", s.String())
	}
}

func TestHostFuncReceivesObjectByIdentity(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// The callback retains the object handle; it is navigated AFTER the
	// evaluation returns (a callback must not re-enter its interpreter).
	var got *spidermonkey.Object
	err := js.Global().DefineFunc("take", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if len(args) == 1 {
			got = args[0].Object()
		}
		return spidermonkey.Undefined(), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	evalValue(t, js, `globalThis.box = { n: 5 }; take(box); box.n = 6;`)
	if got == nil {
		t.Fatalf("callback did not receive an object")
	}
	defer got.Free()

	// The guest mutated the object AFTER handing it over; the handle must see
	// the new value — it aliases the guest object rather than copying it.
	n, err := got.Get("n")
	if err != nil {
		t.Fatalf("Get(n): %v", err)
	}
	if n.Int() != 6 {
		t.Errorf("box.n via handle = %v, want 6 (post-call mutation must be visible)", n.Int())
	}
}

func TestHostFuncReturnsObject(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// A host function can hand a host-created object to the guest; the guest
	// receives the SAME object the host still holds.
	obj, err := js.NewObject()
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Free()
	if err := obj.Set("tag", spidermonkey.ValueOf("host")); err != nil {
		t.Fatal(err)
	}
	err = js.Global().DefineFunc("give", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return obj, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if v := evalValue(t, js, `give().tag`); v.String() != "host" {
		t.Errorf("give().tag = %q, want \"host\"", v.String())
	}
	// Guest-side mutation is visible through the host's handle: same object.
	evalValue(t, js, `give().extra = true;`)
	extra, err := obj.Get("extra")
	if err != nil {
		t.Fatal(err)
	}
	if !extra.Bool() {
		t.Errorf("obj.extra = %v, want true (guest mutation must alias the host handle)", extra.Export())
	}
}

func TestPrimitiveSetAndGet(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	g := js.Global()
	for name, v := range map[string]spidermonkey.Value{
		"hostNum":  spidermonkey.ValueOf(3.5),
		"hostStr":  spidermonkey.ValueOf("s"),
		"hostBool": spidermonkey.ValueOf(true),
		"hostNull": spidermonkey.Null(),
	} {
		if err := g.Set(name, v); err != nil {
			t.Fatalf("Set(%s): %v", name, err)
		}
	}
	if got := evalValue(t, js, `hostNum * 2`); got.Float() != 7 {
		t.Errorf("hostNum*2 = %v, want 7", got.Float())
	}
	if got := evalValue(t, js, `hostStr + "!"`); got.String() != "s!" {
		t.Errorf("hostStr+\"!\" = %q, want \"s!\"", got.String())
	}
	if got := evalValue(t, js, `hostBool && hostNull === null`); !got.Bool() {
		t.Errorf("hostBool/hostNull round-trip failed")
	}

	// And back out through Get.
	back, err := g.Get("hostNum")
	if err != nil {
		t.Fatal(err)
	}
	if back.Float() != 3.5 {
		t.Errorf("Get(hostNum) = %v, want 3.5", back.Float())
	}
	missing, err := g.Get("definitelyMissing")
	if err != nil {
		t.Fatal(err)
	}
	if !missing.IsUndefined() {
		t.Errorf("missing property = %#v, want undefined", missing)
	}
}

func TestHostFuncReentersInterpreter(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// The callback re-enters the paused interpreter: it navigates its object
	// argument, mutates it, and even runs a nested Eval — all mid-call. The
	// invoke lock is released for the callback's duration, so none of this
	// deadlocks.
	var seen float64
	err := js.Global().DefineFunc("inspect", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		obj := args[0].Object()
		if obj == nil {
			return nil, nil
		}
		n, err := obj.Get("n")
		if err != nil {
			return nil, err
		}
		seen = n.Float()
		if err := obj.Set("fromHost", spidermonkey.ValueOf(true)); err != nil {
			return nil, err
		}
		nested, err := js.Eval(context.Background(), `6 * 7`)
		if err != nil || nested.Error != nil {
			return nil, err
		}
		return nested.Value, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	v := evalValue(t, js, `globalThis.arg = { n: 5 }; inspect(arg)`)
	if seen != 5 {
		t.Errorf("callback read arg.n = %v, want 5 (re-entrant Get failed)", seen)
	}
	if v.Int() != 42 {
		t.Errorf("inspect(arg) = %v, want 42 (nested Eval result)", v.Int())
	}
	if got := evalValue(t, js, `arg.fromHost`); !got.Bool() {
		t.Errorf("arg.fromHost = %v, want true (re-entrant Set must be visible)", got.Export())
	}
}

func TestValueOfCompositeMaterializesGuestSide(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	g := js.Global()
	if err := g.Set("nums", spidermonkey.ValueOf([]int{1, 2, 3})); err != nil {
		t.Fatalf("Set(slice): %v", err)
	}
	if err := g.Set("conf", spidermonkey.ValueOf(map[string]any{"name": "sm", "level": 3})); err != nil {
		t.Fatalf("Set(map): %v", err)
	}

	// A Go slice arrives as a real JS Array...
	if v := evalValue(t, js, `Array.isArray(nums)`); !v.Bool() {
		t.Errorf("Array.isArray(nums) = %v, want true", v.Export())
	}
	if v := evalValue(t, js, `nums.reduce((a, b) => a + b, 0)`); v.Int() != 6 {
		t.Errorf("sum(nums) = %v, want 6", v.Int())
	}
	// ...and a Go map as a plain object with the same properties.
	if v := evalValue(t, js, `conf.name + ":" + conf.level`); v.String() != "sm:3" {
		t.Errorf("conf = %q, want \"sm:3\"", v.String())
	}

	// It carries data, not identity: each crossing is a fresh guest value.
	if err := g.Set("nums2", spidermonkey.ValueOf([]int{1, 2, 3})); err != nil {
		t.Fatal(err)
	}
	if v := evalValue(t, js, `nums === nums2`); v.Bool() {
		t.Errorf("two crossings must not alias the same guest array")
	}

	// Host functions can return composites the same way.
	err := g.DefineFunc("pair", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf([]string{"a", "b"}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := evalValue(t, js, `pair().join("-")`); v.String() != "a-b" {
		t.Errorf("pair().join = %q, want \"a-b\"", v.String())
	}
}

func TestNonFiniteNumbersCrossTheBridge(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// JS → Go: non-finite completion values arrive as real Go non-finite
	// floats (JSON has no NaN/Infinity literal; the codec tags them).
	if v := evalValue(t, js, `NaN`); !math.IsNaN(v.Float()) {
		t.Errorf("NaN completion = %v, want NaN", v.Float())
	}
	if v := evalValue(t, js, `Infinity`); !math.IsInf(v.Float(), 1) {
		t.Errorf("Infinity completion = %v, want +Inf", v.Float())
	}
	if v := evalValue(t, js, `Math.pow(0, -1)`); !math.IsInf(v.Float(), 1) {
		t.Errorf("Math.pow(0,-1) = %v, want +Inf", v.Float())
	}
	if v := evalValue(t, js, `-Infinity`); !math.IsInf(v.Float(), -1) {
		t.Errorf("-Infinity completion = %v, want -Inf", v.Float())
	}

	// Go → JS and back: Set/Get round-trips the specials.
	g := js.Global()
	if err := g.Set("hostNaN", spidermonkey.ValueOf(math.NaN())); err != nil {
		t.Fatalf("Set(NaN): %v", err)
	}
	if err := g.Set("hostInf", spidermonkey.ValueOf(math.Inf(-1))); err != nil {
		t.Fatalf("Set(-Inf): %v", err)
	}
	if v := evalValue(t, js, `Number.isNaN(hostNaN)`); !v.Bool() {
		t.Errorf("guest sees hostNaN = not NaN")
	}
	if v := evalValue(t, js, `hostInf === -Infinity`); !v.Bool() {
		t.Errorf("guest sees hostInf != -Infinity")
	}
}

func TestSymbolAndBigIntCrossByHandle(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// A symbol completion value crosses by handle (it has no host data form)
	// and stays === itself when handed back to the guest.
	sym := evalValue(t, js, `globalThis.s = Symbol("id"); s`)
	if sym.IsObject() || sym.IsUndefined() {
		t.Fatalf("symbol should be a non-object, defined value: %#v", sym)
	}
	if err := js.Global().Set("back", sym); err != nil {
		t.Fatalf("Set(symbol): %v", err)
	}
	if v := evalValue(t, js, `back === s`); !v.Bool() {
		t.Errorf("round-tripped symbol lost identity (back === s was false)")
	}

	// A bigint likewise.
	big := evalValue(t, js, `10n ** 30n`)
	if big.IsUndefined() {
		t.Fatalf("bigint completion should not be undefined")
	}
	if err := js.Global().Set("b", big); err != nil {
		t.Fatalf("Set(bigint): %v", err)
	}
	if v := evalValue(t, js, `b === 10n ** 30n`); !v.Bool() {
		t.Errorf("round-tripped bigint lost its value")
	}
}

func TestObjectStringUsesGuestToString(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	v := evalValue(t, js, `({ toString() { return "custom!"; } })`)
	obj := v.Object()
	if obj == nil {
		t.Fatalf("not an object: %#v", v)
	}
	defer obj.Free()
	if s := obj.String(); s != "custom!" {
		t.Errorf("Object.String() = %q, want \"custom!\"", s)
	}
}
