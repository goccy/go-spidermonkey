package nodejs

// childproc.go: node:child_process over Go os/exec, gated by Config.Exec.
// Async spawns run on a goroutine that posts stdout/stderr chunks and the
// exit onto the event loop; sync spawns block the op and return the result.

import (
	"bytes"
	"fmt"
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
	stdin map[int64]interface{ Write([]byte) (int, error) }
}

func newProcState() *procState {
	return &procState{procs: map[int64]*exec.Cmd{}, stdin: map[int64]interface{ Write([]byte) (int, error) }{}}
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
	file, argv, err := cmdArgv(opts)
	if err != nil {
		return nil, err
	}
	if err := execAllowed(cfg, file, argv); err != nil {
		return childErr(err), nil
	}
	onStdout := args[1].Object()
	onStderr := args[2].Object()
	onExit := args[3].Object()
	onError := args[4].Object()

	cmd := exec.Command(file, argv...)
	applyCwdEnv(cmd, opts)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return childErr(err), nil
	}

	st := rt.child
	st.mu.Lock()
	id := int64(cmd.Process.Pid)
	st.procs[id] = cmd
	if stdin != nil {
		st.stdin[id] = stdin
	}
	st.mu.Unlock()

	rt.loop.AddPending()
	pipe := func(r interface{ Read([]byte) (int, error) }, cb *spidermonkey.Object) *sync.WaitGroup {
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
	wgOut := pipe(stdout, onStdout)
	wgErr := pipe(stderr, onStderr)

	go func() {
		wgOut.Wait()
		wgErr.Wait()
		werr := cmd.Wait()
		code, signal := exitInfo(werr)
		st.mu.Lock()
		delete(st.procs, id)
		delete(st.stdin, id)
		st.mu.Unlock()
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
		rt.loop.DonePending()
	}()

	return spidermonkey.ValueOf(map[string]any{"pid": id}), nil
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
		return spidermonkey.ValueOf(false), nil
	}
	if args[1].IsUndefined() || args[1].Object() == nil && args[1].Export() == nil {
		if c, ok := w.(interface{ Close() error }); ok {
			c.Close()
		}
		st.mu.Lock()
		delete(st.stdin, pid)
		st.mu.Unlock()
		return spidermonkey.ValueOf(true), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	w.Write(data)
	return spidermonkey.ValueOf(true), nil
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
	file, argv, err := cmdArgv(opts)
	if err != nil {
		return nil, err
	}
	if err := execAllowed(cfg, file, argv); err != nil {
		return childErr(err), nil
	}
	cmd := exec.Command(file, argv...)
	applyCwdEnv(cmd, opts)
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
	if werr != nil && code == -1 {
		result["error"] = werr.Error()
	}
	outObj, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	for k, v := range result {
		outObj.Set(k, spidermonkey.ValueOf(v))
	}
	so, _ := rt.js.NewBytes(stdout.Bytes())
	se, _ := rt.js.NewBytes(stderr.Bytes())
	defer so.Free()
	defer se.Free()
	outObj.Set("stdout", so)
	outObj.Set("stderr", se)
	return rt.trackReturn(outObj), nil
}

// applyCwdEnv reads opts.cwd and opts.envArray (the JS side flattens an env
// object into a ["KEY=VALUE", ...] array, undefined when inheriting).
func applyCwdEnv(cmd *exec.Cmd, opts *spidermonkey.Object) {
	if cwd, _ := opts.Get("cwd"); cwd != nil && !cwd.IsUndefined() {
		cmd.Dir = cwd.String()
	}
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
		}
	}
}

func exitInfo(werr error) (code int, signal any) {
	if werr == nil {
		return 0, nil
	}
	if ee, ok := werr.(*exec.ExitError); ok {
		if ee.ProcessState != nil {
			if ee.ProcessState.ExitCode() >= 0 {
				return ee.ProcessState.ExitCode(), nil
			}
			// Killed by signal.
			return -1, signalName(ee.ProcessState.String())
		}
	}
	return -1, nil
}

func signalName(state string) any {
	if strings.Contains(state, "killed") {
		return "SIGKILL"
	}
	if strings.Contains(state, "terminated") {
		return "SIGTERM"
	}
	if strings.Contains(state, "interrupt") {
		return "SIGINT"
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
			c.Process.Kill()
		}
	}
}
