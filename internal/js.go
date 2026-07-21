package internal

// Hand-written per-instance surface over the generated bridge.
//
// The generated functions in spidermonkey.go (JsEval, JsGlobal, ...) route
// through module()/globalModule — a single process-wide instance. go-spidermonkey
// instead runs many interpreters concurrently and in isolation, each on its own
// wasm module (its own linear memory, so the C++ "globals" g_cx/g_global live in
// per-module memory). JS below is that per-instance handle: every method drives
// THIS instance's module via js.m.invoke, so there is no global module.
//
// The method IDs are hand-maintained (see mid* below). They do NOT auto-follow a
// proto change the way the generated functions do — when the bridge is
// regenerated, re-check them against spidermonkey.go. They funnel through the one
// invokeMethod helper so callers (including the public API) only ever call
// methods; they never touch invoke or an ID.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"

	wasm2go "github.com/goccy/spidermonkeywasm2go"
	"github.com/goccy/spidermonkeywasm2go/base"
)

// ModuleLoaderKey is the reserved go_host_call key the C++ module-resolve hook
// uses to fetch a module's source on a registry miss. It is NUL-prefixed so it
// can never collide with a host function name. An env that recognises it is the
// ES module loader: args are the JSON array [specifier, referrer] and the reply
// is 'R' + the raw module source (or 'E' + an error message) — raw, not JSON,
// because module source is bytes, not a value.
const ModuleLoaderKey = "\x00module-load"

// Reserved go_host_call keys for the agent-side primitives (__agent__ in a
// spawned agent's global). They arrive on the AGENT's goroutine — concurrently
// with main-thread work — and the host owns all communication policy behind
// them. NUL-prefixed like the module loader's key.
const (
	// AgentReceiveKey: args [id]. The host may block this agent's goroutine
	// until it has a value; the reply is 'R' + a clone-handle decimal, or
	// 'E' + message (shutdown / refusal), which unwinds the agent script.
	// This is the BROADCAST channel (one latched value to every agent).
	AgentReceiveKey = "\x00agent-receive"
	// AgentInboxKey: args [id]. Like AgentReceiveKey but the POINT-TO-POINT
	// channel — blocks until this specific agent's inbox has a message
	// (Send delivers per-agent, FIFO). Reply is 'R' + a clone-handle decimal
	// or 'E' + message. This is what a Worker's onmessage is built on.
	AgentInboxKey = "\x00agent-inbox"
	// AgentTryInboxKey: args [id]. NON-blocking inbox poll — reply is 'R' + a
	// clone-handle decimal when a message is waiting, or 'R' with an empty
	// payload when the inbox is empty. An async Worker event loop polls this
	// (yielding to the job queue via a timed Atomics.waitAsync between polls),
	// so an async onmessage's promises still drain.
	AgentTryInboxKey = "\x00agent-try-inbox"
	// AgentPostKey: args [id, cloneHandle]. The host owns the handle from
	// here (JsCloneRead + JsCloneFree on its own thread). Reply 'R'.
	AgentPostKey = "\x00agent-post"
	// AgentSleepKey: args [id, ms]. The host blocks this agent's goroutine
	// for ms milliseconds. Reply 'R'.
	AgentSleepKey = "\x00agent-sleep"
	// AgentNowKey: args [id]. Reply 'R' + monotonic milliseconds (decimal).
	AgentNowKey = "\x00agent-now"
	// AgentExitKey: args [id]; the agent's thread is ending. Reply ignored.
	AgentExitKey = "\x00agent-exit"
)

// Method IDs of the bridge service (service 0). Verified against the generated
// spidermonkey.go; keep in sync on regeneration.
const (
	midCall              int32 = 4
	midClose             int32 = 8
	midDefineFunction    int32 = 11
	midDefineConstructor int32 = 10
	midEval              int32 = 13
	midEvalModule        int32 = 15
	midFreeObject        int32 = 16
	midGet               int32 = 18
	midGlobal            int32 = 19
	midInterruptAddr     int32 = 20
	midInterruptBitsAddr int32 = 21
	midInterruptBitsVal  int32 = 22
	midNew               int32 = 23
	midNewPlainObject    int32 = 26
	midRunJobs           int32 = 28
	midSet               int32 = 29
	midAgentSpawn        int32 = 0
	midAgentWake         int32 = 1
	midBytesNew          int32 = 2
	midBytesRead         int32 = 3
	midConstruct         int32 = 9
	midNewFunction       int32 = 24
	midCloneWrite        int32 = 7
	midCloneRead         int32 = 6
	midCloneFree         int32 = 5
	midGc                int32 = 17
	midDetachArrayBuffer int32 = 12
	midNewRealm          int32 = 27
	midEvalIn            int32 = 14
	midNewHTMLDDA        int32 = 25
)

// JS is one interpreter: its own wasm module and one SpiderMonkey runtime.
type JS struct {
	m    *Module
	h    uint64       // the runtime handle returned by js_new
	intr *interrupter // resolved once in New; fire() stops a running Eval
	// mmapped is the linear memory when it came from a shared copy-on-write
	// image; Go's GC does not manage it, so Close must unmap it explicitly.
	mmapped []byte
}

// interrupter holds the pre-resolved interrupt addresses so fire can run
// concurrently with a busy Eval (which holds the instance lock). The addresses
// are wasm offsets — stable across memory.grow — so a single resolution at New
// serves every Eval. It is unexported: nothing outside this package touches the
// interrupt directly; EvalContext drives it on the caller's behalf.
type interrupter struct {
	m         *Module
	flagAddr  uint32 // the bridge's own "the host asked" word
	bitsAddr  uint32 // &JSContext::interruptBits_, polled at every loop head (0 = not located)
	bitsValue uint32 // the bit to set in interruptBits_
}

// fire trips the interrupt WITHOUT taking the instance lock — that is the point:
// it runs concurrently with a busy Eval. SpiderMonkey notices at the next
// bytecode loop head and unwinds the script with an uncatchable "JS execution
// interrupted". The writes happen inside base.AccessMemory, which holds the same
// lock memory.grow takes, so for their duration the memory can neither be
// resliced nor relocated. Flag first, then bit: a late flag costs a few
// bytecodes, whereas the reverse order could clear the bit before the flag
// arrives and spin Eval forever.
func (ip *interrupter) fire() {
	base.AccessMemory(ip.m.g, func(mem []byte) {
		binary.LittleEndian.PutUint32(mem[ip.flagAddr:], 1)
		if ip.bitsAddr != 0 {
			bits := binary.LittleEndian.Uint32(mem[ip.bitsAddr:])
			binary.LittleEndian.PutUint32(mem[ip.bitsAddr:], bits|ip.bitsValue)
		}
	})
}

// withContext runs one bridge call, aborting it if ctx is cancelled or times
// out. The call runs in its own goroutine so this can return the moment ctx is
// done: on cancellation it fires the interrupt (which stops the script at the
// next bytecode loop head) and waits for the call to unwind, then returns
// ctx.Err(). A non-cancellable ctx (Background/TODO) has a nil Done channel, so
// that select arm never fires and this just waits for the call. ctx must be
// non-nil.
func (js *JS) withContext(ctx context.Context, run func() (string, error)) (string, error) {
	type out struct {
		raw string
		err error
	}
	ch := make(chan out, 1)
	go func() {
		raw, err := run()
		ch <- out{raw, err}
	}()
	select {
	case o := <-ch:
		return o.raw, o.err
	case <-ctx.Done():
		js.intr.fire()
		o := <-ch // interrupt stops the script; wait for the call to unwind
		return o.raw, ctx.Err()
	}
}

// EvalContext runs src under ctx (see withContext).
func (js *JS) EvalContext(ctx context.Context, src string) (string, error) {
	return js.withContext(ctx, func() (string, error) { return js.Eval(src) })
}

// EvalModule compiles src as an ES module registered under specifier, loads its
// dependency graph (each miss asks the env's module loader via ModuleLoaderKey),
// links, evaluates, and drains the job queue. Same {ok,result,error} envelope
// as Eval.
func (js *JS) EvalModule(specifier, src string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendString(buf, 2, specifier)
	buf = pbAppendUint64(buf, 3, uint64(len(specifier)))
	buf = pbAppendString(buf, 4, src)
	buf = pbAppendUint64(buf, 5, uint64(len(src)))
	resp, err := js.invokeMethod(midEvalModule, buf, wasm2go.Inv_0_15)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// EvalModuleContext runs EvalModule under ctx (see withContext).
func (js *JS) EvalModuleContext(ctx context.Context, specifier, src string) (string, error) {
	return js.withContext(ctx, func() (string, error) { return js.EvalModule(specifier, src) })
}

// RunJobs performs one step of the host event loop: due host timers, then the
// engine's own job-queue drain (js::RunJobs — microtasks plus cross-thread
// dispatchables). Returns the {ok,result,error} envelope; result is "1" if the
// step made progress, "2" if work is still pending (wait and call again), "0"
// if the queue is idle. The host loops on this to run an event loop.
func (js *JS) RunJobs() (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(midRunJobs, buf, wasm2go.Inv_0_28)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// RunJobsContext runs RunJobs under ctx, so a runaway job (a job that spins)
// is interrupted on cancellation just like a runaway Eval.
func (js *JS) RunJobsContext(ctx context.Context) (string, error) {
	return js.withContext(ctx, js.RunJobs)
}

// u32 runs one interrupt-address accessor (handle in, single uint32 out).
func (js *JS) u32(mid int32, call func(*base.Module, int32, int32) (int64, error)) (uint32, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(mid, buf, call)
	if err != nil {
		return 0, err
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint32), nil
}

func (js *JS) prepareInterrupter() (*interrupter, error) {
	flagAddr, err := js.u32(midInterruptAddr, wasm2go.Inv_0_20)
	if err != nil {
		return nil, err
	}
	bitsAddr, err := js.u32(midInterruptBitsAddr, wasm2go.Inv_0_21)
	if err != nil {
		return nil, err
	}
	bitsValue, err := js.u32(midInterruptBitsVal, wasm2go.Inv_0_22)
	if err != nil {
		return nil, err
	}
	return &interrupter{m: js.m, flagAddr: flagAddr, bitsAddr: bitsAddr, bitsValue: bitsValue}, nil
}

// AgentSpawn runs src on a NEW agent — its own thread (a goroutine under
// wasm2go), its own JSContext and global, sharing nothing with the main
// runtime but SharedArrayBuffer memory. The agent's __agent__ primitives call
// back through the reserved Agent*Key host-call keys, so the env owns all
// communication policy. Returns the agent's opaque non-zero id.
func (js *JS) AgentSpawn(glue, src string) (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendString(buf, 2, glue)
	buf = pbAppendUint64(buf, 3, uint64(len(glue)))
	buf = pbAppendString(buf, 4, src)
	buf = pbAppendUint64(buf, 5, uint64(len(src)))
	resp, err := js.invokeMethod(midAgentSpawn, buf, wasm2go.Inv_0_0)
	if err != nil {
		return 0, err
	}
	id := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if id == 0 {
		return 0, fmt.Errorf("agent spawn failed")
	}
	return id, nil
}

// AgentWake wakes every agent pump parked on the event futex, so an agent whose
// inbox was just filled (Send) delivers promptly.
func (js *JS) AgentWake() error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	_, err := js.invokeMethod(midAgentWake, buf, wasm2go.Inv_0_1)
	return err
}

// CloneWrite structured-clones the value the encoding describes (shared-memory
// objects allowed: a SAB is shared, everything else copied) into a clone
// handle the caller owns. Runs on the main runtime.
func (js *JS) CloneWrite(val string) (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendString(buf, 2, val)
	buf = pbAppendUint64(buf, 3, uint64(len(val)))
	resp, err := js.invokeMethod(midCloneWrite, buf, wasm2go.Inv_0_7)
	if err != nil {
		return 0, err
	}
	h := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if h == 0 {
		return 0, fmt.Errorf("value is not structured-clonable")
	}
	return h, nil
}

// CloneRead deserializes a clone handle into the main runtime and returns the
// value encoding. The handle stays valid until CloneFree.
func (js *JS) CloneRead(clone uint64) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, clone)
	resp, err := js.invokeMethod(midCloneRead, buf, wasm2go.Inv_0_6)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// CloneFree releases a clone handle.
func (js *JS) CloneFree(clone uint64) error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, clone)
	_, err := js.invokeMethod(midCloneFree, buf, wasm2go.Inv_0_5)
	return err
}

// Gc forces a full garbage collection.
func (js *JS) Gc() error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	_, err := js.invokeMethod(midGc, buf, wasm2go.Inv_0_17)
	return err
}

// DetachArrayBuffer detaches the ArrayBuffer behind obj. The reply is a value
// encoding: {"k":"undefined"} on success or an error encoding.
func (js *JS) DetachArrayBuffer(obj uint64) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	resp, err := js.invokeMethod(midDetachArrayBuffer, buf, wasm2go.Inv_0_12)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// NewFunction creates a fresh host-backed guest FUNCTION object (attached to
// nothing) and returns its handle. Guest calls dispatch to env under `key`;
// name is the function's name property, nargs its declared arity. The Go-side
// FuncOf primitive.
func (js *JS) NewFunction(name, key string, nargs uint32) (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendString(buf, 2, name)
	buf = pbAppendUint64(buf, 3, uint64(len(name)))
	buf = pbAppendString(buf, 4, key)
	buf = pbAppendUint64(buf, 5, uint64(len(key)))
	buf = pbAppendUint64(buf, 6, uint64(nargs))
	resp, err := js.invokeMethod(midNewFunction, buf, wasm2go.Inv_0_24)
	if err != nil {
		return 0, err
	}
	h := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if h == 0 {
		return 0, fmt.Errorf("function creation failed")
	}
	return h, nil
}

// Construct runs `new fn(...args)` — Call's [[Construct]] counterpart. fn must
// be a constructor (a class or function) handle; args is a JSON array of value
// encodings. The return is the new instance's value encoding.
func (js *JS) Construct(fn uint64, args string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, fn)
	buf = pbAppendString(buf, 3, args)
	buf = pbAppendUint64(buf, 4, uint64(len(args)))
	resp, err := js.invokeMethod(midConstruct, buf, wasm2go.Inv_0_9)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// BytesNew creates a fresh guest Uint8Array holding a copy of data and returns
// its object handle. The bytes cross the bridge RAW — the protobuf channel is
// length-delimited and 8-bit clean — with no base64/JSON encoding.
func (js *JS) BytesNew(data []byte) (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendBytes(buf, 2, data)
	buf = pbAppendUint64(buf, 3, uint64(len(data)))
	resp, err := js.invokeMethod(midBytesNew, buf, wasm2go.Inv_0_2)
	if err != nil {
		return 0, err
	}
	h := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if h == 0 {
		return 0, fmt.Errorf("byte array creation failed")
	}
	return h, nil
}

// BytesRead copies the binary contents of the object behind the handle
// (Uint8Array, any other ArrayBuffer view, ArrayBuffer, SharedArrayBuffer)
// out of the guest, raw. The reply is 'B' + bytes or 'E' + message.
func (js *JS) BytesRead(obj uint64) ([]byte, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	resp, err := js.invokeMethod(midBytesRead, buf, wasm2go.Inv_0_3)
	if err != nil {
		return nil, err
	}
	b := readScalarAtField(resp, 1, (*pbReader).readBytes)
	if len(b) == 0 {
		return nil, fmt.Errorf("malformed bytes reply: empty")
	}
	switch b[0] {
	case 'B':
		return b[1:], nil
	case 'E':
		return nil, fmt.Errorf("%s", b[1:])
	}
	return nil, fmt.Errorf("malformed bytes reply tag %q", b[0])
}

// NewRealm creates a fresh same-compartment realm (standard classes, empty
// host surface) and returns its global object's handle.
func (js *JS) NewRealm() (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(midNewRealm, buf, wasm2go.Inv_0_27)
	if err != nil {
		return 0, err
	}
	h := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if h == 0 {
		return 0, fmt.Errorf("realm creation failed")
	}
	return h, nil
}

// EvalIn evaluates src as a classic script in the realm of the given global
// handle; the reply is the completion value's encoding ({"k":"error",...} on
// throw). It does NOT drain the job queue.
func (js *JS) EvalIn(global uint64, src string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, global)
	buf = pbAppendString(buf, 3, src)
	buf = pbAppendUint64(buf, 4, uint64(len(src)))
	resp, err := js.invokeMethod(midEvalIn, buf, wasm2go.Inv_0_14)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// EvalInContext runs EvalIn under ctx (see withContext).
func (js *JS) EvalInContext(ctx context.Context, global uint64, src string) (string, error) {
	return js.withContext(ctx, func() (string, error) { return js.EvalIn(global, src) })
}

// NewHTMLDDA returns a fresh [[IsHTMLDDA]] object handle (emulates undefined;
// yields null when called — document.all semantics).
func (js *JS) NewHTMLDDA() (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(midNewHTMLDDA, buf, wasm2go.Inv_0_25)
	if err != nil {
		return 0, err
	}
	h := readScalarAtField(resp, 1, (*pbReader).readUint64)
	if h == 0 {
		return 0, fmt.Errorf("IsHTMLDDA creation failed")
	}
	return h, nil
}

// UnlockForHostCallback releases this instance's invoke lock for the duration
// of a guest→host callback, so the callback can re-enter the interpreter
// (Eval, Get, Set, Call, ...) without self-deadlocking on the non-reentrant
// invoke mutex; it returns the function that reacquires the lock. Call it ONLY
// from inside a host import (go_host_call), where the guest is paused waiting
// for the reply: re-entry then continues from the current wasm stack pointer,
// exactly like a native function calling back into the engine.
//
//	relock := js.UnlockForHostCallback()
//	ret, err := userCallback(args)
//	relock()
func (js *JS) UnlockForHostCallback() (relock func()) {
	js.m.mu.Unlock()
	return js.m.mu.Lock
}

// invokeMethod runs one RPC on THIS instance's module and folds in the standard
// bridge error check. The single seam every method funnels through.
func (js *JS) invokeMethod(mid int32, req []byte, call func(*base.Module, int32, int32) (int64, error)) ([]byte, error) {
	resp, err := js.m.invoke(0, mid, req, call)
	if err != nil {
		return nil, err
	}
	if e := pbExtractError(resp); e != nil {
		return nil, e
	}
	return resp, nil
}

func (js *JS) jsNew(maxHeapBytes, nativeStackQuotaBytes uint32) (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, uint64(maxHeapBytes))
	buf = pbAppendUint64(buf, 2, uint64(nativeStackQuotaBytes))
	resp, err := js.invokeMethod(midNew, buf, wasm2go.Inv_0_23)
	if err != nil {
		return 0, err
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// Close destroys the runtime (JS_DestroyContext) and unmaps the linear memory if
// it came from a shared copy-on-write image.
func (js *JS) Close() error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	_, err := js.invokeMethod(midClose, buf, wasm2go.Inv_0_8)
	if js.mmapped != nil {
		base.UnmapMemory(js.mmapped)
		js.mmapped = nil
	}
	return err
}

// Eval runs src as a classic script and returns the {ok,result,error} JSON
// envelope (see JsEval).
func (js *JS) Eval(src string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendString(buf, 2, src)
	buf = pbAppendUint64(buf, 3, uint64(len(src)))
	resp, err := js.invokeMethod(midEval, buf, wasm2go.Inv_0_13)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// Global returns a handle to this runtime's global object.
func (js *JS) Global() (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(midGlobal, buf, wasm2go.Inv_0_19)
	if err != nil {
		return 0, err
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// NewPlainObject returns a handle to a fresh plain object (JS_NewPlainObject).
func (js *JS) NewPlainObject() (uint64, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	resp, err := js.invokeMethod(midNewPlainObject, buf, wasm2go.Inv_0_26)
	if err != nil {
		return 0, err
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// Get returns obj[name] as a value ENCODING — the identity-preserving JSON tag
// the C++ codec emits: {"k":"bool|number|string|null|undefined","v":...} for a
// primitive, {"k":"object|function","h":<handle>} for an object (the handle is
// a fresh persistent root the caller must eventually FreeObject), and
// {"k":"error","v":<message>} when the property access threw.
func (js *JS) Get(obj uint64, name string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = pbAppendString(buf, 3, name)
	buf = pbAppendUint64(buf, 4, uint64(len(name)))
	resp, err := js.invokeMethod(midGet, buf, wasm2go.Inv_0_18)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// Set assigns obj[name] = the value the encoding `val` describes (same encoding
// language as Get). The reply is {"k":"undefined"} on success or an error
// encoding.
func (js *JS) Set(obj uint64, name, val string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = pbAppendString(buf, 3, name)
	buf = pbAppendUint64(buf, 4, uint64(len(name)))
	buf = pbAppendString(buf, 5, val)
	buf = pbAppendUint64(buf, 6, uint64(len(val)))
	resp, err := js.invokeMethod(midSet, buf, wasm2go.Inv_0_29)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// Call invokes the callable fn (an object handle) with `this` (an object handle,
// 0 = undefined) and args — a JSON array of value encodings. The return is one
// value encoding (possibly {"k":"error",...} when the call threw).
func (js *JS) Call(fn, this uint64, args string) (string, error) {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, fn)
	buf = pbAppendUint64(buf, 3, this)
	buf = pbAppendString(buf, 4, args)
	buf = pbAppendUint64(buf, 5, uint64(len(args)))
	resp, err := js.invokeMethod(midCall, buf, wasm2go.Inv_0_4)
	if err != nil {
		return "", err
	}
	return readScalarAtField(resp, 1, (*pbReader).readString), nil
}

// CallContext runs Call under ctx, so a callee that spins is interrupted on
// cancellation just like a runaway Eval.
func (js *JS) CallContext(ctx context.Context, fn, this uint64, args string) (string, error) {
	return js.withContext(ctx, func() (string, error) { return js.Call(fn, this, args) })
}

// DefineFunction defines a host-backed function `name` on obj. Guest calls
// dispatch to env under `key`; nargs sets the function's declared arity.
func (js *JS) DefineFunction(obj uint64, name, key string, nargs uint32) error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = pbAppendString(buf, 3, name)
	buf = pbAppendUint64(buf, 4, uint64(len(name)))
	buf = pbAppendString(buf, 5, key)
	buf = pbAppendUint64(buf, 6, uint64(len(key)))
	buf = pbAppendUint64(buf, 7, uint64(nargs))
	resp, err := js.invokeMethod(midDefineFunction, buf, wasm2go.Inv_0_11)
	if err != nil {
		return err
	}
	return envelopeError(resp)
}

// DefineConstructor defines a CONSTRUCTABLE host-backed function `name` on obj
// (new name(...) is allowed). Guest constructs dispatch to env under `key`.
func (js *JS) DefineConstructor(obj uint64, name, key string, nargs uint32) error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, js.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = pbAppendString(buf, 3, name)
	buf = pbAppendUint64(buf, 4, uint64(len(name)))
	buf = pbAppendString(buf, 5, key)
	buf = pbAppendUint64(buf, 6, uint64(len(key)))
	buf = pbAppendUint64(buf, 7, uint64(nargs))
	resp, err := js.invokeMethod(midDefineConstructor, buf, wasm2go.Inv_0_10)
	if err != nil {
		return err
	}
	return envelopeError(resp)
}

// FreeObject releases an object handle (deletes its persistent root).
func (js *JS) FreeObject(obj uint64) error {
	buf := pbNewBuf()
	buf = pbAppendUint64(buf, 1, obj)
	_, err := js.invokeMethod(midFreeObject, buf, wasm2go.Inv_0_16)
	return err
}

// envelopeError decodes the {ok,error} result envelope js_define_function /
// js_set_property_object return and surfaces a false `ok` as a Go error. These
// bridge calls report failure in the envelope, not the proto error field.
func envelopeError(resp []byte) error {
	s := readScalarAtField(resp, 1, (*pbReader).readString)
	if s == "" {
		return nil
	}
	var r struct {
		Ok    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil // not an envelope; nothing to report
	}
	if !r.Ok {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
