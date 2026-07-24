// Package web installs the WinterTC (minimum common Web API) vocabulary on a
// go-spidermonkey interpreter: console, TextEncoder/TextDecoder, atob/btoa,
// URL/URLSearchParams, AbortController, queueMicrotask, structuredClone,
// performance.now, crypto.getRandomValues/randomUUID, ReadableStream, fetch,
// and setTimeout/setInterval.
//
// Registration is explicit:
//
//	js, _ := spidermonkey.New(cfg)
//	w, err := web.Install(js)
//	...
//	js.Eval(ctx, script)   // script may set timers, fetch, ...
//	w.Wait(ctx)            // run the event loop until all timers/ops settle
//
// Network access from fetch is gated by the interpreter's Config: Resolve is
// consulted per hostname lookup and Dial per outbound connection. Console
// output goes to Config.Stdout/Stderr.
package web

import (
	"context"
	"crypto/rand"
	_ "embed"
	"fmt"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
)

//go:embed js/builtins.js
var builtinsJS string

//go:embed js/subtle.js
var subtleJS string

//go:embed js/extended.js
var extendedJS string

// Web is one installation of the web vocabulary on one interpreter.
type Web struct {
	js     *spidermonkey.JS
	loop   *eventloop.Loop
	fetch  *fetchAPI
	subtle *subtleAPI
	start  time.Time
}

// Install defines the web globals on js and returns the handle that drives
// the event loop (Wait) and cleanup (Close). Install once per interpreter.
func Install(js *spidermonkey.JS) (*Web, error) {
	w := &Web{js: js, loop: eventloop.New(js), start: time.Now()}

	ops, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	defer ops.Free()
	opTable := map[string]spidermonkey.Func{
		"console_write": w.opConsoleWrite,
		"random_bytes":  w.opRandomBytes,
		"perf_now":      w.opPerfNow,
		"timer_set":     w.opTimerSet,
		"timer_clear":   w.opTimerClear,
		"timer_ref":     w.opTimerRef,
	}
	subtle := newSubtleAPI()
	w.subtle = subtle
	for name, fn := range subtle.ops() {
		opTable[name] = fn
	}
	for name, fn := range subtle.ops2() {
		opTable[name] = fn
	}
	for name, fn := range opTable {
		if err := ops.DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	if err := js.Global().Set("__web_ops", ops); err != nil {
		return nil, err
	}

	for _, src := range []string{builtinsJS, subtleJS, extendedJS, `delete globalThis.__web_ops;`} {
		r, err := js.Eval(context.Background(), src)
		if err != nil {
			return nil, fmt.Errorf("web: evaluating builtins: %w", err)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("web: builtins threw: %w", r.Error)
		}
	}

	w.fetch, err = installFetch(js)
	if err != nil {
		return nil, fmt.Errorf("web: installing fetch: %w", err)
	}
	return w, nil
}

// Wait runs the event loop until every timer has fired (or been cleared) and
// every in-flight op has completed, or ctx is done. A JS exception thrown by
// a timer callback stops the loop and is returned. Call it after evaluating
// code that schedules async work.
func (w *Web) Wait(ctx context.Context) error {
	return w.loop.Run(ctx)
}

// Close releases host resources held by the installation (open fetch bodies,
// cached engine handles). The interpreter itself stays usable.
func (w *Web) Close() error {
	w.fetch.closeAll()
	return nil
}

// Loop exposes the installation's event loop so sibling compat packages
// (compat/nodejs) can extend it — schedule immediates, replace the microtask
// drain. The type lives in an internal package; external callers use Wait.
func (w *Web) Loop() *eventloop.Loop {
	return w.loop
}

// ResetPerRequest drops per-request host state that must not leak across pooled
// instance reuse (cfworkers). Currently the SubtleCrypto key table: its handles
// are guest-visible and forgeable via globalThis.CryptoKey, so a later request
// must not be able to address an earlier request's key material. Call alongside
// Loop().Reset() between requests.
func (w *Web) ResetPerRequest() {
	if w.subtle != nil {
		w.subtle.reset()
	}
}

func (w *Web) opConsoleWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.Undefined(), nil
	}
	out := cfg.Stdout
	if args[0].Int() != 0 {
		out = cfg.Stderr
	}
	if out != nil {
		fmt.Fprintln(out, args[1].String())
	}
	return spidermonkey.Undefined(), nil
}

// opRandomBytes returns n cryptographically random bytes as a plain array —
// data, not a handle — so the guest copy leaves nothing pinned host-side.
func (w *Web) opRandomBytes(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("random_bytes: length required")
	}
	n := args[0].Int()
	if n < 0 || n > 65536 {
		return nil, fmt.Errorf("random_bytes: invalid length %d", n)
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	ints := make([]int, n)
	for i, b := range buf {
		ints[i] = int(b)
	}
	return spidermonkey.ValueOf(ints), nil
}

func (w *Web) opPerfNow(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	return spidermonkey.ValueOf(float64(time.Since(w.start)) / float64(time.Millisecond)), nil
}

func (w *Web) opTimerSet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("timer_set: (callback, delayMs, repeat) required")
	}
	fn := args[0].Object()
	if fn == nil || !fn.IsFunction() {
		return nil, fmt.Errorf("timer_set: callback is not a function")
	}
	delay := time.Duration(args[1].Float() * float64(time.Millisecond))
	// The loop takes ownership of fn's handle (freed on fire or clear).
	id := w.loop.SetTimer(fn, delay, args[2].Bool())
	return spidermonkey.ValueOf(id), nil
}

func (w *Web) opTimerClear(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) >= 1 {
		w.loop.ClearTimer(int64(args[0].Float()))
	}
	return spidermonkey.Undefined(), nil
}

// opTimerRef(id, ref) sets whether a timer keeps the loop alive — the Go half of
// Timeout.ref()/unref().
func (w *Web) opTimerRef(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) >= 2 {
		w.loop.SetTimerRef(int64(args[0].Float()), args[1].Bool())
	}
	return spidermonkey.Undefined(), nil
}
