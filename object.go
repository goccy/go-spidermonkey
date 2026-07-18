package spidermonkey

// Object is a handle to a JavaScript object (or function) owned by a JS
// interpreter. An Object IS a Value — it implements the Value interface — so it
// can be passed anywhere a Value is expected, and property navigation (Get) can
// return either a primitive Value or another Object.
//
// The handle preserves identity: an Object that crosses the bridge in either
// direction refers to the SAME guest object, so mutations are visible on both
// sides. The handle pins the object against garbage collection until Free (or
// the interpreter's Close).
type Object struct {
	js       *JS
	handle   uint64
	callable bool // true when the value decoded as a function
}

// Object implements Value.
func (*Object) isValue()            {}
func (o *Object) IsUndefined() bool { return false }
func (o *Object) Float() float64    { return 0 }
func (o *Object) Int() int          { return 0 }
func (o *Object) Bool() bool        { return true } // objects are truthy
func (o *Object) IsObject() bool    { return true }
func (o *Object) Object() *Object   { return o }
func (o *Object) Export() any       { return o }

// IsFunction reports whether the object is callable (Call will work).
func (o *Object) IsFunction() bool { return o.callable }

// String returns the object's JS ToString — o.toString() run guest-side — or
// "[object Object]" when that is not reachable (a Value cannot surface an
// error).
func (o *Object) String() string {
	fn, err := o.Get("toString")
	if err != nil {
		return "[object Object]"
	}
	fnObj := fn.Object()
	if fnObj == nil {
		return "[object Object]"
	}
	defer fnObj.Free()
	s, err := fnObj.call(o, nil)
	if err != nil || s.IsObject() {
		return "[object Object]"
	}
	return s.String()
}

// Global returns the interpreter's global object. Defining a function on it is
// the host-surface opt-in: a fresh interpreter is pure ECMA-262 and exposes
// nothing until the embedder adds something.
//
//	js.Global().DefineFunc("print", func(cfg Config, args []Value) (Value, error) {
//		...
//	})
func (js *JS) Global() *Object {
	if js.global == nil {
		h, err := js.raw.Global()
		if err != nil {
			return &Object{js: js} // handle 0; operations will fail cleanly
		}
		js.global = &Object{js: js, handle: h}
	}
	return js.global
}

// DefineFunc defines a host-backed function name on the object; calling it from
// JS runs fn with the interpreter's Config and the call arguments.
func (o *Object) DefineFunc(name string, fn Func) error {
	o.js.env.funcs[name] = fn
	return o.js.raw.DefineFunction(o.handle, name, name, 0)
}

// DefineConstructor defines a host-backed CONSTRUCTOR name on the object:
// `new name(...)` from JS runs fn, and the Value fn returns (typically an
// *Object built with NewObject) becomes the instance. This is how a real host
// class — `new Worker(src)` and the like — is defined from Go.
//
// key lets several constructors share a name across objects with distinct Go
// callbacks; pass name itself for the common case.
func (o *Object) DefineConstructor(name, key string, fn Func) error {
	o.js.env.funcs[key] = fn
	return o.js.raw.DefineConstructor(o.handle, name, key, 0)
}

// NewObject creates a fresh, empty JavaScript object owned by this interpreter.
func (js *JS) NewObject() (*Object, error) {
	h, err := js.raw.NewPlainObject()
	if err != nil {
		return nil, err
	}
	return &Object{js: js, handle: h}, nil
}

// Get returns the property name of the object as a Value — a primitive carries
// its data, an object or function comes back as an *Object with identity
// preserved. A missing property is Undefined; a property whose access throws
// (a getter) is returned as a *JSError.
func (o *Object) Get(name string) (Value, error) {
	encoded, err := o.js.raw.Get(o.handle, name)
	if err != nil {
		return nil, err
	}
	return decodeValue(o.js, encoded)
}

// Set sets the property name of the object to v — a primitive Value or an
// *Object (which keeps its identity: the guest sees the same object).
func (o *Object) Set(name string, v Value) error {
	encoded, err := encodeValue(v)
	if err != nil {
		return err
	}
	reply, err := o.js.raw.Set(o.handle, name, encoded)
	if err != nil {
		return err
	}
	if _, err := decodeValue(o.js, reply); err != nil {
		return err
	}
	return nil
}

// Call invokes the object as a function with `this` undefined. A guest throw
// comes back as a *JSError.
func (o *Object) Call(args ...Value) (Value, error) {
	return o.call(nil, args)
}

// CallMethod invokes o[name](args...) with `this` bound to o.
func (o *Object) CallMethod(name string, args ...Value) (Value, error) {
	fn, err := o.Get(name)
	if err != nil {
		return nil, err
	}
	fnObj := fn.Object()
	if fnObj == nil {
		return nil, &JSError{Message: name + " is not a function"}
	}
	defer fnObj.Free()
	return fnObj.call(o, args)
}

// call is the one seam over the raw bridge call: fn = o, this = self (nil for
// undefined).
func (o *Object) call(self *Object, args []Value) (Value, error) {
	encodedArgs, err := encodeArgs(args)
	if err != nil {
		return nil, err
	}
	var thisHandle uint64
	if self != nil {
		thisHandle = self.handle
	}
	encoded, err := o.js.raw.Call(o.handle, thisHandle, encodedArgs)
	if err != nil {
		return nil, err
	}
	return decodeValue(o.js, encoded)
}

// Free releases the object handle (its GC pin). The object itself lives on in
// the guest; only the host's reference is dropped. The global object (from
// Global) is freed by Close and must not be freed here.
func (o *Object) Free() error {
	return o.js.raw.FreeObject(o.handle)
}
