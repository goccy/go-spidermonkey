package nodejs

// runtime.go: nodejs.Install — the Node runtime proper. Installs compat/web,
// defines the __node_ops host functions (fs, module resolution, process
// plumbing, immediates), evaluates the embedded JS builtins (process,
// Buffer, require, core modules), wires Node's microtask ordering
// (process.nextTick drains before engine promise jobs at every checkpoint),
// and registers the module loaders (node: prefix + full ESM/CJS fallback).

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
	"github.com/goccy/go-spidermonkey/compat/web"
)

//go:embed js/runtime.js
var runtimeJS string

//go:embed js/corelibs.js
var corelibsJS string

//go:embed js/streams.js
var streamsJS string

//go:embed js/extras.js
var extrasJS string

//go:embed js/http.js
var httpJS string

//go:embed js/extended.js
var extendedJS string

// Options configures Install.
type Options struct {
	// Argv becomes process.argv. Empty means ["node", "main"].
	Argv []string
}

// Runtime is one Node.js-compatible installation on one interpreter.
type Runtime struct {
	js   *spidermonkey.JS
	web  *web.Web
	loop *eventloop.Loop
	opts Options

	coreExports    map[string][]string // core module -> identifier export names
	http           *httpState
	httpDispatch   *spidermonkey.Object // __node_http_dispatch
	httpBody       *spidermonkey.Object // __node_http_body
	httpAborted    *spidermonkey.Object // __node_http_aborted (client disconnect)
	httpClientBody *spidermonkey.Object // __node_http_client_body (streaming client response body)
	net            *netState
	workers        *workerManager
	child          *procState
	io             *ioState

	mu             sync.Mutex
	pendingReturns []*spidermonkey.Object // handles returned to the guest, freed on release_pending
	fds            map[int64]*openFile    // fd table for fs.openSync
	nextFD         int64
	zstreams       map[int64]*zlibStream // live incremental zlib streams (createGzip etc.)
	nextZStream    int64
	exited         bool // process.exit() was called; the exit sentinel is not a crash
	exitCode       int
	started        time.Time // process start; backs cpuUsage/resourceUsage elapsed
}

// ExitCode reports the code passed to process.exit() (0 if it exited without an
// explicit code); Exited reports whether process.exit() was called at all.
func (rt *Runtime) Exited() bool  { rt.mu.Lock(); defer rt.mu.Unlock(); return rt.exited }
func (rt *Runtime) ExitCode() int { rt.mu.Lock(); defer rt.mu.Unlock(); return rt.exitCode }

// exitFilter converts the process.exit() unwind sentinel into a clean return: a
// script that calls process.exit(0) is a normal termination, not an error.
func (rt *Runtime) exitFilter(err error) error {
	if err != nil && rt.Exited() {
		return nil
	}
	return err
}

// resetExit clears the exit state at the start of each run so a process.exit()
// from a PRIOR run on a reused Runtime can't make this run's genuine error read
// as a clean exit.
func (rt *Runtime) resetExit() {
	rt.mu.Lock()
	rt.exited = false
	rt.exitCode = 0
	rt.mu.Unlock()
	// Allow 'exit' to fire again for this run on a reused Runtime.
	_, _ = rt.js.Eval(context.Background(), "globalThis.__node_reset_exit_emitted && globalThis.__node_reset_exit_emitted()")
}

// openFile is one fs.openSync handle: the whole file loaded into memory,
// position-tracked; writes flush back on close.
type openFile struct {
	path   string
	data   []byte
	pos    int64
	write  bool
	append bool
	dirty  bool
	cfg    spidermonkey.Config
}

// Install sets up the Node runtime on js (installing compat/web itself — do
// not call web.Install separately on the same interpreter).
func Install(js *spidermonkey.JS, opts ...Options) (*Runtime, error) {
	rt := &Runtime{
		js:          js,
		coreExports: map[string][]string{},
		http:        &httpState{servers: map[int64]*httpServer{}, reqs: map[int64]*httpPending{}},
		net:         newNetState(),
		child:       newProcState(),
		io:          newIOState(),
		fds:         map[int64]*openFile{},
		nextFD:      3, // 0,1,2 reserved for stdio
		started:     time.Now(),
	}
	if len(opts) > 0 {
		rt.opts = opts[0]
	}
	if len(rt.opts.Argv) == 0 {
		rt.opts.Argv = []string{"node", "main"}
	}

	w, err := web.Install(js)
	if err != nil {
		return nil, err
	}
	rt.web = w
	rt.loop = w.Loop()
	rt.workers = newWorkerManager(rt)

	ops, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	defer ops.Free()
	for name, fn := range rt.ops() {
		if err := ops.DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	if err := js.Global().Set("__node_ops", ops); err != nil {
		return nil, err
	}

	ctx := context.Background()
	for _, src := range []string{runtimeJS, corelibsJS, streamsJS, extrasJS, httpJS, extendedJS, `delete globalThis.__node_ops;`} {
		r, err := js.Eval(ctx, src)
		if err != nil {
			return nil, fmt.Errorf("nodejs: evaluating builtins: %w", err)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("nodejs: builtins threw: %w", r.Error)
		}
	}

	v, err := js.Global().Get("__node_http_dispatch")
	if err != nil {
		return nil, err
	}
	if o := v.Object(); o != nil && o.IsFunction() {
		rt.httpDispatch = o
	} else {
		return nil, fmt.Errorf("nodejs: __node_http_dispatch missing")
	}
	bv, err := js.Global().Get("__node_http_body")
	if err != nil {
		return nil, err
	}
	if o := bv.Object(); o != nil && o.IsFunction() {
		rt.httpBody = o
	} else {
		return nil, fmt.Errorf("nodejs: __node_http_body missing")
	}
	av, err := js.Global().Get("__node_http_aborted")
	if err != nil {
		return nil, err
	}
	if o := av.Object(); o != nil && o.IsFunction() {
		rt.httpAborted = o
	} else {
		return nil, fmt.Errorf("nodejs: __node_http_aborted missing")
	}
	cbv, err := js.Global().Get("__node_http_client_body")
	if err != nil {
		return nil, err
	}
	if o := cbv.Object(); o != nil && o.IsFunction() {
		rt.httpClientBody = o
	} else {
		return nil, fmt.Errorf("nodejs: __node_http_client_body missing")
	}

	if err := rt.collectCoreExports(ctx); err != nil {
		return nil, err
	}
	js.RegisterModuleResolver("node:", func(_ spidermonkey.Config, specifier, referrer string) (string, error) {
		name := strings.TrimPrefix(specifier, "node:")
		if !coreModules[name] {
			return "", fmt.Errorf("unknown builtin module %q", specifier)
		}
		return rt.coreShim(name), nil
	})
	js.SetModuleLoader(rt.esmLoader)
	return rt, nil
}

// Wait runs the event loop until timers, immediates, nextTicks and pending
// ops are exhausted, or ctx is done. A process.exit() during the loop stops it
// and returns cleanly (Exited/ExitCode report the outcome). When the loop
// drains naturally it fires process 'beforeExit' (draining again if a handler
// scheduled work), and it fires 'exit' once on termination.
func (rt *Runtime) Wait(ctx context.Context) error {
	err := rt.web.Wait(ctx)
	if err == nil && !rt.Exited() {
		// Loop drained without error or process.exit: fire 'beforeExit'. If a
		// listener exists (and may have scheduled work), drain once more.
		if r, e := rt.js.Eval(ctx, "!!(globalThis.__node_emit_before_exit && globalThis.__node_emit_before_exit())"); e != nil {
			err = e
		} else if r.Value.Bool() {
			err = rt.web.Wait(ctx)
		}
	}
	err = rt.exitFilter(err)
	// Fire 'exit' exactly once (the JS side guards re-entry). Best-effort on the
	// teardown path; a ctx error still returns below.
	if _, e := rt.js.Eval(context.Background(), "globalThis.__node_emit_exit && globalThis.__node_emit_exit()"); e != nil && err == nil {
		err = e
	}
	return err
}

// Web returns the underlying compat/web installation.
func (rt *Runtime) Web() *web.Web { return rt.web }

// Close releases host resources (open HTTP servers included); the
// interpreter stays usable.
func (rt *Runtime) Close() error {
	rt.closeHTTP()
	rt.closeNet()
	rt.closeChild()
	rt.closeIO()
	rt.closeZlib()
	rt.workers.close()
	rt.mu.Lock()
	pending := rt.pendingReturns
	rt.pendingReturns = nil
	rt.mu.Unlock()
	for _, o := range pending {
		o.Free()
	}
	return rt.web.Close()
}

// RunScript evaluates src as a classic script (require available) and then
// runs the event loop to completion.
func (rt *Runtime) RunScript(ctx context.Context, src string) (spidermonkey.Result, error) {
	rt.resetExit()
	r, err := rt.js.Eval(ctx, src)
	if err != nil {
		return r, err
	}
	if r.Error != nil {
		// A top-level process.exit() surfaces as the unwind sentinel; report it as
		// a clean exit rather than an evaluation error, and don't run the loop.
		if rt.Exited() {
			r.Error = nil
			return r, nil
		}
		return r, nil
	}
	return r, rt.Wait(ctx)
}

// moduleSchemeRE recognizes a specifier that already carries a URL scheme
// (file:, node:, https:, ...), which must be registered verbatim.
var moduleSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

// entrySpecifier is the registration key for the entry module: a path-shaped
// specifier ("main.mjs", "/app/src/main.mjs") registers under its canonical
// file:// URL so import.meta.url is a real URL (new URL("./x",
// import.meta.url) works, as in Node) and the engine's relative-import
// joining still resolves. A specifier that already has a scheme is kept.
// The path is percent-encoded (space, non-ASCII, "%", "#", "?") so the
// registered URL — what import.meta.url reports — is a VALID URL that
// new URL(relative, import.meta.url) can join against.
func entrySpecifier(specifier string) string {
	if moduleSchemeRE.MatchString(specifier) {
		return specifier
	}
	return "file://" + fsPathToFileURLPath(path.Clean("/"+specifier))
}

// RunModule evaluates src as an ES module registered under specifier and
// then runs the event loop to completion. A path-shaped specifier registers
// under its file:// URL (see entrySpecifier), which is what import.meta.url
// reports inside the module.
func (rt *Runtime) RunModule(ctx context.Context, specifier, src string) (spidermonkey.ModuleResult, error) {
	rt.resetExit()
	r, err := rt.js.EvalModule(ctx, entrySpecifier(specifier), src)
	if err != nil {
		return r, err
	}
	if r.Error != nil {
		if rt.Exited() {
			r.Error = nil
			return r, nil
		}
		return r, nil
	}
	return r, rt.Wait(ctx)
}

// collectCoreExports records each core module's identifier-shaped export
// names, for generating static `export const` shims. (Done eagerly: the
// module loader cannot re-enter the interpreter.)
func (rt *Runtime) collectCoreExports(ctx context.Context) error {
	for name := range coreModules {
		r, err := rt.js.Eval(ctx, `Object.keys(globalThis.__node_core(`+strconv.Quote(name)+`)).join(",")`)
		if err != nil {
			return err
		}
		if r.Error != nil {
			return fmt.Errorf("nodejs: enumerating %s exports: %w", name, r.Error)
		}
		var names []string
		for _, n := range strings.Split(r.Value.String(), ",") {
			if identRE.MatchString(n) && !reservedWords[n] {
				names = append(names, n)
			}
		}
		rt.coreExports[name] = names
	}
	return nil
}

var identRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

var reservedWords = map[string]bool{
	"default": true, "class": true, "function": true, "var": true,
	"let": true, "const": true, "new": true, "delete": true, "in": true,
	"of": true, "if": true, "else": true, "return": true, "this": true,
	"typeof": true, "void": true, "with": true, "yield": true, "await": true,
	"static": true, "export": true, "import": true, "super": true,
	"extends": true, "enum": true, "null": true, "true": true, "false": true,
	"do": true, "while": true, "for": true, "switch": true, "case": true,
	"try": true, "catch": true, "finally": true, "throw": true, "break": true,
	"continue": true, "debugger": true, "instanceof": true,
}

// coreShim is the ESM view of a core module: default export plus static
// named re-exports.
func (rt *Runtime) coreShim(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "const m = globalThis.__node_core(%q);\nexport default m;\n", name)
	for _, n := range rt.coreExports[name] {
		fmt.Fprintf(&b, "export const %s = m.%s;\n", n, n)
	}
	return b.String()
}

// esmLoader is the fallback module loader: full Node resolution with
// ESM⇄CJS interop (a CJS target evaluates through require and surfaces as
// the default export plus statically detected named exports).
func (rt *Runtime) esmLoader(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
	r, err := resolveModule(cfg.FS, specifier, referrer, false)
	if err != nil {
		return "", err
	}
	if r.Core != "" {
		return rt.coreShim(r.Core), nil
	}
	src, err := readModuleFile(cfg, r.Path)
	if err != nil {
		return "", err
	}
	rt.refineKind(cfg.FS, &r, src)
	switch r.Kind {
	case kindJSON:
		return jsonModuleSource(src)
	case kindCJS:
		return rt.cjsShim(cfg.FS, r.Path, src), nil
	}
	// Every ESM file registers under its canonical file:// URL, so
	// import.meta.url is a real URL in EVERY module (not just the entry) and
	// any two import spellings of the same file dedupe to one instance. A
	// non-canonical specifier — the engine's path-joined form ("file:/a/b.mjs",
	// "a/b.mjs"), a bare package name, an extension-less path — answers with a
	// re-export shim onto the canonical URL; the engine passes absolute
	// specifiers to the loader verbatim, where they match canonical and get
	// the real source. The canonical URL uses the percent-ENCODED path (the
	// same spelling entrySpecifier produces), so import.meta.url is a valid
	// URL for paths with spaces/non-ASCII, and encoded vs raw spellings of
	// one file both funnel (via the re-export shim) into ONE instance.
	canonical := "file:///" + fsPathToFileURLPath(r.Path)
	if specifier == canonical {
		return string(src), nil
	}
	return canonicalESMShim(canonical, src), nil
}

// canonicalESMShim re-exports the module registered at its canonical file://
// URL. `export * from` cannot forward a default export, so one is added when
// the target visibly declares it.
func canonicalESMShim(canonical string, src []byte) string {
	shim := fmt.Sprintf("export * from %q;\n", canonical)
	if hasDefaultExport(src) {
		shim += fmt.Sprintf("export { default } from %q;\n", canonical)
	}
	return shim
}

// ---------------------------------------------------------------- host ops

func (rt *Runtime) ops() map[string]spidermonkey.Func {
	table := map[string]spidermonkey.Func{
		"node_env":        rt.opEnv,
		"node_argv":       rt.opArgv,
		"node_platform":   rt.opPlatform,
		"raw_write":       rt.opRawWrite,
		"immediate_set":   rt.opImmediateSet,
		"immediate_clear": rt.opImmediateClear,
		"loop_ref":        rt.opLoopRef,
		"node_exit":       rt.opNodeExit,
		"node_resolve":    rt.opResolve,
		"node_read":       rt.opRead,
		"fs_read_file":    rt.opFSReadFile,
		"fs_write_file":   rt.opFSWriteFile,
		"fs_stat":         rt.opFSStat,
		"fs_readdir":      rt.opFSReaddir,
		"fs_mkdir":        rt.opFSMkdir,
		"fs_remove":       rt.opFSRemove,
		"fs_rename":       rt.opFSRename,
		"fs_exists":       rt.opFSExists,
		"release_pending": rt.opReleasePending,
		"crypto_hash":     rt.opCryptoHash,
		"crypto_hmac":     rt.opCryptoHMAC,
	}
	for _, group := range []map[string]spidermonkey.Func{
		rt.httpOps(), rt.zlibOps(), rt.crypto2Ops(), rt.crypto3Ops(), rt.netOps(), rt.fsExtraOps(), rt.dgramOps(), rt.workerOps(), rt.childOps(), rt.tlsOps(), rt.ioOps(), rt.sysOps(),
	} {
		for name, fn := range group {
			table[name] = fn
		}
	}
	return table
}

func (rt *Runtime) opEnv(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	env := map[string]string{}
	for _, kv := range cfg.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return spidermonkey.ValueOf(env), nil
}

func (rt *Runtime) opArgv(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	return spidermonkey.ValueOf(rt.opts.Argv), nil
}

func (rt *Runtime) opPlatform(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	p := goruntime.GOOS
	if p == "windows" {
		p = "win32"
	}
	return spidermonkey.ValueOf(p), nil
}

func (rt *Runtime) opRawWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.Undefined(), nil
	}
	out := cfg.Stdout
	if args[0].Int() != 0 {
		out = cfg.Stderr
	}
	if out == nil {
		return spidermonkey.Undefined(), nil
	}
	// A Buffer/Uint8Array chunk is written as raw bytes so binary stdout (gzip,
	// images, protobuf) isn't corrupted by a UTF-8 round-trip; a string is written
	// verbatim.
	if args[1].IsObject() {
		if b, err := valueBytes(args[1]); err == nil {
			out.Write(b)
			return spidermonkey.Undefined(), nil
		}
	}
	io.WriteString(out, args[1].String())
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opImmediateSet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("setImmediate: callback required")
	}
	fn := args[0].Object()
	if fn == nil || !fn.IsFunction() {
		fn.Free() // taken but unusable; releasing it keeps the error path leak-free
		return nil, fmt.Errorf("setImmediate: callback is not a function")
	}
	return spidermonkey.ValueOf(rt.loop.PostImmediate(fn)), nil
}

func (rt *Runtime) opImmediateClear(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) >= 1 {
		rt.loop.ClearImmediate(int64(args[0].Float()))
	}
	return spidermonkey.Undefined(), nil
}

// opNodeExit(code) records a process.exit() request. process.exit then throws
// an unwind sentinel to stop execution; the host boundaries (RunScript/Wait)
// consult this flag to report the exit as a clean termination rather than a
// crash. It also stops the loop from running further work.
func (rt *Runtime) opNodeExit(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	rt.mu.Lock()
	rt.exited = true
	if len(args) >= 1 {
		rt.exitCode = args[0].Int()
	}
	rt.mu.Unlock()
	return spidermonkey.Undefined(), nil
}

// opLoopRef(ref) toggles whether an AddPending handle (a listening server, etc.)
// keeps the loop alive — the Go half of server.ref()/unref(). The JS caller
// guards it so ref state stays balanced.
func (rt *Runtime) opLoopRef(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) >= 1 && args[0].Bool() {
		rt.loop.Ref()
	} else {
		rt.loop.Unref()
	}
	return spidermonkey.Undefined(), nil
}

// opResolve implements require's resolution: {core} | {path, kind} |
// {code: "MODULE_NOT_FOUND", message}.
func (rt *Runtime) opResolve(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("node_resolve: (specifier, parent) required")
	}
	r, err := resolveModule(cfg.FS, args[0].String(), guestPath(args[1].String()), true)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "MODULE_NOT_FOUND", "message": err.Error()}), nil
	}
	if r.Core != "" {
		return spidermonkey.ValueOf(map[string]any{"core": r.Core}), nil
	}
	rt.refineKind(cfg.FS, &r, nil)
	kind := "cjs"
	switch r.Kind {
	case kindESM:
		kind = "esm"
	case kindJSON:
		kind = "json"
	}
	return spidermonkey.ValueOf(map[string]any{"path": r.Path, "kind": kind}), nil
}

func (rt *Runtime) opRead(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("node_read: path required")
	}
	p := guestPath(args[0].String())
	src, err := readModuleFile(cfg, p)
	if err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(string(src)), nil
}

// ------------------------------------------------------------------ fs ops

// guestPath maps a guest path ("/a/b", "./a") onto the fs.FS namespace.
func guestPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "."
	}
	cleaned := strings.TrimPrefix(strings.ReplaceAll("/"+p, "//", "/"), "/")
	cleaned = pathClean(cleaned)
	if cleaned == "" {
		return "."
	}
	return cleaned
}

func pathClean(p string) string {
	out := []string{}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// fsErrValue shapes a Go error as the {code, message} object the JS fs
// wrappers convert into Node-style errors.
// optScalar reads o[name] as a primitive option. If the guest supplied an
// object (which cannot be a valid scalar), its persistent root is freed and the
// option is reported absent — never leaked. Missing/undefined is also absent.
func optScalar(o *spidermonkey.Object, name string) (spidermonkey.Value, bool) {
	v, _ := o.Get(name)
	if v == nil || v.IsUndefined() {
		return nil, false
	}
	if obj := v.Object(); obj != nil {
		obj.Free()
		return nil, false
	}
	return v, true
}

// intArg reads a positional numeric argument, freeing its persistent root if
// the guest passed an object where a number is expected. Value.Int() returns 0
// for an object receiver without releasing the root, so a wrapper that forwards
// an object (e.g. saltLength / keylen) would otherwise pin it, and its backing
// store, for the interpreter's life on every call (unbounded exhaustion).
func intArg(v spidermonkey.Value) int {
	freeObjects(v.Object())
	return v.Int()
}

// strArg is intArg for a string argument. Value.String() runs the object's
// guest toString (so it is read BEFORE the free), but the read never releases
// the root; a wrapper that forwards the value raw would otherwise pin it.
func strArg(v spidermonkey.Value) string {
	s := v.String()
	freeObjects(v.Object())
	return s
}

// optPresent reports whether name is set to a non-undefined value, freeing the
// value's root if it is object-typed (e.g. a Buffer passphrase) — for presence
// checks that must count objects as present without pinning them.
func optPresent(o *spidermonkey.Object, name string) bool {
	v, _ := o.Get(name)
	if v == nil || v.IsUndefined() {
		return false
	}
	freeObjects(v.Object())
	return true
}

// errnoError carries a POSIX errno code directly (the fd ops raise e.g.
// EBADF/EFBIG), so fsErrValue reads the code from the value instead of matching
// on the message text.
type errnoError string

func (e errnoError) Error() string { return string(e) }

func fsErrValue(err error) spidermonkey.Value {
	code := "EIO"
	var errno errnoError
	switch {
	case errors.As(err, &errno):
		code = string(errno)
	case errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case errors.Is(err, fs.ErrExist):
		code = "EEXIST"
	case errors.Is(err, fs.ErrPermission), strings.Contains(err.Error(), "permission denied"):
		code = "EACCES"
	case errors.Is(err, fs.ErrInvalid):
		code = "EINVAL"
	case strings.Contains(err.Error(), "read-only"):
		code = "EROFS"
	}
	return spidermonkey.ValueOf(map[string]any{"code": code, "message": err.Error()})
}

var errReadOnlyFS = errors.New("filesystem is read-only (Config.FS does not implement WritableFS)")

func writableFS(cfg spidermonkey.Config) (spidermonkey.WritableFS, error) {
	if cfg.FS == nil {
		return nil, fmt.Errorf("no filesystem configured: %w", fs.ErrNotExist)
	}
	w, ok := cfg.FS.(spidermonkey.WritableFS)
	if !ok {
		return nil, errReadOnlyFS
	}
	return w, nil
}

// trackReturn pins obj until the guest calls release_pending (immediately
// after copying the value out), so returned handles do not accumulate.
func (rt *Runtime) trackReturn(obj *spidermonkey.Object) spidermonkey.Value {
	rt.mu.Lock()
	rt.pendingReturns = append(rt.pendingReturns, obj)
	rt.mu.Unlock()
	return obj
}

func (rt *Runtime) opReleasePending(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	rt.mu.Lock()
	pending := rt.pendingReturns
	rt.pendingReturns = nil
	rt.mu.Unlock()
	for _, o := range pending {
		o.Free()
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opFSReadFile(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_read_file: path required")
	}
	p := guestPath(args[0].String())
	b, err := readFile(cfg.FS, p)
	if err != nil {
		return fsErrValue(err), nil
	}
	return rt.bytesReturn(b)
}

func (rt *Runtime) opFSWriteFile(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("fs_write_file: (path, data, append) required")
	}
	p := guestPath(args[0].String())
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if args[2].Bool() {
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := wfs.OpenFile(p, flag, 0o644)
	if err != nil {
		return fsErrValue(err), nil
	}
	w, ok := f.(io.Writer)
	if !ok {
		f.Close()
		return fsErrValue(errReadOnlyFS), nil
	}
	if _, err := w.Write(data); err != nil {
		f.Close()
		return fsErrValue(err), nil
	}
	if err := f.Close(); err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

func (rt *Runtime) opFSStat(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_stat: path required")
	}
	p := guestPath(args[0].String())
	if cfg.FS == nil {
		return fsErrValue(fs.ErrNotExist), nil
	}
	info, err := fs.Stat(cfg.FS, p)
	if err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{
		"size":    info.Size(),
		"dir":     info.IsDir(),
		"mode":    statMode(info),
		"mtimeMs": info.ModTime().UnixMilli(),
	}), nil
}

// statMode is Node's stats.mode: the permission bits ORed with the file-type
// bits (S_IFDIR/S_IFREG/S_IFLNK), so `(mode & S_IFMT) === S_IFDIR` works.
func statMode(info fs.FileInfo) uint32 {
	mode := uint32(info.Mode().Perm())
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		mode |= 0o120000 // S_IFLNK
	case info.IsDir():
		mode |= 0o040000 // S_IFDIR
	default:
		mode |= 0o100000 // S_IFREG
	}
	return mode
}

func (rt *Runtime) opFSReaddir(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_readdir: path required")
	}
	p := guestPath(args[0].String())
	if cfg.FS == nil {
		return fsErrValue(fs.ErrNotExist), nil
	}
	entries, err := fs.ReadDir(cfg.FS, p)
	if err != nil {
		return fsErrValue(err), nil
	}
	names := make([]string, len(entries))
	dirs := make([]bool, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
		dirs[i] = e.IsDir()
	}
	return spidermonkey.ValueOf(map[string]any{"names": names, "dirs": dirs}), nil
}

func (rt *Runtime) opFSMkdir(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("fs_mkdir: (path, recursive) required")
	}
	p := guestPath(args[0].String())
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	if !args[1].Bool() {
		if err := wfs.Mkdir(p, 0o755); err != nil {
			return fsErrValue(err), nil
		}
		return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
	}
	segs := strings.Split(p, "/")
	for i := range segs {
		dir := strings.Join(segs[:i+1], "/")
		if info, serr := fs.Stat(wfs, dir); serr == nil && info.IsDir() {
			continue
		}
		if err := wfs.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fsErrValue(err), nil
		}
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

func (rt *Runtime) opFSRemove(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_remove: path required")
	}
	p := guestPath(args[0].String())
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	if err := wfs.Remove(p); err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

func (rt *Runtime) opFSRename(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("fs_rename: (old, new) required")
	}
	oldp, newp := guestPath(args[0].String()), guestPath(args[1].String())
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	if err := wfs.Rename(oldp, newp); err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

func (rt *Runtime) opFSExists(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 || cfg.FS == nil {
		return spidermonkey.ValueOf(false), nil
	}
	p := guestPath(args[0].String())
	// A policy FS may hide the path (fs.ErrNotExist) or deny it; either way
	// existsSync reports false.
	_, err := fs.Stat(cfg.FS, p)
	return spidermonkey.ValueOf(err == nil), nil
}
