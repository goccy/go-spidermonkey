package internal

// Instance construction and the process-wide copy-on-write image machinery.
//
// An interpreter's linear memory is ~33 MB by the time it can run a line of
// JavaScript, and almost none of it is about THIS interpreter: 8 MB of untouched
// C stack, 18 MB of SpiderMonkey's static tables + ICU data, and ~7 MB the JS
// runtime leaves on the C++ heap. Every byte is identical in every interpreter,
// so build it ONCE per process and map it copy-on-write into each instance —
// only the pages a guest writes become private. wasm2go owns the machinery
// (base.NewSharedSnapshot / base.NewSharedImage); this file says what to image.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"

	wasm2go "github.com/goccy/spidermonkeywasm2go"
	"github.com/goccy/spidermonkeywasm2go/base"
)

const (
	wasmPageSize = 65536
	// defaultMaxMemoryBytes is the linear-memory ceiling when Options.MaxMemory
	// is 0. It sizes the allocation the guest grows into; untouched pages never
	// page in, so headroom is nearly free.
	defaultMaxMemoryBytes = 256 << 20
	// defaultNativeStackQuota caps native recursion depth (JS_SetNativeStackQuota)
	// so runaway recursion raises a catchable "too much recursion" InternalError
	// instead of overflowing the 8 MB linked C stack (a fatal trap). Fixed: the
	// value is a safe margin below the linked stack, sized so that legitimate
	// deep nesting passes — ECMA-262 conformance (e.g. S13.2.1_A1_T1's nested
	// function bodies) needs well beyond 512 KiB, and 4 MiB still leaves the
	// linked stack half free.
	defaultNativeStackQuota = 4 << 20
)

// Options configures a new interpreter's wasm instance. All fields are the
// sandbox's business; the public API maps its Config onto them.
type Options struct {
	// Env receives guest->host calls (the callbacks DefineFunction wires up).
	// nil means the guest can register nothing.
	Env base.EnvImports
	// Environ is the environment the guest sees. It is baked into the shared
	// snapshot (the guest's libc caches it during startup), so it keys the
	// snapshot cache — instances with different Environ get different snapshots.
	Environ []string
	// Stdin/Stdout/Stderr back the guest's raw fds 0/1/2. Unset streams are
	// sandboxed (empty stdin, discarded stdout/stderr) — never the host's.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// MaxMemory is the linear-memory ceiling in bytes; 0 means default.
	MaxMemory int
}

// New brings up an interpreter: its own wasm module (its own linear memory) and
// one SpiderMonkey runtime, with the interrupt addresses resolved and cached.
func New(opts Options) (*JS, error) {
	maxMem := opts.MaxMemory
	if maxMem == 0 {
		maxMem = defaultMaxMemoryBytes
	}
	maxMemBytes := uint64(maxMem) / wasmPageSize * wasmPageSize

	wasi := buildWASI(opts)
	env := opts.Env
	if env == nil {
		env = envStubs{}
	}
	m := &Module{}

	// Fast path: a copy-on-write map of a fully started interpreter. It re-runs
	// nothing (no start section, no _initialize, no js_new) and inherits the
	// snapshot's runtime handle.
	snap := sharedSnapshot(opts.Environ)
	if mem, err := snap.memory(int(maxMemBytes)); err == nil {
		m.g = wasm2go.NewFromSnapshot(wasi, env, mem, snap.img.Size(), snap.img.Globals())
		m.g.MaxMem = maxMemBytes
		return finishJS(&JS{m: m, h: snap.handle, mmapped: mem})
	}

	// No snapshot: start this runtime ourselves — over a shared data-segment
	// image if one is available, else a private allocation.
	var mmapped []byte
	if mem, err := sharedImage().Memory(int(maxMemBytes)); err == nil {
		m.g = wasm2go.NewWithMemory(wasi, env, mem, sharedImage().Size())
		mmapped = mem
	} else {
		m.g = wasm2go.NewWithWASIReserve(wasi, env, int(maxMemBytes))
	}
	m.g.MaxMem = maxMemBytes

	if err := initInstance(m.g); err != nil {
		unmap(mmapped)
		return nil, err
	}
	js := &JS{m: m, mmapped: mmapped}
	h, err := js.jsNew(0, defaultNativeStackQuota)
	if err != nil {
		unmap(mmapped)
		return nil, fmt.Errorf("internal: js_new: %w", err)
	}
	if h == 0 {
		unmap(mmapped)
		return nil, fmt.Errorf("internal: js_new returned 0 (runtime init failed)")
	}
	js.h = h
	return finishJS(js)
}

// finishJS resolves and caches the interrupter, tearing down on failure.
func finishJS(js *JS) (*JS, error) {
	ip, err := js.prepareInterrupter()
	if err != nil {
		unmap(js.mmapped)
		return nil, err
	}
	js.intr = ip
	return js, nil
}

func unmap(mem []byte) {
	if mem != nil {
		base.UnmapMemory(mem)
	}
}

// buildWASI configures a fully sandboxed WASI: the guest's environment, its
// stdio (defaulting to empty/discard, never the host's), and a filesystem that
// denies every access. SpiderMonkey exposes no file API for a script to reach it
// anyway; denying at the WASI layer is belt-and-suspenders.
func buildWASI(opts Options) *base.WasiStubs {
	wasi := base.DefaultWASI()
	wasi.SetEnv(opts.Environ)
	if opts.Stdin != nil {
		wasi.SetStdin(opts.Stdin)
	} else {
		wasi.SetStdin(bytes.NewReader(nil))
	}
	if opts.Stdout != nil {
		wasi.SetStdout(opts.Stdout)
	} else {
		wasi.SetStdout(io.Discard)
	}
	if opts.Stderr != nil {
		wasi.SetStderr(opts.Stderr)
	} else {
		wasi.SetStderr(io.Discard)
	}
	wasi.SetFSAccessHook(func(string, bool) bool { return false })
	return wasi
}

// initModule runs the start section (installs the data segments) and
// _initialize (the C++ static constructors) under a recover, so a trap in a
// static initializer surfaces as an error rather than a panic.
func initInstance(g *base.Module) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal: instance init panicked: %v", r)
		}
	}()
	wasm2go.Initialize(g)
	_ = wasm2go.WasmInit(g)
	return nil
}

// --- copy-on-write snapshot (whole started runtime) -------------------------

type snapshot struct {
	img    *base.SharedImage
	handle uint64
	err    error
}

var (
	snapMu    sync.Mutex
	snapshots = map[string]*snapshot{}
)

// snapshotKey is everything the snapshotted interpreter reads once, at startup,
// and bakes into the image. With the heap cap and stack quota fixed internally,
// that is just the environment (the guest's libc caches it during startup).
func snapshotKey(environ []string) string { return fmt.Sprintf("env=%q", environ) }

func sharedSnapshot(environ []string) *snapshot {
	key := snapshotKey(environ)
	snapMu.Lock()
	defer snapMu.Unlock()
	if s, ok := snapshots[key]; ok {
		return s
	}
	s := buildSnapshot(environ)
	snapshots[key] = s
	return s
}

func (s *snapshot) memory(ceiling int) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.img.Memory(ceiling)
}

func buildSnapshot(environ []string) *snapshot {
	if os.Getenv("GO_SPIDERMONKEY_NO_SHARED_IMAGE") != "" {
		return &snapshot{err: fmt.Errorf("shared image disabled by GO_SPIDERMONKEY_NO_SHARED_IMAGE")}
	}
	var handle uint64
	img := base.NewSharedSnapshot(func() (g *base.Module, err error) {
		defer func() {
			if r := recover(); r != nil {
				g, err = nil, fmt.Errorf("starting the interpreter to snapshot panicked: %v", r)
			}
		}()
		wasi := base.DefaultWASI()
		wasi.SetEnv(environ)
		wasi.SetStdin(nil)
		wasi.SetStdout(nil)
		wasi.SetStderr(nil)
		wasi.SetFSAccessHook(func(string, bool) bool { return false })

		g = wasm2go.NewWithWASIReserve(wasi, envStubs{}, defaultMaxMemoryBytes)
		g.MaxMem = uint64(defaultMaxMemoryBytes)
		wasm2go.Initialize(g)
		if rc := wasm2go.WasmInit(g); rc != 0 {
			return nil, fmt.Errorf("_initialize returned %d", rc)
		}
		tmp := &JS{m: &Module{g: g}}
		h, err := tmp.jsNew(0, defaultNativeStackQuota)
		if err != nil {
			return nil, fmt.Errorf("js_new: %w", err)
		}
		if h == 0 {
			return nil, fmt.Errorf("js_new returned 0 (runtime init failed)")
		}
		handle = h
		return g, nil
	})
	if err := img.Err(); err != nil {
		return &snapshot{err: err}
	}
	return &snapshot{img: img, handle: handle}
}

// --- copy-on-write data-segment image (fallback) ----------------------------

func sharedImage() *base.SharedImage { return base.NewSharedImage(buildImage) }

func buildImage() (m *base.Module, err error) {
	if os.Getenv("GO_SPIDERMONKEY_NO_SHARED_IMAGE") != "" {
		return nil, fmt.Errorf("disabled by GO_SPIDERMONKEY_NO_SHARED_IMAGE")
	}
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("the engine's start section panicked: %v", r)
		}
	}()
	wasi := base.DefaultWASI()
	wasi.SetStdin(nil)
	wasi.SetStdout(nil)
	wasi.SetStderr(nil)
	wasi.SetFSAccessHook(func(string, bool) bool { return false })

	m = wasm2go.NewWithWASIReserve(wasi, envStubs{}, defaultMaxMemoryBytes)
	wasm2go.Initialize(m)
	return m, nil
}
