package web

// worker.go: the Go half of Worker
// (https://html.spec.whatwg.org/multipage/workers.html).
//
// A Worker is a real thread: the engine's agent facility gives it its own
// SpiderMonkey realm and its own linear memory, sharing nothing with this
// interpreter but SharedArrayBuffer memory, and messages cross through the
// agent structured-clone transport. That is exactly the model the standard
// describes, so nothing here has to pretend.
//
// What the worker realm does NOT have is host functions: an agent's global
// carries one host-provided object and no way to reach a registered host
// function at all (the reasons, and the two ways the engine could close it, are
// docs/engine-followups.md item 11). So the worker environment is pure
// JavaScript — js/workerglue.js — and the surface it can offer is bounded by
// what pure JavaScript can implement. The consequence worth naming: no fetch,
// no crypto.subtle, no URL parser inside a worker, because those are Go here.
//
// Where a worker needs a result only the Go side can compute, it is computed
// ONCE at spawn and handed over in the init message. location is the example:
// the components come from the real URL parser rather than from a second,
// approximate one written in the guest.

import (
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
)

type workerAPI struct {
	js   *spidermonkey.JS
	loop *eventloop.Loop
	// console writes a worker's console output to the parent's streams: a thread
	// with nowhere to write is a thread whose diagnostics are lost. The streams
	// come from the interpreter's Config, which is visible only inside a host op —
	// captured in opSpawn, which necessarily runs before any worker can speak.
	console func(level int, text string, out, errOut io.Writer)
	mu2     sync.Mutex
	stdout  io.Writer
	stderr  io.Writer

	mu       sync.Mutex
	insts    map[spidermonkey.AgentID]*spidermonkey.Object
	deadSeen map[spidermonkey.AgentID]int
	reaping  map[spidermonkey.AgentID]bool
	started  bool
	stop     chan struct{}
	pumpDone chan struct{}
	nextID   float64
}

func installWorker(js *spidermonkey.JS, loop *eventloop.Loop, console func(int, string, io.Writer, io.Writer)) (*workerAPI, error) {
	a := &workerAPI{
		js: js, loop: loop, console: console,
		insts:    map[spidermonkey.AgentID]*spidermonkey.Object{},
		deadSeen: map[spidermonkey.AgentID]int{},
		reaping:  map[spidermonkey.AgentID]bool{},
		stop:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	for name, fn := range map[string]spidermonkey.Func{
		"__worker_spawn":     a.opSpawn,
		"__worker_post":      a.opPost,
		"__worker_terminate": a.opTerminate,
	} {
		if err := js.Global().DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// __worker_spawn(source, scriptURL, instance) starts a worker and returns its
// agent id. The SOURCE is already text: the guest fetched it (a module worker)
// or the host did (a classic one) — either way the constructor must not block,
// so loading happens before this op is reached.
func (a *workerAPI) opSpawn(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("__worker_spawn: (source, url, instance) required")
	}
	source := args[0].String()
	scriptURL := args[1].String()
	inst := args[2].Object()
	if inst == nil {
		return nil, fmt.Errorf("__worker_spawn: instance must be an object")
	}

	a.mu2.Lock()
	a.stdout, a.stderr = cfg.Stdout, cfg.Stderr
	a.mu2.Unlock()

	a.mu.Lock()
	if !a.started {
		a.started = true
		go a.pump()
	}
	a.nextID++
	name := fmt.Sprintf("worker-%d", int64(a.nextID))
	a.mu.Unlock()

	id, err := a.js.Agents().Spawn(workerGlueJS, wrapWorkerSource(source))
	if err != nil {
		inst.Free()
		return nil, err
	}

	// The init handshake: the glue blocks in its first receive for this. It
	// carries the location COMPONENTS, parsed here by the real parser — the
	// worker has no URL of its own to parse them with.
	init, err := a.js.NewObject()
	if err != nil {
		inst.Free()
		return nil, err
	}
	init.Set("__web_worker_init", spidermonkey.ValueOf(true))
	init.Set("name", spidermonkey.ValueOf(name))
	if loc, lerr := locationParts(scriptURL); lerr == nil {
		init.Set("location", spidermonkey.ValueOf(loc))
	}
	a.mu.Lock()
	a.insts[id] = inst
	a.mu.Unlock()
	if err := a.js.Agents().Send(id, init); err != nil {
		init.Free()
		a.mu.Lock()
		delete(a.insts, id)
		a.mu.Unlock()
		inst.Free()
		return nil, err
	}
	init.Free()

	a.loop.AddPending("worker")
	return spidermonkey.ValueOf(float64(id)), nil
}

// locationParts is the WorkerLocation a worker reports, parsed host-side.
func locationParts(rawURL string) (map[string]any, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	origin := ""
	if u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	search := ""
	if u.RawQuery != "" {
		search = "?" + u.RawQuery
	}
	hash := ""
	if u.Fragment != "" {
		hash = "#" + u.Fragment
	}
	return map[string]any{
		"href": u.String(), "origin": origin, "protocol": u.Scheme + ":",
		"host": u.Host, "hostname": u.Hostname(), "port": u.Port(),
		"pathname": u.Path, "search": search, "hash": hash,
	}, nil
}

// wrapWorkerSource runs the worker's own code inside a try so a top-level throw
// reaches the parent as the Worker's 'error' event instead of killing the agent
// with nothing said. The wrapper preserves a "use strict" prologue (a directive
// inside a block is inert) and does not add lines, so reported line numbers stay
// the author's.
func wrapWorkerSource(source string) string {
	prologue := ""
	rest := source
	if trimmed := trimLeadingSpace(source); len(trimmed) >= 12 &&
		(trimmed[:12] == `"use strict"` || trimmed[:12] == "'use strict'") {
		prologue = trimmed[:12] + ";"
		rest = trimmed[12:]
		if len(rest) > 0 && rest[0] == ';' {
			rest = rest[1:]
		}
	}
	// The loop starts after the source has run: from then on the worker IS its
	// messages and its timers.
	return prologue + "try {" + rest +
		"\n} catch (e) { __web_worker_fatal(e); }\n__web_worker_loop();"
}

func trimLeadingSpace(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
		default:
			return s[i:]
		}
	}
	return ""
}

// __worker_post(id, value) sends a message to a worker.
func (a *workerAPI) opPost(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("__worker_post: (id, value) required")
	}
	id := spidermonkey.AgentID(args[0].Float())
	wrap, err := a.js.NewObject()
	if err != nil {
		return nil, err
	}
	defer wrap.Free()
	wrap.Set("__web_worker_msg", spidermonkey.ValueOf(true))
	wrap.Set("data", args[1])
	if err := a.js.Agents().Send(id, wrap); err != nil {
		// A message to a worker that has gone is dropped, as it is in a browser:
		// postMessage does not report delivery.
		freeValue(args[1])
		return spidermonkey.Undefined(), nil
	}
	freeValue(args[1])
	return spidermonkey.Undefined(), nil
}

// __worker_terminate(id) ends a worker. Both halves are needed and neither is
// enough: the sentinel releases a worker parked in a host receive, and the
// interrupt unwinds one that is executing JavaScript.
func (a *workerAPI) opTerminate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	id := spidermonkey.AgentID(args[0].Float())
	if term, err := a.js.NewObject(); err == nil {
		term.Set("__web_worker_terminate", spidermonkey.ValueOf(true))
		_ = a.js.Agents().Send(id, term)
		term.Free()
	}
	_, _ = a.js.Agents().Interrupt(id)
	return spidermonkey.Undefined(), nil
}

// pump moves worker->parent messages onto the loop, and notices a worker that
// died without saying so.
func (a *workerAPI) pump() {
	defer close(a.pumpDone)
	for {
		select {
		case <-a.stop:
			return
		default:
		}
		from, v, ok, err := a.js.Agents().Receive()
		if err == nil && ok {
			msg := v
			a.mu.Lock()
			inst := a.insts[from]
			a.mu.Unlock()
			if inst != nil {
				a.loop.Post(func() error { a.dispatch(from, inst, msg); return nil })
			}
			continue
		}
		a.reapDead()
		select {
		case <-a.stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// reapDead reports a worker whose agent ended without a clean exit. A worker is
// only reaped after being seen dead on TWO consecutive empty-queue cycles, with
// a Receive drain in between, so a clean exit already in the queue is dispatched
// first and never raced into a crash report.
func (a *workerAPI) reapDead() {
	a.mu.Lock()
	var reap []spidermonkey.AgentID
	for id := range a.insts {
		if a.js.Agents().IsAlive(id) {
			delete(a.deadSeen, id)
			continue
		}
		a.deadSeen[id]++
		if a.deadSeen[id] >= 2 && !a.reaping[id] {
			a.reaping[id] = true
			reap = append(reap, id)
		}
	}
	a.mu.Unlock()
	for _, id := range reap {
		agentID := id
		a.loop.Post(func() error {
			a.mu.Lock()
			inst := a.insts[agentID]
			delete(a.insts, agentID)
			delete(a.deadSeen, agentID)
			delete(a.reaping, agentID)
			a.mu.Unlock()
			if inst != nil {
				a.emit(inst, "close", spidermonkey.Undefined())
				inst.Free()
				a.loop.DonePending("worker")
			}
			return nil
		})
	}
}

func (a *workerAPI) dispatch(from spidermonkey.AgentID, inst *spidermonkey.Object, v spidermonkey.Value) {
	o := v.Object()
	if o == nil {
		a.emit(inst, "message", v)
		return
	}
	defer o.Free()

	// A worker's console output belongs on the parent's streams: a thread with
	// nowhere to write is a thread whose diagnostics are lost.
	if con, _ := o.Get("__web_worker_console"); con != nil && con.IsObject() {
		c := con.Object()
		defer c.Free()
		text := ""
		if tv, _ := c.Get("text"); tv != nil {
			if to := tv.Object(); to != nil {
				to.Free()
			} else {
				text = tv.String()
			}
		}
		level := 0
		if lv, _ := c.Get("level"); lv != nil {
			if lo := lv.Object(); lo != nil {
				lo.Free()
			} else {
				level = lv.Int()
			}
		}
		a.writeConsole(level, text)
		return
	}
	if errv, _ := o.Get("__web_worker_error"); errv != nil && !errv.IsUndefined() {
		a.emit(inst, "error", errv)
		freeValue(errv)
		return
	}
	if flag, _ := o.Get("__web_worker_msg"); flag != nil && flag.Bool() {
		data, _ := o.Get("data")
		a.emit(inst, "message", data)
		freeValue(data)
		return
	}
	if flag, _ := o.Get("__web_worker_exit"); flag != nil && flag.Bool() {
		a.mu.Lock()
		still := a.insts[from] == inst
		delete(a.insts, from)
		delete(a.deadSeen, from)
		delete(a.reaping, from)
		a.mu.Unlock()
		if still {
			a.emit(inst, "close", spidermonkey.Undefined())
			inst.Free()
			a.loop.DonePending("worker")
		}
		return
	}
	a.emit(inst, "message", v)
}

func (a *workerAPI) writeConsole(level int, text string) {
	if a.console == nil {
		return
	}
	a.mu2.Lock()
	out, errOut := a.stdout, a.stderr
	a.mu2.Unlock()
	a.console(level, text, out, errOut)
}

func (a *workerAPI) emit(inst *spidermonkey.Object, event string, v spidermonkey.Value) {
	_, _ = inst.CallMethod("_emit", spidermonkey.ValueOf(event), v)
}

// closeAll terminates every worker and stops the pump. Called from Web.Close
// and between pooled requests: a worker must not outlive the instance that
// started it, and its posts must not land on the next request's loop.
func (a *workerAPI) closeAll() {
	a.mu.Lock()
	ids := make([]spidermonkey.AgentID, 0, len(a.insts))
	for id := range a.insts {
		ids = append(ids, id)
	}
	started := a.started
	a.mu.Unlock()
	for _, id := range ids {
		if term, err := a.js.NewObject(); err == nil {
			term.Set("__web_worker_terminate", spidermonkey.ValueOf(true))
			_ = a.js.Agents().Send(id, term)
			term.Free()
		}
		_, _ = a.js.Agents().Interrupt(id)
	}
	if started {
		a.mu.Lock()
		if a.started {
			a.started = false
			close(a.stop)
		}
		a.mu.Unlock()
		<-a.pumpDone
	}
	a.mu.Lock()
	for id, inst := range a.insts {
		inst.Free()
		delete(a.insts, id)
		a.loop.DonePending("worker")
	}
	a.mu.Unlock()
}
