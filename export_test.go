package spidermonkey

// Test-only seams. This file is a _test file in the IMPLEMENTATION package, so
// the conformance suite (package spidermonkey_test) can reach the generic
// engine primitives without any of it leaking into the public API. Nothing
// $262-shaped exists below this layer: the suite composes the whole harness
// surface from these plus the public API.

import (
	"context"
	"encoding/json"
)

// GC forces a full garbage collection.
func GC(js *JS) error { return js.raw.Gc() }

// DetachArrayBuffer detaches the ArrayBuffer behind o.
func DetachArrayBuffer(js *JS, o *Object) error {
	reply, err := js.raw.DetachArrayBuffer(o.handle)
	if err != nil {
		return err
	}
	if _, err := decodeValue(js, reply); err != nil {
		return err
	}
	return nil
}

// NewRealm creates a fresh same-compartment realm (standard classes, EMPTY
// host surface) and returns its global object.
func NewRealm(js *JS) (*Object, error) {
	h, err := js.raw.NewRealm()
	if err != nil {
		return nil, err
	}
	return &Object{js: js, handle: h}, nil
}

// EvalIn evaluates src as a classic script in the realm of the given global
// (from NewRealm, or Global) and returns the completion value. A guest throw
// comes back as Throw(exception) — so an evalScript host func propagates the
// original exception with its type intact. It does NOT drain the job queue.
func EvalIn(js *JS, global *Object, src string) (Value, error) {
	encoded, err := js.raw.EvalIn(global.handle, src)
	if err != nil {
		return nil, err
	}
	if thrown(encoded) {
		v, derr := decodeValue(js, encoded)
		if derr != nil {
			return nil, derr
		}
		return nil, Throw(v)
	}
	return decodeValue(js, encoded)
}

// thrown reports whether an eval encoding is a thrown exception value.
func thrown(encoded string) bool {
	var e valueEncoding
	return json.Unmarshal([]byte(encoded), &e) == nil && e.Thrown
}

// NewHTMLDDA returns a fresh [[IsHTMLDDA]] object (emulates undefined; yields
// null when called — document.all semantics).
func NewHTMLDDA(js *JS) (*Object, error) {
	h, err := js.raw.NewHTMLDDA()
	if err != nil {
		return nil, err
	}
	return &Object{js: js, handle: h}, nil
}

// DefineFuncKeyed is DefineFunc with an explicit dispatch key, so the same
// property name can be defined on several objects (one per realm, say) with
// DIFFERENT Go callbacks behind it.
func DefineFuncKeyed(js *JS, o *Object, name, key string, fn Func) error {
	js.env.funcs[key] = fn
	return js.raw.DefineFunction(o.handle, name, key, 0)
}

// PumpResult is one step of the job queue for a harness that drives its own
// event loop (host timers interleaved with engine jobs). Code is "2" (engine
// work still pending) or "0" (engine idle). Err is a *JSError if a drained job
// threw. Guest output does not appear here — it flows through host funcs.
type PumpResult struct {
	Code string
	Err  error
}

// PumpJobs runs exactly one job-queue step under ctx — the single-step seam the
// public RunJobs iterator is built on, exposed for a harness that must fire its
// own host timers between steps.
func PumpJobs(ctx context.Context, js *JS) (PumpResult, error) {
	raw, err := js.raw.RunJobsContext(ctx)
	if err != nil {
		return PumpResult{}, err
	}
	e, perr := parseEnvelope(raw)
	if perr != nil {
		return PumpResult{}, perr
	}
	return PumpResult{Code: e.Result, Err: jsErr(e)}, nil
}
