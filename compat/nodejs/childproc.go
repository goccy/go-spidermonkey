package nodejs

// childproc.go: node:child_process over Go os/exec, gated by Config.Exec.
// Async spawns run on a goroutine that posts stdout/stderr chunks and the
// exit onto the event loop; sync spawns block the op and return the result.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// signalByName maps a Node signal name to the OS signal; unknown names and the
// empty string default to SIGTERM (Node's default for ChildProcess.kill()).
func signalByName(name string) os.Signal {
	switch name {
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGINT":
		return syscall.SIGINT
	case "SIGHUP":
		return syscall.SIGHUP
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGUSR1":
		return syscall.SIGUSR1
	case "SIGUSR2":
		return syscall.SIGUSR2
	default:
		return syscall.SIGTERM
	}
}

type procState struct {
	mu    sync.Mutex
	procs map[int64]*exec.Cmd
	stdin map[int64]*connWriter // async stdin pipe writer (off-loop, ordered)
	// nextNestedPID numbers the children that are nested interpreters rather
	// than OS processes; their ids are negative so they can never collide with
	// a real pid.
	nextNestedPID int64
	// ipc holds the fork() channel of each nested child, keyed by its id.
	ipc map[int64]*ipcChannel
}

func newProcState() *procState {
	return &procState{procs: map[int64]*exec.Cmd{}, stdin: map[int64]*connWriter{}, ipc: map[int64]*ipcChannel{}}
}

func (rt *Runtime) childOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"child_spawn":     rt.opChildSpawn,
		"child_stdin":     rt.opChildStdin,
		"child_kill":      rt.opChildKill,
		"child_spawnsync": rt.opChildSpawnSync,
	}
}

// execAllowed enforces Config.Exec (a nil hook denies by default — subprocess
// is high-risk, so an embedder must opt in explicitly).
func execAllowed(cfg spidermonkey.Config, path string, argv []string) error {
	if cfg.Exec == nil {
		return fmt.Errorf("child_process is disabled: Config.Exec is not set")
	}
	if !cfg.Exec(path, argv) {
		return fmt.Errorf("spawn %s: permission denied", path)
	}
	return nil
}

func cmdArgv(o *spidermonkey.Object) (string, []string, error) {
	file, err := o.Get("file")
	if err != nil {
		return "", nil, err
	}
	argsV, _ := o.Get("args")
	var args []string
	if a := argsV.Object(); a != nil {
		defer a.Free()
		lenV, _ := a.Get("length")
		for i := 0; i < lenV.Int(); i++ {
			iv, _ := a.Get(fmt.Sprint(i))
			args = append(args, iv.String())
		}
	}
	return file.String(), args, nil
}

// opChildSpawn(optsObj, onStdout, onStderr, onExit, onError) -> {pid} | err.
func (rt *Runtime) opChildSpawn(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("child_spawn: (opts, onStdout, onStderr, onExit, onError) required")
	}
	opts := args[0].Object()
	if opts == nil {
		return nil, fmt.Errorf("child_spawn: opts must be an object")
	}
	defer opts.Free()
	// The four callback args are GC-pinned at decode; free them on any early
	// return that doesn't reach the success path (which frees them in onExit).
	freeCallbacks := func() { freeObjects(args[1].Object(), args[2].Object(), args[3].Object(), args[4].Object()) }
	file, argv, err := cmdArgv(opts)
	if err != nil {
		freeCallbacks()
		return nil, err
	}
	if err := execAllowed(cfg, file, argv); err != nil {
		freeCallbacks()
		return childErr(err), nil
	}
	onStdout := args[1].Object()
	onStderr := args[2].Object()
	onExit := args[3].Object()
	onError := args[4].Object()

	// Spawning THIS runtime runs a nested interpreter (see nested.go) with the
	// same pipes an OS process would have had.
	if isSelfExec(file) {
		return rt.spawnNested(cfg, opts, argv, onStdout, onStderr, onExit, onError)
	}

	cmd := exec.Command(file, argv...)
	isolateProcessGroup(cmd)
	applyCwdEnv(cmd, opts, cfg.Env)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		// Start failed (ENOENT/EACCES/bad cwd, …): the success path frees these
		// four callback handles in the onExit Post, which never runs here, so free
		// them now or every allowed-but-failed spawn leaks guest handle slots.
		for _, o := range []*spidermonkey.Object{onStdout, onStderr, onExit, onError} {
			if o != nil {
				o.Free()
			}
		}
		return childErr(err), nil
	}

	st := rt.child
	st.mu.Lock()
	id := int64(cmd.Process.Pid)
	st.procs[id] = cmd
	if stdin != nil {
		// Writes go through an off-loop actor so a child that stops reading its
		// stdin can't block the event loop on a full pipe buffer.
		w := newConnWriter()
		w.attach(stdin)
		go w.run(func(error) {})
		st.stdin[id] = w
	}
	st.mu.Unlock()

	rt.loop.AddPending("childproc")
	wgOut := rt.pipeToCallback(stdout, onStdout)
	wgErr := rt.pipeToCallback(stderr, onStderr)

	go func() {
		wgOut.Wait()
		wgErr.Wait()
		werr := cmd.Wait()
		code, signal := exitInfo(werr)
		st.mu.Lock()
		delete(st.procs, id)
		sw := st.stdin[id]
		delete(st.stdin, id)
		st.mu.Unlock()
		if sw != nil {
			sw.requestClose() // stop the stdin write actor
		}
		rt.loop.Post(func() error {
			if onExit != nil {
				onExit.Call(spidermonkey.ValueOf(code), spidermonkey.ValueOf(signal))
			}
			for _, o := range []*spidermonkey.Object{onStdout, onStderr, onExit, onError} {
				if o != nil {
					o.Free()
				}
			}
			return nil
		})
		rt.loop.DonePending("childproc")
	}()

	return spidermonkey.ValueOf(map[string]any{"pid": id}), nil
}

// pipeToCallback streams a child's output to a guest callback, one chunk per
// read, on the loop goroutine.
func (rt *Runtime) pipeToCallback(r interface{ Read([]byte) (int, error) }, cb *spidermonkey.Object) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	if r == nil || cb == nil {
		return wg
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				rt.loop.Post(func() error {
					u8, e := rt.js.NewBytes(chunk)
					if e == nil {
						cb.Call(u8)
						u8.Free()
					}
					return nil
				})
			}
			if rerr != nil {
				return
			}
		}
	}()
	return wg
}

func (rt *Runtime) opChildStdin(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("child_stdin: (pid, data|null) required")
	}
	pid := int64(args[0].Float())
	st := rt.child
	st.mu.Lock()
	w := st.stdin[pid]
	st.mu.Unlock()
	if w == nil {
		// The child's stdin is gone (exited): free the data Buffer arg (the
		// success path frees it via valueBytes) so a write-after-exit doesn't leak.
		freeObjects(args[1].Object())
		return spidermonkey.ValueOf(false), nil
	}
	if args[1].IsUndefined() || args[1].Object() == nil && args[1].Export() == nil {
		w.requestClose() // flush queued writes, then close the pipe
		st.mu.Lock()
		delete(st.stdin, pid)
		st.mu.Unlock()
		return spidermonkey.ValueOf(true), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(w.enqueue(data, nil)), nil
}

func (rt *Runtime) opChildKill(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	pid := int64(args[0].Float())
	sig := os.Signal(syscall.SIGTERM)
	if len(args) > 1 && !args[1].IsUndefined() {
		sig = signalByName(args[1].String())
	}
	st := rt.child
	st.mu.Lock()
	cmd := st.procs[pid]
	st.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		// Honor the requested signal (Node's kill(signal)); default SIGTERM.
		cmd.Process.Signal(sig)
	}
	return spidermonkey.Undefined(), nil
}

// defaultMaxBuffer is Node's cap on the output a synchronous child may return
// (1 MiB). Without one, a child that writes more than the guest's whole linear
// memory crashes the host outright when the bytes are copied in — a real
// SIGSEGV, not an error. Node reports ENOBUFS and truncates.
const defaultMaxBuffer = 1024 * 1024

// capOutput truncates a captured stream to the caller's maxBuffer (or Node's
// default) and reports whether it had to.
func capOutput(b []byte, max int) ([]byte, bool) {
	if max <= 0 {
		max = defaultMaxBuffer
	}
	if len(b) <= max {
		return b, false
	}
	return b[:max], true
}

// opChildSpawnSync(opts, input) -> {status, signal, stdout, stderr, pid, error}.
func (rt *Runtime) opChildSpawnSync(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("child_spawnsync: opts required")
	}
	opts := args[0].Object()
	if opts == nil {
		return nil, fmt.Errorf("child_spawnsync: opts must be an object")
	}
	defer opts.Free()
	// The input Buffer arg is GC-pinned at decode; the success path frees it via
	// valueBytes, so free it on the early error returns too.
	freeInput := func() {
		if len(args) > 1 {
			freeObjects(args[1].Object())
		}
	}
	file, argv, err := cmdArgv(opts)
	if err != nil {
		freeInput()
		return nil, err
	}
	if err := execAllowed(cfg, file, argv); err != nil {
		freeInput()
		return childErr(err), nil
	}
	// Spawning THIS runtime runs a nested interpreter rather than an OS process
	// (see nested.go): there is no node binary to exec, and a child "node" is
	// what a large part of Node's own suite is written against.
	if isSelfExec(file) {
		var stdout, stderr bytes.Buffer
		var stdin io.Reader
		if len(args) > 1 && !args[1].IsUndefined() {
			if in, ierr := valueBytes(args[1]); ierr == nil && len(in) > 0 {
				stdin = bytes.NewReader(in)
			}
		}
		res := rt.runNested(cfg, argv, envList(opts, cfg.Env), optString(opts, "cwd"),
			stdin, &stdout, &stderr, 0)
		maxBuf := optInt(opts, "maxBuffer")
		out, outCut := capOutput(stdout.Bytes(), maxBuf)
		errOut, errCut := capOutput(stderr.Bytes(), maxBuf)
		nestedErr := res.err
		if (outCut || errCut) && nestedErr == nil {
			nestedErr = errnoError("ENOBUFS")
		}
		return rt.spawnSyncResult(out, errOut, res.exitCode, nestedErr)
	}

	cmd := exec.Command(file, argv...)
	isolateProcessGroup(cmd)
	applyCwdEnv(cmd, opts, cfg.Env)
	if len(args) > 1 && !args[1].IsUndefined() {
		if in, ierr := valueBytes(args[1]); ierr == nil && len(in) > 0 {
			cmd.Stdin = bytes.NewReader(in)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	werr := cmd.Run()
	code, signal := exitInfo(werr)
	result := map[string]any{
		"status": code,
		"signal": signal,
		"pid":    0,
	}
	if cmd.Process != nil {
		result["pid"] = cmd.Process.Pid
	}
	// Set error only for a genuine spawn/exec failure (ENOENT, etc.) — not a
	// signal death (signal != nil) and not a non-zero exit (code is an int).
	if werr != nil && code == nil && signal == nil {
		result["error"] = werr.Error()
	}
	outObj, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	for k, v := range result {
		outObj.Set(k, spidermonkey.ValueOf(v))
	}
	// Bound what a child can hand back, as Node's maxBuffer does. Copying an
	// unbounded capture into the guest's linear memory is a host crash, not an
	// error the program can see.
	capped, outCut := capOutput(stdout.Bytes(), optInt(opts, "maxBuffer"))
	cappedErr, errCut := capOutput(stderr.Bytes(), optInt(opts, "maxBuffer"))
	if (outCut || errCut) && result["error"] == nil {
		result["error"] = "ENOBUFS: stdout maxBuffer length exceeded"
		outObj.Set("error", spidermonkey.ValueOf(result["error"]))
	}
	so, _ := rt.js.NewBytes(capped)
	se, _ := rt.js.NewBytes(cappedErr)
	defer so.Free()
	defer se.Free()
	outObj.Set("stdout", so)
	outObj.Set("stderr", se)
	return rt.trackReturn(outObj), nil
}

// applyCwdEnv reads opts.cwd and opts.envArray (the JS side flattens an env
// object into a ["KEY=VALUE", ...] array, undefined when inheriting).
// spawnNested is opChildSpawn's path for a child "node": a fresh interpreter on
// a goroutine, its stdout/stderr streamed to the same callbacks a real process
// would have fed, and an exit code delivered the same way.
func (rt *Runtime) spawnNested(cfg spidermonkey.Config, opts *spidermonkey.Object, argv []string,
	onStdout, onStderr, onExit, onError *spidermonkey.Object,
) (spidermonkey.Value, error) {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	inR, inW := io.Pipe()

	st := rt.child
	st.mu.Lock()
	st.nextNestedPID++
	id := -st.nextNestedPID // negative: not an OS pid, and never collides with one
	w := newConnWriter()
	w.attach(inW)
	go w.run(func(error) {})
	st.stdin[id] = w
	// fork() asks for an IPC channel; spawn() does not. The guest signals it
	// with stdio containing "ipc", exactly as Node's own option does.
	var ipc *ipcChannel
	if wantsIPC(opts) {
		ipc = newIPCChannel()
		st.ipc[id] = ipc
	}
	st.mu.Unlock()

	rt.loop.AddPending("childproc")
	wgOut := rt.pipeToCallback(outR, onStdout)
	wgErr := rt.pipeToCallback(errR, onStderr)

	env := envList(opts, cfg.Env)
	cwd := optString(opts, "cwd")
	go func() {
		res := rt.runNestedIPC(cfg, argv, env, cwd, inR, outW, errW, 0, ipc)
		if ipc != nil {
			ipc.close()
		}
		outW.Close()
		errW.Close()
		wgOut.Wait()
		wgErr.Wait()
		st.mu.Lock()
		sw := st.stdin[id]
		delete(st.stdin, id)
		st.mu.Unlock()
		if sw != nil {
			sw.requestClose()
		}
		rt.loop.Post(func() error {
			if res.err != nil && onError != nil {
				onError.Call(spidermonkey.ValueOf(res.err.Error()))
			}
			if onExit != nil {
				onExit.Call(spidermonkey.ValueOf(res.exitCode), spidermonkey.ValueOf(nil))
			}
			freeObjects(onStdout, onStderr, onExit, onError)
			return nil
		})
		rt.loop.DonePending("childproc")
	}()
	return spidermonkey.ValueOf(map[string]any{"pid": id}), nil
}

// envList reads the spawn options' environment the same way applyCwdEnv does,
// for the nested-interpreter path (which has no exec.Cmd to fill in).
func envList(opts *spidermonkey.Object, defaultEnv []string) []string {
	if envV, _ := opts.Get("envArray"); envV != nil {
		if a := envV.Object(); a != nil {
			defer a.Free()
			lenV, _ := a.Get("length")
			env := make([]string, 0, lenV.Int())
			for i := 0; i < lenV.Int(); i++ {
				iv, _ := a.Get(fmt.Sprint(i))
				env = append(env, iv.String())
			}
			return env
		}
	}
	if defaultEnv == nil {
		return []string{}
	}
	return defaultEnv
}

// optInt reads a numeric option (0 when absent).
func optInt(opts *spidermonkey.Object, name string) int {
	if v, ok := optScalar(opts, name); ok {
		return v.Int()
	}
	return 0
}

func optString(opts *spidermonkey.Object, name string) string {
	if v, ok := optScalar(opts, name); ok {
		return v.String()
	}
	return ""
}

// spawnSyncResult builds the object spawnSync returns.
func (rt *Runtime) spawnSyncResult(stdout, stderr []byte, code int, err error) (spidermonkey.Value, error) {
	outObj, oerr := rt.js.NewObject()
	if oerr != nil {
		return nil, oerr
	}
	outObj.Set("status", spidermonkey.ValueOf(code))
	outObj.Set("signal", spidermonkey.ValueOf(nil))
	outObj.Set("pid", spidermonkey.ValueOf(0))
	if err != nil {
		outObj.Set("error", spidermonkey.ValueOf(err.Error()))
	}
	so, _ := rt.js.NewBytes(stdout)
	se, _ := rt.js.NewBytes(stderr)
	defer so.Free()
	defer se.Free()
	outObj.Set("stdout", so)
	outObj.Set("stderr", se)
	return rt.trackReturn(outObj), nil
}

func applyCwdEnv(cmd *exec.Cmd, opts *spidermonkey.Object, defaultEnv []string) {
	if cwd, ok := optScalar(opts, "cwd"); ok {
		cmd.Dir = cwd.String()
	}
	set := false
	if envV, _ := opts.Get("envArray"); envV != nil {
		if a := envV.Object(); a != nil {
			defer a.Free()
			lenV, _ := a.Get("length")
			env := make([]string, 0, lenV.Int())
			for i := 0; i < lenV.Int(); i++ {
				iv, _ := a.Get(fmt.Sprint(i))
				env = append(env, iv.String())
			}
			cmd.Env = env
			set = true
		}
	}
	// No env option: default to the sandbox's Config.Env, NOT nil — a nil
	// cmd.Env makes os/exec inherit the HOST process environment (which may hold
	// secrets the embedder deliberately kept out of Config.Env). Use a non-nil
	// empty slice so the child gets exactly the configured environment.
	if !set {
		cmd.Env = append([]string(nil), defaultEnv...)
		if cmd.Env == nil {
			cmd.Env = []string{}
		}
	}
}

// exitInfo returns the child's exit code and signal name in Node's shape: a
// normal exit yields (code, nil); a signal death yields (nil, "SIG...") — code
// is null, NOT -1, when the child was signaled; a spawn failure yields (nil,
// nil). code is `any` so a signal death marshals to JS null.
func exitInfo(werr error) (code any, signal any) {
	if werr == nil {
		return 0, nil
	}
	if ee, ok := werr.(*exec.ExitError); ok && ee.ProcessState != nil {
		if c := ee.ProcessState.ExitCode(); c >= 0 {
			return c, nil
		}
		// Killed by a signal: Node reports code=null, signal=name.
		return nil, signalName(ee.ProcessState.String())
	}
	return nil, nil
}

// childSignals maps the signals Node can report to their names. signalName
// matches a ProcessState's textual form (which embeds syscall.Signal.String(),
// e.g. "hangup") against each, staying portable without reaching into the
// platform-specific WaitStatus.
var childSignals = []struct {
	sig  syscall.Signal
	name string
}{
	{syscall.SIGHUP, "SIGHUP"}, {syscall.SIGINT, "SIGINT"}, {syscall.SIGQUIT, "SIGQUIT"},
	{syscall.SIGILL, "SIGILL"}, {syscall.SIGTRAP, "SIGTRAP"}, {syscall.SIGABRT, "SIGABRT"},
	{syscall.SIGBUS, "SIGBUS"}, {syscall.SIGFPE, "SIGFPE"}, {syscall.SIGKILL, "SIGKILL"},
	{syscall.SIGSEGV, "SIGSEGV"}, {syscall.SIGPIPE, "SIGPIPE"}, {syscall.SIGALRM, "SIGALRM"},
	{syscall.SIGTERM, "SIGTERM"}, {syscall.SIGUSR1, "SIGUSR1"}, {syscall.SIGUSR2, "SIGUSR2"},
}

func signalName(state string) any {
	for _, s := range childSignals {
		if strings.Contains(state, s.sig.String()) {
			return s.name
		}
	}
	return nil
}

func childErr(err error) spidermonkey.Value {
	code := "ENOENT"
	if strings.Contains(err.Error(), "permission denied") {
		code = "EACCES"
	} else if strings.Contains(err.Error(), "disabled") {
		code = "EPERM"
	}
	return spidermonkey.ValueOf(map[string]any{"code": code, "message": err.Error()})
}

func (rt *Runtime) closeChild() {
	st := rt.child
	st.mu.Lock()
	procs := make([]*exec.Cmd, 0, len(st.procs))
	for _, c := range st.procs {
		procs = append(procs, c)
	}
	st.procs = map[int64]*exec.Cmd{}
	st.mu.Unlock()
	for _, c := range procs {
		if c.Process != nil {
			// The GROUP, not just the child: a grandchild holding the runner's
			// pipe is what keeps a finished run from returning.
			killProcessGroup(c.Process.Pid)
			c.Process.Kill()
		}
	}
}

// wantsIPC reports whether the caller asked for a message channel: Node
// signals it by putting "ipc" in the stdio array, which is what fork() does.
func wantsIPC(opts *spidermonkey.Object) bool {
	if opts == nil {
		return false
	}
	v, ok := optScalar(opts, "ipc")
	return ok && v.Bool()
}
