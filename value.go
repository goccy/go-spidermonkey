package spidermonkey

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Value is any JavaScript value — a primitive (number, string, boolean, null,
// undefined) or an Object. It is an interface precisely because a JS value is a
// single family spanning all of these: the concrete kinds (primitives and
// *Object) implement it, and code that holds a Value need not care which it is.
//
// Primitives cross the guest/host boundary as data; objects and functions cross
// as a handle (an *Object) so they keep their identity and can still be
// navigated and called. String coerces like JS ToString for primitives; Float,
// Int and Bool coerce to the Go type (returning the zero value when the Value
// is not that kind); Export returns the default Go representation.
type Value interface {
	String() string
	Float() float64
	Int() int
	Bool() bool
	IsUndefined() bool
	IsObject() bool
	// Object returns the value as an *Object when IsObject, else nil.
	Object() *Object
	// Export returns the default Go representation of the value (JS → Go).
	Export() any

	// isValue seals the interface: only this package's types are Values.
	isValue()
}

// ValueOf wraps a Go value as a host Value. An existing Value passes through;
// bool, string and the numeric types become the matching JS primitive; nil
// becomes undefined. A composite Go value (slice, array, map, struct — anything
// encoding/json can marshal) materializes guest-side as a FRESH JS Array/Object
// each time it crosses: it carries data, not identity. When the guest must see
// the same object across calls, build it once with NewObject/Set instead.
func ValueOf(x any) Value {
	switch v := x.(type) {
	case Value:
		return v
	case nil:
		return primitive{nil}
	case bool, string, float64:
		return primitive{v}
	case float32:
		return primitive{float64(v)}
	case int:
		return primitive{float64(v)}
	case int8:
		return primitive{float64(v)}
	case int16:
		return primitive{float64(v)}
	case int32:
		return primitive{float64(v)}
	case int64:
		return primitive{float64(v)}
	case uint:
		return primitive{float64(v)}
	case uint8:
		return primitive{float64(v)}
	case uint16:
		return primitive{float64(v)}
	case uint32:
		return primitive{float64(v)}
	case uint64:
		return primitive{float64(v)}
	default:
		return primitive{x} // composite: encodeValue emits it as the json kind
	}
}

// Undefined is the Value meaning "no value".
func Undefined() Value { return primitive{nil} }

// Null is the JS null value (distinct from Undefined).
func Null() Value { return primitive{nullMarker{}} }

// nullMarker distinguishes JS null from undefined inside a primitive; both
// Export to nil.
type nullMarker struct{}

// primitive implements Value for the data kinds — everything that crosses the
// bridge as data rather than as an object handle. v is nil (undefined),
// nullMarker (null), bool, float64 or string.
type primitive struct{ v any }

func (primitive) isValue()            {}
func (p primitive) IsUndefined() bool { return p.v == nil }
func (p primitive) String() string {
	switch v := p.v.(type) {
	case nil:
		return "undefined"
	case nullMarker:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}
func (p primitive) Float() float64  { f, _ := p.v.(float64); return f }
func (p primitive) Int() int        { return int(p.Float()) }
func (p primitive) Bool() bool      { b, _ := p.v.(bool); return b }
func (p primitive) IsObject() bool  { return false }
func (p primitive) Object() *Object { return nil }
func (p primitive) Export() any {
	if _, isNull := p.v.(nullMarker); isNull {
		return nil
	}
	return p.v
}

// opaqueValue is a Value with no host data representation — a symbol or a
// bigint. It crosses the bridge by handle (staying === itself in the guest),
// but Go cannot read its contents. Free releases the handle.
type opaqueValue struct {
	js     *JS
	handle uint64
	kind   string // "symbol" | "bigint"
}

func (opaqueValue) isValue()          {}
func (opaqueValue) IsUndefined() bool { return false }
func (o opaqueValue) String() string  { return o.kind }
func (opaqueValue) Float() float64    { return 0 }
func (opaqueValue) Int() int          { return 0 }
func (opaqueValue) Bool() bool        { return true }
func (opaqueValue) IsObject() bool    { return false }
func (opaqueValue) Object() *Object   { return nil }
func (opaqueValue) Export() any       { return nil }

// thrownValue is the error a host Func returns (via Throw) to throw a Value
// as-is — preserving its JS type — instead of a generic Error.
type thrownValue struct{ v Value }

func (t *thrownValue) Error() string { return "throw: " + t.v.String() }

// Throw wraps v so a host Func can throw it verbatim: `return nil, Throw(v)`
// makes the guest see v thrown (a SyntaxError instance stays a SyntaxError),
// whereas returning an ordinary error surfaces as a generic Error with that
// message.
func Throw(v Value) error { return &thrownValue{v} }

// valueEncoding is the identity-preserving JSON tag the C++ codec speaks:
// {"k":"bool|number|string","v":<data>}, {"k":"null"}, {"k":"undefined"},
// {"k":"object|function","h":<persistent-root handle>}, or
// {"k":"error","v":<message>} when the guest operation threw. Host→guest only,
// {"k":"json","v":<data>} carries composite Go data that materializes as a
// fresh guest Array/Object.
type valueEncoding struct {
	K string `json:"k"`
	V any    `json:"v,omitempty"`
	H uint64 `json:"h,omitempty"`
	// Thrown marks an eval result that is a thrown exception value (js_eval_in)
	// rather than a completion value — the caller re-throws it.
	Thrown bool `json:"thrown,omitempty"`
}

// decodeValue turns one value encoding into a Value. An object/function handle
// becomes an *Object owned by js (the caller owns its persistent root — Free
// releases it). A {"k":"error"} encoding is returned as a *JSError.
func decodeValue(js *JS, encoded string) (Value, error) {
	var e valueEncoding
	if err := json.Unmarshal([]byte(encoded), &e); err != nil {
		return nil, fmt.Errorf("undecodable value encoding %q: %w", encoded, err)
	}
	switch e.K {
	case "undefined":
		return Undefined(), nil
	case "null":
		return Null(), nil
	case "bool":
		b, _ := e.V.(bool)
		return primitive{b}, nil
	case "number":
		// JSON has no NaN/Infinity literal; the codec tags the non-finite
		// specials as strings.
		if s, isStr := e.V.(string); isStr {
			switch s {
			case "NaN":
				return primitive{math.NaN()}, nil
			case "Infinity":
				return primitive{math.Inf(1)}, nil
			case "-Infinity":
				return primitive{math.Inf(-1)}, nil
			}
			return nil, fmt.Errorf("unknown number encoding %q", s)
		}
		f, _ := e.V.(float64)
		return primitive{f}, nil
	case "string":
		s, _ := e.V.(string)
		return primitive{s}, nil
	case "object", "function":
		return &Object{js: js, handle: e.H, callable: e.K == "function"}, nil
	case "symbol", "bigint":
		// No host representation: crosses by handle, staying identical to
		// itself, but Go cannot inspect it.
		return opaqueValue{js: js, handle: e.H, kind: e.K}, nil
	case "error":
		msg, _ := e.V.(string)
		return nil, &JSError{Message: msg}
	default:
		return nil, fmt.Errorf("unknown value encoding kind %q", e.K)
	}
}

// encodeValue turns a Value into the encoding the guest decodes. It is the
// exact inverse of decodeValue for every Value this package produces.
func encodeValue(v Value) (string, error) {
	if v == nil {
		return `{"k":"undefined"}`, nil
	}
	if o := v.Object(); o != nil {
		return fmt.Sprintf(`{"k":"object","h":%d}`, o.handle), nil
	}
	if ov, ok := v.(opaqueValue); ok {
		return fmt.Sprintf(`{"k":%q,"h":%d}`, ov.kind, ov.handle), nil
	}
	p, ok := v.(primitive)
	if !ok {
		return "", fmt.Errorf("unencodable value type %T", v)
	}
	switch pv := p.v.(type) {
	case nil:
		return `{"k":"undefined"}`, nil
	case nullMarker:
		return `{"k":"null"}`, nil
	case bool:
		return fmt.Sprintf(`{"k":"bool","v":%t}`, pv), nil
	case float64:
		// The non-finite specials cross as tagged strings (JSON has no
		// NaN/Infinity literal); see decodeValue.
		switch {
		case math.IsNaN(pv):
			return `{"k":"number","v":"NaN"}`, nil
		case math.IsInf(pv, 1):
			return `{"k":"number","v":"Infinity"}`, nil
		case math.IsInf(pv, -1):
			return `{"k":"number","v":"-Infinity"}`, nil
		}
		return fmt.Sprintf(`{"k":"number","v":%s}`, strconv.FormatFloat(pv, 'g', -1, 64)), nil
	case string:
		b, err := json.Marshal(pv)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"k":"string","v":%s}`, b), nil
	default:
		// Composite host data (slice, map, struct): crosses as its JSON and
		// materializes guest-side as a fresh Array/Object — data, not identity.
		b, err := json.Marshal(pv)
		if err != nil {
			return "", fmt.Errorf("unencodable Go value %T: %w", pv, err)
		}
		return fmt.Sprintf(`{"k":"json","v":%s}`, b), nil
	}
}

// encodeArgs builds the JSON array of value encodings a guest call expects.
func encodeArgs(args []Value) (string, error) {
	out := "["
	for i, a := range args {
		if i > 0 {
			out += ","
		}
		e, err := encodeValue(a)
		if err != nil {
			return "", fmt.Errorf("argument %d: %w", i, err)
		}
		out += e
	}
	return out + "]", nil
}
