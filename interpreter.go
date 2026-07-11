package spidermonkey

// Interpreter is a hand-written, multi-runtime API layered on top of the
// generated single-global binding in spidermonkey.go. Each Interpreter owns its
// own wasm module (independent linear memory + WASI host) and one SpiderMonkey
// runtime, so several interpreters run concurrently and in isolation. It also
// exposes the sandbox controls (memory caps, environment, stdio) and the
// interrupt primitive that lets a host watchdog stop a runaway script.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	wasm2go "github.com/goccy/spidermonkeywasm2go"
	"github.com/goccy/spidermonkeywasm2go/base"
)

// Method IDs of the spidermonkey bridge service (service 0), in the order the
// proto declares them: js_close, js_eval, js_interrupt_addr,
// js_interrupt_bits_addr, js_interrupt_bits_value, js_new.
const (
	midClose            = 0
	midEval             = 1
	midInterruptAddr    = 2
	midInterruptBitsPtr = 3
	midInterruptBitsVal = 4
	midNew              = 5
)

// Defaults for Config's zero value. MaxHeapBytes is deliberately a quarter of
// MaxMemoryBytes: see Config.MaxHeapBytes for why the ratio matters.
const (
	defaultMaxMemoryBytes = 256 << 20
	defaultMaxHeapBytes   = 64 << 20
	defaultStackQuota     = 512 << 10
)

// EvalResult is the decoded form of the JSON document js_eval returns.
type EvalResult struct {
	// Ok is false when the script threw, failed to compile, or was
	// interrupted; Error then describes it.
	Ok bool `json:"ok"`
	// Result is the stringification of the script's completion value, valid
	// only when Ok is true.
	Result string `json:"result"`
	// Stdout / Stderr capture what the script wrote via print() and
	// console.log() / console.error(). SpiderMonkey has no I/O of its own;
	// these functions are the only output surface a script is given.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Error is the exception's stringification plus its stack when Ok is
	// false, or "JS execution interrupted" after Interrupt.
	Error string `json:"error"`
}

// Config configures a new Interpreter's sandbox. The zero value is a usable,
// sandboxed interpreter: no host environment is leaked, no host stdio is
// attached, and the memory and recursion caps below take their defaults.
type Config struct {
	// MaxHeapBytes caps SpiderMonkey's GC heap (JSGC_MAX_BYTES). A script
	// that allocates past it gets a catchable JavaScript "out of memory"
	// error and the interpreter stays usable. Zero means defaultMaxHeapBytes.
	//
	// This — not MaxMemoryBytes — is the limit to tune. It must stay well
	// below MaxMemoryBytes, because the GC needs slack to fail gracefully:
	// it allocates in chunks, and when a chunk cannot be obtained from an
	// allocation path SpiderMonkey treats as infallible, it aborts the
	// process instead of throwing. Measured against this build, a heap cap up
	// to roughly a quarter of the wasm memory cap fails gracefully and
	// anything above it can trap. NewInterpreter rejects a ratio it cannot
	// vouch for rather than let it surface as a dead instance later.
	MaxHeapBytes int

	// MaxMemoryBytes caps this interpreter's wasm linear memory. A guest
	// allocation that would grow memory past this limit fails (memory.grow
	// returns -1) instead of growing the host process unbounded. Zero means
	// defaultMaxMemoryBytes. Rounded down to a multiple of the 64 KiB wasm
	// page size; values below the module's initial memory are ignored.
	//
	// This is a backstop that protects the HOST, not a limit the guest can
	// recover from: hitting it makes SpiderMonkey abort inside the guest,
	// which surfaces as a wasm trap and leaves this Interpreter unusable.
	// Bound the workload with MaxHeapBytes and Interrupt; size this so the
	// guest never reaches it.
	MaxMemoryBytes int

	// NativeStackQuotaBytes caps native recursion depth
	// (JS_SetNativeStackQuota), so runaway recursion raises a catchable
	// InternalError ("too much recursion") rather than overflowing the
	// guest's C stack — which would trap the whole instance. Zero means
	// defaultStackQuota. Keep it comfortably below the 8 MiB C stack the wasm
	// was linked with.
	NativeStackQuotaBytes int

	// MemoryReserveBytes, when > 0, is the initial linear-memory slice
	// capacity reserved for this interpreter. Reserving capacity up front
	// makes boot-time grows zero-copy reslices, dropping a freshly-booted
	// interpreter's resident memory. The reservation is virtual address space,
	// not resident memory. When 0, a default headroom is used.
	MemoryReserveBytes int

	// Env is the environment the guest sees. nil means an empty environment —
	// the host process os.Environ() is NOT leaked. A script cannot read it
	// (this bridge exposes no `process` or `env` binding); it reaches only
	// SpiderMonkey's own startup, which consults a handful of variables.
	Env []string

	// Stdin, when non-nil, backs the guest's fd 0. Defaults to an empty stream
	// (the host process stdin is NOT used).
	Stdin io.Reader
	// Stdout, when non-nil, receives the guest's fd 1 writes. Note: a script's
	// print() output is captured into EvalResult.Stdout, not written here;
	// this is the raw fd sink, which SpiderMonkey itself only touches on a
	// fatal error.
	Stdout io.Writer
	// Stderr, when non-nil, receives the guest's fd 2 writes.
	Stderr io.Writer

	// FSAccess is the guest filesystem access policy. It is consulted before
	// every file open with the guest path and whether the open is for writing;
	// returning false denies the operation (the guest sees EACCES).
	//
	// When nil — the default — ALL filesystem access is denied: the guest is
	// given no filesystem at all. This matters because the underlying WASI host
	// preopens the real host root ("/"), so without a policy a file open would
	// pass straight through to the host. SpiderMonkey installs no file API for a
	// script to reach that with, but denying at the WASI layer as well is the
	// belt-and-suspenders a sandbox wants. Supply a hook only to grant a script
	// specific, host-controlled access.
	FSAccess func(path string, write bool) bool
}

// Interpreter is one isolated SpiderMonkey runtime.
type Interpreter struct {
	m    *Module
	wasi *base.WasiStubs
	h    uint64
}

// NewInterpreter builds a fresh wasm instance, applies the sandbox config,
// initializes the SpiderMonkey runtime, and returns a ready interpreter.
func NewInterpreter(cfg Config) (inst *Interpreter, err error) {
	if cfg.MaxMemoryBytes == 0 {
		cfg.MaxMemoryBytes = defaultMaxMemoryBytes
	}
	if cfg.MaxHeapBytes == 0 {
		cfg.MaxHeapBytes = defaultMaxHeapBytes
	}
	if cfg.NativeStackQuotaBytes == 0 {
		cfg.NativeStackQuotaBytes = defaultStackQuota
	}
	// Refuse a heap cap the GC cannot fail gracefully under. Letting it
	// through would turn a script's out-of-memory — a recoverable, catchable
	// condition — into a wasm trap that kills the interpreter, and it would do
	// so only for the allocation shapes that happen to route through an
	// infallible path. Better to reject the configuration than to make the
	// failure mode depend on what the script allocates.
	if maxHeap := cfg.MaxMemoryBytes / 4; cfg.MaxHeapBytes > maxHeap {
		return nil, fmt.Errorf(
			"MaxHeapBytes (%d) exceeds a quarter of MaxMemoryBytes (%d): the GC needs that slack to raise "+
				"an out-of-memory error instead of aborting the instance; lower MaxHeapBytes to at most %d "+
				"or raise MaxMemoryBytes", cfg.MaxHeapBytes, cfg.MaxMemoryBytes, maxHeap)
	}

	m := &Module{}
	wasi := base.DefaultWASI()
	// Sandbox by default: do not leak the host environment.
	wasi.SetEnv(cfg.Env)
	// Sandbox stdio by default: an unset stream does NOT fall through to the
	// host process stdio. Stdin defaults to empty (immediate EOF), stdout and
	// stderr to discard. (A script's print() output is captured into
	// EvalResult by the bridge; these back the raw guest fds 0/1/2.)
	if cfg.Stdin != nil {
		wasi.SetStdin(cfg.Stdin)
	} else {
		wasi.SetStdin(bytes.NewReader(nil))
	}
	if cfg.Stdout != nil {
		wasi.SetStdout(cfg.Stdout)
	} else {
		wasi.SetStdout(io.Discard)
	}
	if cfg.Stderr != nil {
		wasi.SetStderr(cfg.Stderr)
	} else {
		wasi.SetStderr(io.Discard)
	}
	// Deny the guest all filesystem access by default. base.DefaultWASI preopens
	// the host "/", so without a policy a file open would pass straight through
	// to the host; SpiderMonkey installs no file API for a script to reach it,
	// but denying at the WASI layer too is the belt-and-suspenders the sandbox
	// wants. An embedder that needs to grant a script specific access supplies
	// Config.FSAccess. (Sockets and subprocesses are OFF at the module level —
	// the wasm is built with those wasmify capabilities disabled, so unlike the
	// filesystem they are not even imported; every import is wasi_snapshot_preview1.)
	if cfg.FSAccess != nil {
		wasi.SetFSAccessHook(cfg.FSAccess)
	} else {
		wasi.SetFSAccessHook(func(string, bool) bool { return false })
	}
	if cfg.MemoryReserveBytes > 0 {
		m.g = wasm2go.NewWithWASIReserve(wasi, cfg.MemoryReserveBytes)
	} else {
		m.g = wasm2go.NewWithWASI(wasi)
	}
	const wasmPage = 65536
	if max := uint64(cfg.MaxMemoryBytes) / wasmPage * wasmPage; max >= uint64(len(wasm2go.Memory(m.g))) {
		m.g.MaxMem = max
	}

	// Run the reactor _initialize + wasmify init under a recover so a C++
	// static-initializer trap surfaces as an error.
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("instance init panicked: %v", r)
			}
		}()
		wasm2go.Initialize(m.g)
		_ = wasm2go.WasmInit(m.g)
	}()
	if err != nil {
		return nil, err
	}

	inst = &Interpreter{m: m, wasi: wasi}
	h, err := inst.jsNew(uint32(cfg.MaxHeapBytes), uint32(cfg.NativeStackQuotaBytes))
	if err != nil {
		return nil, fmt.Errorf("js_new: %w", err)
	}
	if h == 0 {
		return nil, fmt.Errorf("js_new returned 0 (runtime init failed)")
	}
	inst.h = h
	return inst, nil
}

// Eval compiles and runs src as a classic script in this interpreter's
// persistent global, drains the microtask queue, and returns the structured
// result. A JavaScript-level throw is reported via EvalResult.Ok=false /
// .Error, not as a Go error; a Go error indicates a host/transport failure (a
// wasm trap, an encoding problem, ...).
//
// Global state persists across calls on the same interpreter, so Eval is
// REPL-like: `globalThis.x = 1` in one call is visible in the next.
func (i *Interpreter) Eval(src string) (EvalResult, error) {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	buf = pbAppendString(buf, 2, src)
	resp, err := i.m.invoke(0, midEval, buf, wasm2go.Inv_0_1)
	if err != nil {
		return EvalResult{}, err
	}
	if e := pbExtractError(resp); e != nil {
		return EvalResult{}, e
	}
	js := readScalarAtField(resp, 1, (*pbReader).readString)
	var r EvalResult
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		return EvalResult{}, fmt.Errorf("decode eval result %q: %w", js, err)
	}
	return r, nil
}

// Close finalizes the interpreter. The Interpreter must not be used afterward.
func (i *Interpreter) Close() error {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	resp, err := i.m.invoke(0, midClose, buf, wasm2go.Inv_0_0)
	if err != nil {
		return err
	}
	return pbExtractError(resp)
}

func (i *Interpreter) jsNew(maxHeapBytes, stackQuota uint32) (uint64, error) {
	// proto3 uint32 is varint-encoded, same as uint64 on the wire.
	var buf []byte
	buf = pbAppendUint64(buf, 1, uint64(maxHeapBytes))
	buf = pbAppendUint64(buf, 2, uint64(stackQuota))
	resp, err := i.m.invoke(0, midNew, buf, wasm2go.Inv_0_5)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// u32Call runs one of the interrupt-address accessors, all of which take the
// handle and return a single uint32.
func (i *Interpreter) u32Call(methodID int32, call func(*base.Module, int32, int32) (int64, error)) (uint32, error) {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	resp, err := i.m.invoke(0, methodID, buf, call)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint32), nil
}

// Interrupt stops a running Eval WITHOUT executing any wasm/C code on this
// instance: it writes directly into linear memory, and SpiderMonkey's
// interpreter notices at the next bytecode loop head. Eval then returns cleanly
// with Ok=false and Error="JS execution interrupted".
//
// The termination is an UNCATCHABLE exception: a script cannot swallow it with
// `try { while (true) {} } catch (e) {}`.
//
// Safe to call from another goroutine while Eval is running. Interrupt itself
// resolves the addresses first, which needs the instance lock that a running
// Eval holds — so to interrupt a loop that is ALREADY running, resolve them up
// front with PrepareInterrupt and call Fire.
func (i *Interpreter) Interrupt() error {
	ip, err := i.PrepareInterrupt()
	if err != nil {
		return err
	}
	ip.Fire()
	return nil
}

// Interrupter holds the pre-resolved interrupt addresses. Resolve it with
// PrepareInterrupt BEFORE starting the Eval you intend to interrupt (resolving
// needs the instance lock, which a running Eval holds), then call Fire from a
// watchdog goroutine.
type Interrupter struct {
	m *Module
	// flagAddr is the bridge's own "the host asked" word.
	flagAddr uint32
	// bitsAddr is &JSContext::interruptBits_, the word SpiderMonkey's
	// interpreter polls at every loop head, and bitsValue the bit to set in
	// it. bitsAddr is 0 when the guest could not locate the field; the guest
	// then keeps its interrupt permanently armed and the flag alone suffices.
	bitsAddr  uint32
	bitsValue uint32
}

// PrepareInterrupt resolves the interrupt addresses up front so Fire can run
// concurrently with a busy Eval (which holds the instance lock).
func (i *Interpreter) PrepareInterrupt() (*Interrupter, error) {
	flagAddr, err := i.u32Call(midInterruptAddr, wasm2go.Inv_0_2)
	if err != nil {
		return nil, err
	}
	bitsAddr, err := i.u32Call(midInterruptBitsPtr, wasm2go.Inv_0_3)
	if err != nil {
		return nil, err
	}
	bitsValue, err := i.u32Call(midInterruptBitsVal, wasm2go.Inv_0_4)
	if err != nil {
		return nil, err
	}
	var addrErr error
	base.AccessMemory(i.m.g, func(mem []byte) {
		// Both words live in the guest heap, far below the initial memory
		// size; validate anyway so a surprising address surfaces here rather
		// than as a wild write in Fire. bitsAddr == 0 is the documented
		// "not located" sentinel, not an address.
		if uint64(flagAddr)+4 > uint64(len(mem)) {
			addrErr = fmt.Errorf("interrupt flag address %#x out of range (memory is %d bytes)", flagAddr, len(mem))
			return
		}
		if bitsAddr != 0 && uint64(bitsAddr)+4 > uint64(len(mem)) {
			addrErr = fmt.Errorf("interruptBits_ address %#x out of range (memory is %d bytes)", bitsAddr, len(mem))
		}
	})
	if addrErr != nil {
		return nil, addrErr
	}
	return &Interrupter{m: i.m, flagAddr: flagAddr, bitsAddr: bitsAddr, bitsValue: bitsValue}, nil
}

// Fire trips the interrupt. It does not take the instance lock — that is the
// point: it runs concurrently with a busy Eval.
//
// The writes happen inside base.AccessMemory, which holds the same lock the
// runtime's memory.grow takes to mutate the memory slice header or relocate its
// backing array. That makes delivery deterministic: for the duration of the
// writes the memory can neither be resliced nor relocated, so they land in the
// array the guest observes.
//
// Order matters. The flag says "this interrupt is the host's"; the bit is what
// makes SpiderMonkey poll. Writing the flag first means the guest can never see
// the bit while the flag is still stale in a way that loses the interrupt: the
// guest's callback re-arms and resumes when the flag has not arrived yet, so a
// late flag costs a few bytecodes. The reverse order would let the callback
// resume with the bit already cleared, and Eval would spin forever while this
// goroutine believed it had cancelled the script.
func (ip *Interrupter) Fire() {
	base.AccessMemory(ip.m.g, func(mem []byte) {
		binary.LittleEndian.PutUint32(mem[ip.flagAddr:], 1)
		if ip.bitsAddr != 0 {
			bits := binary.LittleEndian.Uint32(mem[ip.bitsAddr:])
			binary.LittleEndian.PutUint32(mem[ip.bitsAddr:], bits|ip.bitsValue)
		}
	})
}
