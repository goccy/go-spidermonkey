package nodejs

// worker.go: node:worker_threads over the engine's agent cluster
// (js.Agents()). Each Worker is a real agent — its own goroutine, its own
// SpiderMonkey realm and linear memory, sharing nothing but SharedArrayBuffer
// with the main interpreter. Messages cross via the structured-clone
// transport. This is genuine preemptive parallelism.
//
// A per-runtime pump goroutine drains worker->main messages and posts each
// onto the main event loop, so a Worker's 'message'/'online'/'exit'/'error'
// handlers run serialized with the rest of the main thread. Live workers hold
// the loop open (AddPending) until they post their exit.

import (
	_ "embed"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

//go:embed js/worker.js
var workerJS string

// hasUseStrictPrologue reports whether the worker source opens with a
// "use strict" directive (after optional leading whitespace).
func hasUseStrictPrologue(src string) bool {
	s := strings.TrimLeft(src, " \t\r\n")
	return strings.HasPrefix(s, `"use strict"`) || strings.HasPrefix(s, `'use strict'`)
}

// wrapWorkerSource wraps the user source so a top-level throw is reported to the
// main thread (Worker 'error' then exit 1), while preserving the source's own
// semantics: a leading shebang is stripped (Node does this), a leading
// "use strict" directive is hoisted so the source stays strict (a directive
// inside the try block would be inert), and `try {` is glued to the source with
// no intervening newline so stack-trace line numbers are unchanged.
//
// After the try/catch, an idle check is scheduled: a worker whose top-level
// script finishes NORMALLY (no process.exit/parentPort.close, no live timers,
// no 'message' listener) auto-exits with code 0, matching Node — otherwise the
// agent goroutine would park on its inbox forever and the loop's AddPending
// would never be released. The check is a no-op when the source already exited
// (a throw sets `exiting` via __wt_reportError, so the throw path still reports
// then exits 1) and only ever schedules a macrotask probe, never a sync exit.
func wrapWorkerSource(source string) string {
	src := source
	if strings.HasPrefix(src, "#!") {
		if i := strings.IndexByte(src, '\n'); i >= 0 {
			src = src[i+1:]
		} else {
			src = ""
		}
	}
	prefix := ""
	if hasUseStrictPrologue(src) {
		prefix = `"use strict"; `
	}
	return prefix + "try {" + src + "\n} catch (__wt_e) { __wt_reportError(__wt_e); }\n;globalThis.__wt_idleCheck && globalThis.__wt_idleCheck();"
}

type workerManager struct {
	rt       *Runtime
	agents   *spidermonkey.Agents
	stdout   io.Writer
	stderr   io.Writer
	mu       sync.Mutex
	nextTID  int
	insts    map[spidermonkey.AgentID]*spidermonkey.Object // worker id -> JS Worker instance
	deadSeen map[spidermonkey.AgentID]int
	reaping  map[spidermonkey.AgentID]bool // a crash-exit reap has been posted
	stop     chan struct{}
	pumpDone chan struct{} // closed when the pump goroutine exits
	started  bool
}

func newWorkerManager(rt *Runtime) *workerManager {
	return &workerManager{
		rt:       rt,
		agents:   rt.js.Agents(),
		insts:    map[spidermonkey.AgentID]*spidermonkey.Object{},
		deadSeen: map[spidermonkey.AgentID]int{},
		reaping:  map[spidermonkey.AgentID]bool{},
		stop:     make(chan struct{}),
		pumpDone: make(chan struct{}),
		nextTID:  1,
	}
}

func (rt *Runtime) workerOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"worker_spawn":     rt.opWorkerSpawn,
		"worker_post":      rt.opWorkerPost,
		"worker_terminate": rt.opWorkerTerminate,
	}
}

// opWorkerSpawn(source, workerData, instance) -> threadId. source is the
// worker script (main reads a filename into source; eval code passes through).
// instance is the JS Worker object the pump routes messages to. workerData is
// handed to the worker via the first inbox message (structured-cloned).
func (rt *Runtime) opWorkerSpawn(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("worker_spawn: (source, workerData, instance) required")
	}
	source := args[0].String()
	workerData := args[1]
	// workerData is GC-pinned at decode; it's only used to populate the init
	// object (which Send clones), so free it on every return path.
	freeWorkerData := func() { freeObjects(workerData.Object()) }
	inst := args[2].Object()
	if inst == nil {
		freeWorkerData()
		return nil, fmt.Errorf("worker_spawn: instance must be an object")
	}

	wm := rt.workers
	wm.mu.Lock()
	tid := wm.nextTID
	wm.nextTID++
	// Capture the config stdio for worker console forwarding.
	wm.stdout, wm.stderr = cfg.Stdout, cfg.Stderr
	if !wm.started {
		wm.started = true
		go wm.pump()
	}
	wm.mu.Unlock()

	// Wrap the user source so a top-level throw is reported to the main thread
	// as the Worker's 'error' event (then exit 1) instead of silently killing
	// the agent with only a bare exit. The wrapper preserves strict mode,
	// strips a shebang, and keeps line numbers (see wrapWorkerSource).
	id, err := wm.agents.Spawn(workerJS, wrapWorkerSource(source))
	if err != nil {
		inst.Free()
		freeWorkerData()
		return nil, err
	}

	// The workerData handshake: the worker's glue blocks in A.recv() for this.
	initObj, err := rt.js.NewObject()
	if err != nil {
		inst.Free()
		freeWorkerData()
		// The agent is spawned and blocked in its init recv(); it's reaped when
		// the cluster closes (NewObject only fails under host OOM).
		return nil, err
	}
	initObj.Set("__wt_init", spidermonkey.ValueOf(true))
	initObj.Set("threadId", spidermonkey.ValueOf(tid))
	if workerData != nil && !workerData.IsUndefined() {
		initObj.Set("workerData", workerData)
	}
	// Register the instance BEFORE the handshake Send: the worker can post a
	// message the instant it returns from recv(init), and the pump drops (and
	// leaks the handle of) any message for an unregistered id.
	// The instance *Object arg stays valid after this op returns (its handle
	// pins the object), so the pump keeps it for routing; freed at exit.
	wm.mu.Lock()
	wm.insts[id] = inst
	wm.mu.Unlock()
	if err := wm.agents.Send(id, initObj); err != nil {
		initObj.Free()
		wm.mu.Lock()
		delete(wm.insts, id)
		wm.mu.Unlock()
		inst.Free()
		freeWorkerData()
		// Release the agent (blocked in its init recv) with a terminate sentinel
		// that has no workerData, so it clones cleanly even though the full init
		// (which carried the un-cloneable workerData) did not.
		if term, terr := rt.js.NewObject(); terr == nil {
			term.Set("__terminate__", spidermonkey.ValueOf(true))
			_ = wm.agents.Send(id, term)
			term.Free()
		}
		return nil, err
	}
	initObj.Free()
	freeWorkerData() // cloned into initObj by Send; the arg pin is no longer needed

	rt.loop.AddPending()
	rt.loop.Post(func() error { wm.emit(inst, "online", spidermonkey.Undefined()); return nil })

	return spidermonkey.ValueOf(map[string]any{"id": float64(id), "threadId": tid}), nil
}

func (rt *Runtime) opWorkerPost(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("worker_post: (id, value) required")
	}
	id := spidermonkey.AgentID(args[0].Float())
	wrap, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	defer wrap.Free()
	wrap.Set("__wt_msg", spidermonkey.ValueOf(true))
	wrap.Set("data", args[1])
	err = rt.workers.agents.Send(id, wrap)
	// Send cloned wrap (and the data it references); the data arg's decode-time
	// pin is no longer needed, so free it or every postMessage leaks a handle.
	freeObjects(args[1].Object())
	return spidermonkey.Undefined(), err
}

// opWorkerTerminate requests a worker stop by sending a cooperative
// __terminate__ sentinel to its inbox; the agent acts on it between job-queue
// drains. KNOWN LIMITATION: this cannot stop a worker stuck in an infinite
// SYNCHRONOUS loop (e.g. new Worker('while(true){}', {eval:true})) — it never
// drains its inbox, so it never sees the sentinel. Forcibly interrupting such a
// worker needs a per-agent engine interrupt (a js_agent_interrupt primitive in
// the spidermonkey-wasm C++/js.cc that trips that agent's JSContext
// interruptBits_); the host ctx-interrupt only targets the main JSContext.
// Tracked as a separate engine-layer follow-up.
func (rt *Runtime) opWorkerTerminate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	id := spidermonkey.AgentID(args[0].Float())
	sentinel, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	defer sentinel.Free()
	sentinel.Set("__terminate__", spidermonkey.ValueOf(true))
	return spidermonkey.Undefined(), rt.workers.agents.Send(id, sentinel)
}

// pump drains worker->main messages and posts each onto the main loop. When
// no message is waiting it also reaps agents that ended without posting a
// farewell (an uncaught error during evaluation), firing 'exit' with code 1.
func (wm *workerManager) pump() {
	defer close(wm.pumpDone)
	for {
		select {
		case <-wm.stop:
			return
		default:
		}
		from, v, ok, err := wm.agents.Receive()
		if err == nil && ok {
			msg := v
			wm.mu.Lock()
			inst := wm.insts[from]
			wm.mu.Unlock()
			if inst != nil {
				wm.rt.loop.Post(func() error { wm.dispatch(from, inst, msg); return nil })
			}
			continue
		}
		// No message waiting: reap any crashed workers, then back off briefly.
		wm.reapDead()
		select {
		case <-wm.stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// reapDead fires a crash exit(1) for any tracked worker whose agent has ended
// without a clean __wt_exit (which removes the inst via dispatch). A clean
// process.exit(0) posts __wt_exit BEFORE leaving, so the message is already in
// the queue when the agent dies — but the pump may observe "dead" before it
// has Received that message. To avoid racing a clean exit into a crash exit,
// an agent must be seen dead for TWO consecutive empty-queue cycles (a Receive
// drain happens between them, so any queued __wt_exit is dispatched first).
func (wm *workerManager) reapDead() {
	wm.mu.Lock()
	var reap []spidermonkey.AgentID
	for id := range wm.insts {
		if wm.agents.IsAlive(id) {
			delete(wm.deadSeen, id)
			continue
		}
		wm.deadSeen[id]++
		if wm.deadSeen[id] >= 2 && !wm.reaping[id] {
			wm.reaping[id] = true
			reap = append(reap, id)
		}
	}
	wm.mu.Unlock()

	// The crash-exit is delivered via a POST so it runs AFTER any __wt_exit
	// message the pump already posted for this agent. On the loop, if the inst
	// is still present the clean exit never handled it — so it crashed
	// (exit 1); if a clean __wt_exit already removed it, this is a no-op. This
	// ordering is what keeps a clean exit(0) from racing into a bogus exit(1)
	// when the loop is busy during the reap window.
	for _, id := range reap {
		idCopy := id
		wm.rt.loop.Post(func() error {
			wm.mu.Lock()
			inst := wm.insts[idCopy]
			delete(wm.insts, idCopy)
			delete(wm.deadSeen, idCopy)
			delete(wm.reaping, idCopy)
			wm.mu.Unlock()
			if inst != nil {
				wm.emit(inst, "exit", spidermonkey.ValueOf(1))
				inst.Free()
				wm.rt.loop.DonePending()
			}
			return nil
		})
	}
}

// dispatch routes one worker->main message on the loop goroutine.
func (wm *workerManager) dispatch(from spidermonkey.AgentID, inst *spidermonkey.Object, v spidermonkey.Value) {
	o := v.Object()
	if o == nil {
		wm.emit(inst, "message", v)
		return
	}
	defer o.Free()

	if con, _ := o.Get("__wt_console"); con != nil && con.IsObject() {
		c := con.Object()
		defer c.Free()
		// optScalar frees an object-typed level/text (a worker can forge a
		// __wt_console post with object fields; reading them as scalars and
		// dropping the root would pin them on the main interpreter for life).
		out := wm.stdout
		if level, ok := optScalar(c, "level"); ok && level.Int() != 0 {
			out = wm.stderr
		}
		text := ""
		if tv, ok := optScalar(c, "text"); ok {
			text = tv.String()
		}
		if out != nil {
			fmt.Fprintln(out, text)
		}
		return
	}
	if ex, _ := o.Get("__wt_exit"); ex != nil && !ex.IsUndefined() {
		code := ex
		// A forged object exit code is a fresh root; free it and report 0.
		if eo := ex.Object(); eo != nil {
			eo.Free()
			code = spidermonkey.ValueOf(float64(0))
		}
		wm.mu.Lock()
		still := wm.insts[from] == inst
		delete(wm.insts, from)
		// The reaper may have already recorded this agent as dead-and-present for
		// one cycle before this clean-exit dispatch ran; clear those entries too or
		// they leak one map entry per cleanly-exited worker for the process life.
		delete(wm.deadSeen, from)
		delete(wm.reaping, from)
		wm.mu.Unlock()
		if still {
			wm.emit(inst, "exit", code)
			inst.Free()
			wm.rt.loop.DonePending()
		}
		return
	}
	if errmsg, _ := o.Get("__wt_error"); errmsg != nil && !errmsg.IsUndefined() {
		wm.emit(inst, "error", errmsg)
		// An object-valued Get is a fresh persistent root; free it after emit or a
		// worker posting objects leaks one host handle per message.
		if e := errmsg.Object(); e != nil {
			e.Free()
		}
		return
	}
	if flag, _ := o.Get("__wt_msg"); flag != nil && flag.Bool() {
		data, _ := o.Get("data")
		wm.emit(inst, "message", data)
		if d := data.Object(); d != nil {
			d.Free()
		}
		return
	}
	wm.emit(inst, "message", v)
}

// emit invokes the instance's _emit(type, value) glue method.
func (wm *workerManager) emit(inst *spidermonkey.Object, event string, v spidermonkey.Value) {
	_, _ = inst.CallMethod("_emit", spidermonkey.ValueOf(event), v)
}

func (wm *workerManager) close() {
	wm.mu.Lock()
	started := wm.started
	wm.mu.Unlock()
	select {
	case <-wm.stop:
	default:
		close(wm.stop)
	}
	// Join the pump BEFORE the caller tears down the interpreter: the pump makes
	// engine calls (agents.Receive → clone read/decode) off the loop goroutine,
	// so letting js.Close() run while it's mid-Receive risks a use-after-free.
	if started {
		select {
		case <-wm.pumpDone:
		case <-time.After(2 * time.Second):
		}
	}
	wm.mu.Lock()
	for id, inst := range wm.insts {
		delete(wm.insts, id)
		inst.Free()
	}
	wm.mu.Unlock()
}
