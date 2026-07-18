package internal

// Realises console.log purely through the per-instance JS surface: fetch the
// global, build a plain object, define a Go-backed `log` on it, and hang it off
// the global. This is the exact chain the public host-API layer drives —
// Global, NewPlainObject, DefineFunction, Set — each running against THIS
// interpreter's own module, so a green run proves those calls interlock end to
// end (including the guest->host dispatch that carries arguments back to Go)
// with no global module in sight.

import (
	"fmt"
	"strings"
	"testing"
)

func TestConsoleLogViaBridge(t *testing.T) {
	js, env := newJS(t)

	var logged []string
	var objectArgs []Value
	log := func(args []Value) (Value, error) {
		parts := make([]string, len(args))
		for i, a := range args {
			if a.IsObject() {
				objectArgs = append(objectArgs, a)
				parts[i] = "[object]"
				continue
			}
			parts[i] = fmt.Sprint(a.Interface())
		}
		logged = append(logged, strings.Join(parts, " "))
		return Undefined(), nil // console.log returns undefined
	}

	global, err := js.Global()
	if err != nil || global == 0 {
		t.Fatalf("Global: h=%d err=%v", global, err)
	}
	defer js.FreeObject(global)

	console, err := js.NewPlainObject()
	if err != nil || console == 0 {
		t.Fatalf("NewPlainObject: h=%d err=%v", console, err)
	}
	defer js.FreeObject(console)

	defineFunc(t, js, env, console, "log", "console.log", 1, log)

	if _, err := js.Set(global, "console", string(Value{kind: "object", h: console}.encode())); err != nil {
		t.Fatalf("Set(console): %v", err)
	}

	// The script sees a real console.log; primitive arguments arrive as data.
	mustEval(t, js, `console.log("hello", 42, true); console.log("second", [1,2,3]);`)

	want := []string{"hello 42 true", "second [object]"}
	if len(logged) != len(want) {
		t.Fatalf("logged %d lines, want %d: %#v", len(logged), len(want), logged)
	}
	for i := range want {
		if logged[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, logged[i], want[i])
		}
	}

	// The array argument crossed as a HANDLE to the same guest object — not a
	// copy: its elements are readable through the handle.
	if len(objectArgs) != 1 {
		t.Fatalf("object args = %d, want 1 (the [1,2,3] literal)", len(objectArgs))
	}
	arr := objectArgs[0]
	rawLen, err := js.Get(arr.Handle(), "length")
	if err != nil {
		t.Fatalf("Get(length): %v", err)
	}
	if v, _ := decodeEncoding([]byte(rawLen)); v.Float() != 3 {
		t.Errorf("array arg length = %v, want 3 (identity not preserved?)", v)
	}
	defer js.FreeObject(arr.Handle())

	// console.log must be a genuine callable property visible to script.
	if got := evalDisplay(t, js, `typeof console.log`); got != "function" {
		t.Errorf("typeof console.log = %q, want \"function\"", got)
	}
}
