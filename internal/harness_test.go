package internal

// Test harness for the per-instance JS surface (js.go). Each test builds its own
// interpreter via New(env) — its own wasm module, its own runtime — so nothing
// here touches a global module. env routes guest->host calls to a keyed registry
// so DefineFunction'd stubs reach Go.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/goccy/spidermonkeywasm2go/base"
)

// Value is a JavaScript value crossing the host boundary as a value ENCODING —
// the identity-preserving {"k":...} tag the C++ codec speaks. A primitive
// carries its data; an object or function carries the handle of a persistent
// root, so the host holds the SAME guest object. It mirrors the Value type the
// public host-API layer exposes.
type Value struct {
	kind string // "undefined","null","bool","number","string","object","function"
	v    any    // primitive payload
	h    uint64 // object/function handle
}

// ValueOf wraps a Go primitive as a host return. Numbers, bools, strings and
// nil are delivered to JS as the matching JS value.
func ValueOf(x any) Value {
	switch v := x.(type) {
	case nil:
		return Value{kind: "undefined"}
	case bool:
		return Value{kind: "bool", v: v}
	case string:
		return Value{kind: "string", v: v}
	case float64:
		return Value{kind: "number", v: v}
	case int:
		return Value{kind: "number", v: float64(v)}
	default:
		return Value{kind: "string", v: fmt.Sprint(x)}
	}
}

// Undefined is the host return meaning "no value".
func Undefined() Value { return Value{kind: "undefined"} }

func (v Value) IsUndefined() bool { return v.kind == "undefined" }
func (v Value) IsObject() bool    { return v.kind == "object" || v.kind == "function" }
func (v Value) Handle() uint64    { return v.h }
func (v Value) String() string    { s, _ := v.v.(string); return s }
func (v Value) Float() float64    { f, _ := v.v.(float64); return f }
func (v Value) Int() int          { return int(v.Float()) }
func (v Value) Bool() bool        { b, _ := v.v.(bool); return b }
func (v Value) Interface() any    { return v.v }

// encoding is the wire form of a Value.
type encoding struct {
	K string `json:"k"`
	V any    `json:"v,omitempty"`
	H uint64 `json:"h,omitempty"`
}

func decodeEncoding(data []byte) (Value, error) {
	var e encoding
	if err := json.Unmarshal(data, &e); err != nil {
		return Value{}, err
	}
	return Value{kind: e.K, v: e.V, h: e.H}, nil
}

func (v Value) encode() []byte {
	b, _ := json.Marshal(encoding{K: v.kind, V: v.v, H: v.h})
	return b
}

// Func is a guest->host callback: typed JS arguments in, one typed value out
// (or an error, surfaced to the guest as a thrown Error).
type Func func(args []Value) (Value, error)

// testEnv implements base.EnvImports and dispatches guest host-calls to a keyed
// registry. One env per interpreter, so host state is per-instance too.
type testEnv struct {
	mu     sync.Mutex
	funcs  map[string]Func
	loader func(specifier, referrer string) (string, error) // ES module loader
	stash  []byte
}

func (e *testEnv) register(key string, fn Func) {
	e.mu.Lock()
	e.funcs[key] = fn
	e.mu.Unlock()
}

func (e *testEnv) Go_host_call(m *base.Module, keyPtr, keyLen, argsPtr, argsLen int32, thisID int64, outPtr, outCap int32) int32 {
	var key string
	var argsJSON []byte
	base.AccessMemory(m, func(mem []byte) {
		key = string(mem[keyPtr : keyPtr+keyLen])
		argsJSON = append([]byte(nil), mem[argsPtr:argsPtr+argsLen]...)
	})
	payload := e.dispatch(key, argsJSON)
	if int32(len(payload)) <= outCap {
		base.AccessMemory(m, func(mem []byte) { copy(mem[outPtr:], payload) })
	} else {
		e.stash = payload
	}
	return int32(len(payload))
}

func (e *testEnv) Go_host_result(m *base.Module, outPtr int32) {
	p := e.stash
	e.stash = nil
	base.AccessMemory(m, func(mem []byte) { copy(mem[outPtr:], p) })
}

// dispatch runs one call and builds the reply envelope the C++ side expects:
// 'R' + one value encoding on success, 'E' + message on error.
func (e *testEnv) dispatch(key string, argsJSON []byte) []byte {
	// The reserved module-loader key: args are [specifier, referrer]; the reply
	// is 'R' + raw source (not a value encoding).
	if key == ModuleLoaderKey {
		if e.loader == nil {
			return nil // no loader: total==0 → C++ falls back to missing-modules
		}
		var a []string
		_ = json.Unmarshal(argsJSON, &a)
		spec, ref := "", ""
		if len(a) > 0 {
			spec = a[0]
		}
		if len(a) > 1 {
			ref = a[1]
		}
		src, err := e.loader(spec, ref)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return append([]byte{'R'}, src...)
	}

	e.mu.Lock()
	fn, ok := e.funcs[key]
	e.mu.Unlock()
	if !ok {
		return []byte("Ehost function not registered: " + key)
	}
	// Arguments arrive as a JSON array of value encodings.
	var raw []json.RawMessage
	if err := json.Unmarshal(argsJSON, &raw); err != nil {
		return []byte("Ehost call arguments undecodable: " + err.Error())
	}
	args := make([]Value, len(raw))
	for i, enc := range raw {
		v, err := decodeEncoding(enc)
		if err != nil {
			return []byte("Ehost call argument undecodable: " + err.Error())
		}
		args[i] = v
	}
	ret, err := fn(args)
	if err != nil {
		return append([]byte{'E'}, err.Error()...)
	}
	return append([]byte{'R'}, ret.encode()...)
}

// newJS builds a fresh per-instance interpreter with a dispatching env.
func newJS(t *testing.T) (*JS, *testEnv) {
	t.Helper()
	env := &testEnv{funcs: map[string]Func{}}
	js, err := New(Options{Env: env})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = js.Close() })
	return js, env
}

// evalResult is the decoded js_eval envelope. Result carries the completion
// value's encoding; value(t) decodes it.
type evalResult struct {
	Ok     bool   `json:"ok"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

// value decodes the envelope's completion-value encoding.
func (r evalResult) value(t *testing.T) Value {
	t.Helper()
	v, err := decodeEncoding([]byte(r.Result))
	if err != nil {
		t.Fatalf("undecodable completion value %q: %v", r.Result, err)
	}
	return v
}

// display renders the completion value the way JS ToString would render the
// primitive — the convenient form for assertions.
func (r evalResult) display(t *testing.T) string {
	t.Helper()
	v := r.value(t)
	switch v.kind {
	case "undefined", "null":
		return v.kind
	case "bool":
		return strconv.FormatBool(v.Bool())
	case "number":
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case "string":
		return v.String()
	default:
		return "[" + v.kind + "]"
	}
}

// mustEval runs src on js and fails the test if the script threw or failed.
func mustEval(t *testing.T, js *JS, src string) evalResult {
	t.Helper()
	raw, err := js.Eval(src)
	if err != nil {
		t.Fatalf("Eval(%q) transport error: %v", src, err)
	}
	var r evalResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("Eval(%q) undecodable envelope %q: %v", src, raw, err)
	}
	if !r.Ok {
		t.Fatalf("Eval(%q) failed: %s", src, r.Error)
	}
	return r
}

// evalDisplay runs src and returns the completion value's display form.
func evalDisplay(t *testing.T, js *JS, src string) string {
	t.Helper()
	return mustEval(t, js, src).display(t)
}

// getObject is JS.Get with an is-an-object assertion; it returns the handle.
func getObject(t *testing.T, js *JS, parent uint64, name string) uint64 {
	t.Helper()
	raw, err := js.Get(parent, name)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	v, derr := decodeEncoding([]byte(raw))
	if derr != nil {
		t.Fatalf("Get(%q) undecodable %q: %v", name, raw, derr)
	}
	if !v.IsObject() || v.Handle() == 0 {
		t.Fatalf("Get(%q) = %+v, want an object handle", name, v)
	}
	return v.Handle()
}

// defineFunc registers fn under key and defines it on obj as `name`.
func defineFunc(t *testing.T, js *JS, env *testEnv, obj uint64, name, key string, nargs uint32, fn Func) {
	t.Helper()
	env.register(key, fn)
	if err := js.DefineFunction(obj, name, key, nargs); err != nil {
		t.Fatalf("DefineFunction(%q): %v", name, err)
	}
}
