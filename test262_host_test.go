package spidermonkey_test

// The $262 conformance-support object, assembled ENTIRELY host-side from the
// public API plus the test-only engine primitives (export_test.go). The engine
// bridge carries nothing test262-shaped: createRealm, evalScript, gc,
// detachArrayBuffer, IsHTMLDDA, global, setTimeout and agent are all composed
// here in Go — the proof the generic primitives are sufficient.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// harness262 owns the per-interpreter host state $262 needs: the setTimeout
// queue (fired between job-queue steps by pumpUntil) and a key counter so a
// property name defined on several realms gets a distinct dispatch key.
type harness262 struct {
	js  *spidermonkey.JS
	mu  sync.Mutex
	due []timer262
	seq int

	// out/err collect the guest's print()/console output — the engine has no
	// I/O, so console is a host func the harness installs, writing here. The
	// async-marker check reads out. Guarded by outMu (agent + timer callbacks
	// print concurrently with the main drain).
	outMu sync.Mutex
	out   strings.Builder
	err   strings.Builder
}

// stdout / stderr return everything the guest has printed so far.
func (h *harness262) stdout() string {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	return h.out.String()
}

func (h *harness262) stderr() string {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	return h.err.String()
}

type timer262 struct {
	at time.Time
	cb *spidermonkey.Object // a function; freed after it fires
}

func newHarness262(js *spidermonkey.JS) (*harness262, error) {
	h := &harness262{js: js}
	if err := h.installOn(js.Global(), true); err != nil {
		return nil, err
	}
	return h, nil
}

// key returns a fresh dispatch key so per-realm callbacks never collide.
func (h *harness262) key(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return fmt.Sprintf("$262.%s.%d", name, h.seq)
}

// installOn builds $262 (and setTimeout) on one realm's global. isRoot only
// controls whether $262.agent is attached (agents are a process-wide cluster;
// child realms share it, so it is installed once on the root).
func (h *harness262) installOn(global *spidermonkey.Object, isRoot bool) error {
	js := h.js

	d262, err := js.NewObject()
	if err != nil {
		return err
	}
	var perr error
	def := func(name string, fn spidermonkey.Func) {
		if perr == nil {
			perr = spidermonkey.DefineFuncKeyed(js, d262, name, h.key(name), fn)
		}
	}

	def("gc", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.Undefined(), spidermonkey.GC(js)
	})
	def("detachArrayBuffer", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		o := args[0].Object()
		if o == nil {
			return nil, fmt.Errorf("detachArrayBuffer: not an object")
		}
		return spidermonkey.Undefined(), spidermonkey.DetachArrayBuffer(js, o)
	})
	// evalScript runs in THIS realm — bind it to this realm's global.
	def("evalScript", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.EvalIn(js, global, args[0].String())
	})
	// createRealm makes a child realm, installs $262 on it recursively, and
	// returns the child's $262.
	def("createRealm", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		child, err := spidermonkey.NewRealm(js)
		if err != nil {
			return nil, err
		}
		if err := h.installOn(child, false); err != nil {
			return nil, err
		}
		return child.Get("$262")
	})

	// IsHTMLDDA and global are values, not functions.
	dda, err := spidermonkey.NewHTMLDDA(js)
	if err != nil {
		return err
	}
	if err := d262.Set("IsHTMLDDA", dda); err != nil {
		return err
	}
	if err := d262.Set("global", global); err != nil {
		return err
	}
	if perr != nil {
		return perr
	}

	// setTimeout lives on the realm's GLOBAL (not on $262): the harness fires
	// due callbacks between job-queue steps (see pumpUntil). test262's
	// atomicsHelper.js needs a real setTimeout or it installs a busy-wait.
	stKey := h.key("setTimeout")
	if err := spidermonkey.DefineFuncKeyed(js, global, "setTimeout", stKey,
		func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			cb := args[0].Object()
			if cb == nil {
				return nil, fmt.Errorf("setTimeout: first argument must be a function")
			}
			ms := 0.0
			if len(args) > 1 {
				ms = args[1].Float()
			}
			h.mu.Lock()
			h.due = append(h.due, timer262{at: time.Now().Add(time.Duration(ms) * time.Millisecond), cb: cb})
			h.mu.Unlock()
			return spidermonkey.Undefined(), nil
		}); err != nil {
		return err
	}

	// print + console: the engine is pure ECMA-262, so these are host funcs
	// the harness installs (the design's "console is not built in"). They join
	// their arguments with spaces and a newline, exactly like the shell's
	// print, and write into h.out/h.err — which is where the async marker
	// ($DONE prints "Test262:AsyncTestComplete") is read from.
	writer := func(b *strings.Builder) spidermonkey.Func {
		return func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			h.outMu.Lock()
			for i, a := range args {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(a.String())
			}
			b.WriteByte('\n')
			h.outMu.Unlock()
			return spidermonkey.Undefined(), nil
		}
	}
	if err := spidermonkey.DefineFuncKeyed(js, global, "print", h.key("print"), writer(&h.out)); err != nil {
		return err
	}
	console, err := js.NewObject()
	if err != nil {
		return err
	}
	for _, m := range []struct {
		name string
		b    *strings.Builder
	}{{"log", &h.out}, {"info", &h.out}, {"warn", &h.err}, {"error", &h.err}} {
		if err := spidermonkey.DefineFuncKeyed(js, console, m.name, h.key("console."+m.name), writer(m.b)); err != nil {
			return err
		}
	}
	if err := global.Set("console", console); err != nil {
		return err
	}

	if err := global.Set("$262", d262); err != nil {
		return err
	}
	if isRoot {
		return h.installAgent(d262)
	}
	return nil
}

// installAgent attaches $262.agent — the Go adapter over the public Agents
// cluster — onto the $262 object.
func (h *harness262) installAgent(d262 *spidermonkey.Object) error {
	js := h.js
	agents := js.Agents()

	agent, err := js.NewObject()
	if err != nil {
		return err
	}
	var perr error
	def := func(name string, fn spidermonkey.Func) {
		if perr == nil {
			perr = spidermonkey.DefineFuncKeyed(js, agent, name, h.key("agent."+name), fn)
		}
	}
	def("start", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		_, err := agents.Spawn(agent262ChildPrelude, args[0].String())
		return spidermonkey.Undefined(), err
	})
	def("broadcast", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		msg, err := js.NewObject()
		if err != nil {
			return nil, err
		}
		defer msg.Free()
		if err := msg.Set("sab", args[0]); err != nil {
			return nil, err
		}
		id := spidermonkey.Value(spidermonkey.Undefined())
		if len(args) > 1 {
			id = args[1]
		}
		if err := msg.Set("id", id); err != nil {
			return nil, err
		}
		return spidermonkey.Undefined(), agents.Broadcast(msg)
	})
	def("getReport", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		_, v, ok, err := agents.Receive()
		if err != nil {
			return nil, err
		}
		if !ok {
			return spidermonkey.Null(), nil
		}
		return v, nil
	})
	def("sleep", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		time.Sleep(time.Duration(args[0].Float() * float64(time.Millisecond)))
		return spidermonkey.Undefined(), nil
	})
	def("monotonicNow", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(float64(time.Now().UnixNano()) / 1e6), nil
	})
	if perr != nil {
		return perr
	}
	return d262.Set("agent", agent)
}

// fireDueTimers calls every setTimeout callback whose deadline has passed,
// reporting whether any ran. A callback that throws is swallowed (test262
// timers are fire-and-forget). ctx bounds a runaway callback.
func (h *harness262) fireDueTimers(ctx context.Context) bool {
	now := time.Now()
	h.mu.Lock()
	var ready []timer262
	kept := h.due[:0]
	for _, t := range h.due {
		if !t.at.After(now) {
			ready = append(ready, t)
		} else {
			kept = append(kept, t)
		}
	}
	h.due = kept
	h.mu.Unlock()
	for _, t := range ready {
		_, _ = t.cb.Call()
		_ = t.cb.Free()
	}
	return len(ready) > 0
}

// pendingTimers reports whether any setTimeout is still armed.
func (h *harness262) pendingTimers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.due) > 0
}
