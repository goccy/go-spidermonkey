package spidermonkey_test

// A Web-Worker adapter composed ENTIRELY on the public API — the proof that a
// real `new Worker(src)` class can be added by an embedder in ordinary Go, with
// nothing Worker-shaped in the engine: the agent primitives (js.Agents()) for
// the thread + messaging, and a host constructor (Global().DefineConstructor)
// for the class. It lives in the test package (it is an example/adapter, not
// part of go-spidermonkey's public surface).

import (
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// webWorkers is the main-side manager: it maps each worker agent to its
// instance object so worker->main messages reach the right onmessage, and runs
// a goroutine draining those messages.
type webWorkers struct {
	js      *spidermonkey.JS
	agents  *spidermonkey.Agents
	mu      sync.Mutex
	byID    map[spidermonkey.AgentID]*spidermonkey.Object
	stop    chan struct{}
	stopped sync.WaitGroup
}

// workerGlue is the worker-side bootstrap, evaluated as its OWN script BEFORE
// the user source (never concatenated). It makes self === globalThis (so real
// worker bundles that test `self === globalThis` behave), installs
// postMessage/onmessage/close as true globals, and defines __deliver__ — the
// hook the agent's C++ pump calls with each incoming message, ON the agent
// thread, interleaved with the engine's job-queue draining. So there is NO
// Atomics.waitAsync polling loop here: delivery rides the pump, an async
// onmessage's promises drain in the pump's RunJobs, and nothing churns the
// engine while the worker is idle.
const workerGlue = `(() => {
	const A = globalThis.__agent__;
	globalThis.self = globalThis;      // Web Worker: self IS the global scope
	globalThis.onmessage = null;
	globalThis.postMessage = (v) => A.post(v);
	globalThis.close = () => A.leaving();
	globalThis.__deliver__ = (data) => {
		if (data && data.__terminate__) { A.leaving(); return; }
		if (self.onmessage) self.onmessage({ data });  // may be async; the pump drains it
	};
})();
`

// installWebWorker adds a `new Worker(src)` class to the interpreter's global
// and starts the worker->main message pump. Call close() (before js.Close) to
// stop the pump.
func installWebWorker(js *spidermonkey.JS) (*webWorkers, error) {
	wm := &webWorkers{
		js:     js,
		agents: js.Agents(),
		byID:   map[spidermonkey.AgentID]*spidermonkey.Object{},
		stop:   make(chan struct{}),
	}
	wm.stopped.Add(1)
	go wm.pump()
	// Worker is a constructor: `new Worker(src)` runs construct, whose returned
	// *Object is the instance. The dispatch key must not contain a NUL byte
	// (host-func keys round-trip through a JS string read as a C string).
	if err := js.Global().DefineConstructor("Worker", "WebWorker.construct", wm.construct); err != nil {
		wm.close()
		return nil, err
	}
	return wm, nil
}

func (wm *webWorkers) close() {
	close(wm.stop)
	wm.stopped.Wait()
}

// construct implements `new Worker(src)`: spawn the worker agent, build the
// instance object with postMessage/terminate bound to it, and register it for
// message routing. The returned *Object is what `new` yields.
func (wm *webWorkers) construct(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	src := ""
	if len(args) > 0 {
		src = args[0].String()
	}
	id, err := wm.agents.Spawn(workerGlue, src)
	if err != nil {
		return nil, err
	}
	inst, err := wm.js.NewObject()
	if err != nil {
		return nil, err
	}
	// postMessage(v): main -> this worker.
	if err := inst.DefineFunc("postMessage", func(cfg spidermonkey.Config, a []spidermonkey.Value) (spidermonkey.Value, error) {
		v := spidermonkey.Value(spidermonkey.Undefined())
		if len(a) > 0 {
			v = a[0]
		}
		return spidermonkey.Undefined(), wm.agents.Send(id, v)
	}); err != nil {
		return nil, err
	}
	// terminate(): cooperative — the loop exits at the next message boundary.
	if err := inst.DefineFunc("terminate", func(cfg spidermonkey.Config, a []spidermonkey.Value) (spidermonkey.Value, error) {
		sentinel, err := wm.js.NewObject()
		if err != nil {
			return nil, err
		}
		defer sentinel.Free()
		if err := sentinel.Set("__terminate__", spidermonkey.ValueOf(true)); err != nil {
			return nil, err
		}
		return spidermonkey.Undefined(), wm.agents.Send(id, sentinel)
	}); err != nil {
		return nil, err
	}
	wm.mu.Lock()
	wm.byID[id] = inst
	wm.mu.Unlock()
	return inst, nil
}

// pump drains worker->main messages and dispatches each to its worker
// instance's onmessage, on the main interpreter (serialized with Eval by the
// invoke lock — so onmessage runs when the main thread is between tasks).
func (wm *webWorkers) pump() {
	defer wm.stopped.Done()
	for {
		select {
		case <-wm.stop:
			return
		default:
		}
		from, v, ok, err := wm.agents.Receive()
		if err != nil || !ok {
			select {
			case <-wm.stop:
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		wm.mu.Lock()
		inst := wm.byID[from]
		wm.mu.Unlock()
		if inst != nil {
			wm.dispatch(inst, v)
		}
	}
}

// dispatch calls worker.onmessage({data: v}) if a handler is set.
func (wm *webWorkers) dispatch(inst *spidermonkey.Object, v spidermonkey.Value) {
	handler, err := inst.Get("onmessage")
	if err != nil {
		return
	}
	fn := handler.Object()
	if fn == nil || !fn.IsFunction() {
		return
	}
	defer fn.Free()
	evt, err := wm.js.NewObject()
	if err != nil {
		return
	}
	defer evt.Free()
	if err := evt.Set("data", v); err != nil {
		return
	}
	_, _ = fn.Call(evt)
}
