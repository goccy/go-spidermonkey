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
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"

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

	coreExports  map[string][]string // core module -> identifier export names
	http         *httpState
	httpDispatch *spidermonkey.Object // __node_http_dispatch
	httpBody     *spidermonkey.Object // __node_http_body
	net          *netState
	workers      *workerManager
	child        *procState
	io           *ioState

	mu             sync.Mutex
	pendingReturns []*spidermonkey.Object // handles returned to the guest, freed on release_pending
	fds            map[int64]*openFile    // fd table for fs.openSync
	nextFD         int64
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
// ops are exhausted, or ctx is done.
func (rt *Runtime) Wait(ctx context.Context) error { return rt.web.Wait(ctx) }

// Web returns the underlying compat/web installation.
func (rt *Runtime) Web() *web.Web { return rt.web }

// Close releases host resources (open HTTP servers included); the
// interpreter stays usable.
func (rt *Runtime) Close() error {
	rt.closeHTTP()
	rt.closeNet()
	rt.closeChild()
	rt.closeIO()
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
	r, err := rt.js.Eval(ctx, src)
	if err != nil || r.Error != nil {
		return r, err
	}
	return r, rt.Wait(ctx)
}

// RunModule evaluates src as an ES module registered under specifier and
// then runs the event loop to completion.
func (rt *Runtime) RunModule(ctx context.Context, specifier, src string) (spidermonkey.ModuleResult, error) {
	r, err := rt.js.EvalModule(ctx, specifier, src)
	if err != nil || r.Error != nil {
		return r, err
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
// the default export).
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
	switch r.Kind {
	case kindJSON:
		return "export default (" + string(src) + ");", nil
	case kindCJS:
		return fmt.Sprintf("const m = globalThis.__node_require_path(%q);\nexport default m;\n", r.Path), nil
	}
	if registeredPath(specifier, referrer) == r.Path {
		return string(src), nil
	}
	return esmShimFor(specifier, r.Path, src), nil
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
		rt.httpOps(), rt.zlibOps(), rt.crypto2Ops(), rt.crypto3Ops(), rt.netOps(), rt.fsExtraOps(), rt.dgramOps(), rt.workerOps(), rt.childOps(), rt.tlsOps(), rt.ioOps(),
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
	if out != nil {
		io.WriteString(out, args[1].String())
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opImmediateSet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("setImmediate: callback required")
	}
	fn := args[0].Object()
	if fn == nil || !fn.IsFunction() {
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
func fsErrValue(err error) spidermonkey.Value {
	code := "EIO"
	switch {
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
	u8, err := rt.js.NewBytes(b)
	if err != nil {
		return nil, err
	}
	return rt.trackReturn(u8), nil
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
	var data []byte
	if o := args[1].Object(); o != nil {
		data, err = o.Bytes()
		o.Free()
		if err != nil {
			return nil, err
		}
	} else {
		data = []byte(args[1].String())
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
		"mode":    uint32(info.Mode().Perm()),
		"mtimeMs": info.ModTime().UnixMilli(),
	}), nil
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
