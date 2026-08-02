package spidermonkey

import "encoding/json"

// JSError is a JavaScript-level failure: an uncaught throw, a compile error, a
// failed module import, or an interrupt. It is what Result.Error / ModuleResult
// .Error hold; it is distinct from a Go error returned by Eval/EvalModule, which
// signals a host/transport failure (a wasm trap, an encoding problem).
type JSError struct {
	// Message is the exception's stringification plus its stack, or a phrase
	// like "JS execution interrupted".
	Message string
}

func (e *JSError) Error() string { return e.Message }

// Result is the outcome of running a script with Eval. Error is nil on success;
// a non-nil Error (a *JSError) means the script threw, failed to compile, or was
// interrupted.
//
// There is no Stdout/Stderr: the engine is pure ECMA-262 and has no I/O of its
// own. Guest output is whatever the embedder wires up — define a `console` or
// `print` with Global().DefineFunc and route it to any io.Writer.
type Result struct {
	// Value is the script's completion value, valid only when Error is nil.
	// (Scripts have a completion value; modules do not — see ModuleResult.)
	// A primitive completion carries its data and type; an object or function
	// completion is an *Object with identity preserved — the same guest object,
	// navigable and callable.
	Value Value
	// Error is non-nil when the script threw, failed to compile, or was
	// interrupted.
	Error error
}

// ModuleResult is the outcome of running an ES module with EvalModule. A module
// has no completion value — its output is its side effects and its exports —
// so there is no Value field; the exports arrive as Namespace.
type ModuleResult struct {
	// Error is non-nil when the module threw, failed to compile, failed to
	// resolve an import, or was interrupted.
	Error error
	// Namespace is the module's namespace object — its exports, one property
	// per export name. Nil when the module failed, and (rarely) when the
	// engine could not produce a namespace for a module that did evaluate.
	// The caller owns it and should Free it when done.
	Namespace *Object
}

// envelope is the JSON shape the bridge returns for both scripts and modules.
type envelope struct {
	Ok     bool   `json:"ok"`
	Result string `json:"result"`
	Error  string `json:"error"`
	// Namespace is a module namespace object handle (module evaluation only).
	Namespace uint64 `json:"namespace"`
}

func parseEnvelope(raw string) (envelope, error) {
	var e envelope
	err := json.Unmarshal([]byte(raw), &e)
	return e, err
}

func jsErr(e envelope) error {
	if e.Ok {
		return nil
	}
	return &JSError{Message: e.Error}
}

func parseResult(js *JS, raw string) (Result, error) {
	e, err := parseEnvelope(raw)
	if err != nil {
		return Result{}, err
	}
	// Value defaults to undefined rather than a nil interface: a script that
	// threw has no completion value, and a caller that reaches for one anyway
	// must get JS's answer for "no value" instead of a Go nil-pointer panic.
	r := Result{Value: primitive{nil}, Error: jsErr(e)}
	if e.Ok {
		// The envelope's result field carries the completion value's encoding.
		v, verr := decodeValue(js, e.Result)
		if verr != nil {
			return r, verr
		}
		r.Value = v
	}
	return r, nil
}

func parseModuleResult(raw string) (ModuleResult, error) {
	e, err := parseEnvelope(raw)
	if err != nil {
		return ModuleResult{}, err
	}
	return ModuleResult{Error: jsErr(e)}, nil
}

// parseModuleResultFor is parseModuleResult with the interpreter the namespace
// handle belongs to, so the exports come back as a usable Object.
func parseModuleResultFor(js *JS, raw string) (ModuleResult, error) {
	e, err := parseEnvelope(raw)
	if err != nil {
		return ModuleResult{}, err
	}
	out := ModuleResult{Error: jsErr(e)}
	if out.Error == nil && e.Namespace != 0 {
		out.Namespace = &Object{js: js, handle: e.Namespace}
	}
	return out, nil
}
