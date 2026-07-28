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
	// What existed before this installation, so the globals it adds can be given
	// the property attributes a real runtime gives them (see HideNewGlobals).
	preexisting, err := SnapshotGlobals(js)
	if err != nil {
		return nil, fmt.Errorf("web: reading globals: %w", err)
	}

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

	w.fetch, err = installFetch(js, w.loop)
	if err != nil {
		return nil, fmt.Errorf("web: installing fetch: %w", err)
	}
	if err := HideNewGlobals(js, preexisting); err != nil {
		return nil, err
	}
	w.loop.SetRejectionReporter(w.reportRejections)
	return w, nil
}

// reportRejections hands the guest every promise rejection that reached a
// microtask checkpoint with nothing to handle it, and reports whether there
// were any.
//
// The engine is the only place these are visible — an async function's promise
// is created by the engine, so no host-side Promise wrapper sees it — which is
// why this is a host loop concern rather than something builtins.js could do
// for itself. The guest hook decides POLICY (dispatch an `unhandledrejection`
// event here; compat/nodejs replaces it with process.emit); this only carries
// them across.
func (w *Web) reportRejections(ctx context.Context) (bool, error) {
	rejections, err := w.js.TakeUnhandledRejections()
	if err != nil || len(rejections) == 0 {
		return false, err
	}
	hookVal, err := w.js.Global().Get(rejectionHookName)
	if err != nil {
		freeRejections(rejections)
		return false, err
	}
	hook := hookVal.Object()
	if hook == nil || !hook.IsFunction() {
		// No guest hook: still free the handles, and report nothing rather
		// than failing a loop turn over a missing diagnostic channel.
		freeRejections(rejections)
		return false, nil
	}
	defer hook.Free()
	for _, r := range rejections {
		_, cerr := hook.Call(r.Reason, r.Promise)
		freeValue(r.Reason)
		freeValue(r.Promise)
		if cerr != nil && err == nil {
			err = cerr
		}
	}
	return true, err
}

// rejectionHookName is the guest function reportRejections calls, one call per
// rejection, with (reason, promise). compat/nodejs redefines it to Node's
// process.emit('unhandledRejection', ...) semantics.
const rejectionHookName = "__unhandled_rejection"

func freeRejections(rejections []spidermonkey.Rejection) {
	for _, r := range rejections {
		freeValue(r.Reason)
		freeValue(r.Promise)
	}
}

func freeValue(v spidermonkey.Value) {
	if o := v.Object(); o != nil {
		o.Free()
	}
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

// ResetKeys drops the whole SubtleCrypto key table. A host that wants strict
// per-request key isolation on a pooled instance can call this between requests
// so one request's key material can't be addressed — via a forged
// globalThis.CryptoKey carrying its handle — by a later one. The cfworkers pool
// deliberately does NOT call this: real Cloudflare Workers preserve module
// globals (including imported CryptoKeys) across requests on a warm isolate, so
// the "import once, cache the key" pattern must keep working there; the random
// unguessable handle is the anti-forgery boundary within that single trust
// domain. AES/ECDH/HKDF/PBKDF2 material already survives (guest-side WeakMaps),
// so persisting the host table too just makes all algorithms consistent.
func (w *Web) ResetKeys() {
	if w.subtle != nil {
		w.subtle.reset()
	}
}

// ResetPerRequest drops per-request host state that must not leak across pooled
// instance reuse (cfworkers): leftover timers/immediates and in-flight async
// fetches. It deliberately does NOT clear the SubtleCrypto key table — see
// ResetKeys for why keys persist across requests. Call alongside Loop().Reset()
// between requests.
func (w *Web) ResetPerRequest() {
	// Quiesce any leftover timers/immediates BEFORE the fetch drain below. A
	// handler that left a setInterval armed would otherwise fire during the drain
	// RunUntil, spawn a fresh (un-cancelled) fetch goroutine, and have that Post
	// land on the NEXT request's loop — driving its pending accounting negative
	// (premature idle -> a spurious 500). Loop().Reset() (called by the caller
	// after us) also clears timers, but only after the drain has already run.
	w.loop.ClearTimers()
	if w.fetch != nil {
		// Abort any in-flight async fetch so no goroutine's late loop.Post survives
		// into the next request (corrupting its pending accounting). cancelInflight
		// cancels the round-trip ctx (unblocks client.Do); closeOpenStreams closes
		// any response body a buffered read is blocked on (readAll reads st.body,
		// NOT the consumer ctx, so it must be closed to unblock). Do BOTH before
		// draining, or an in-flight body read would pin the instance for the whole
		// 5s timeout. Then drain the (now promptly-returning) cancelled Posts while
		// the loop is still usable, before Loop().Reset() drops queued posts.
		w.fetch.cancelInflight()
		w.fetch.closeOpenStreams()
		if w.fetch.inflightCount() > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			w.loop.RunUntil(ctx, func() bool { return w.fetch.inflightCount() == 0 })
			cancel()
		}
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
	return bytesValue(buf), nil
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
		// A non-callable object was still taken here, so it is ours to release:
		// setTimeout({}) in a loop must not accumulate roots.
		fn.Free()
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
