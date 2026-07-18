package spidermonkey

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

// AgentID identifies one spawned agent within a cluster.
type AgentID uint64

// Agents is the interpreter's agent cluster — the host surface over ECMA-262
// agents. The spec defines what an agent IS (its own thread of execution and
// realm, sharing nothing with other agents but SharedArrayBuffer memory) and
// leaves creation and communication to the host; this type IS that host
// policy, implemented in Go. The engine bridge contributes only the thread
// mechanics (Spawn) and structured-clone transport; the queues, the broadcast
// latch and the lifecycle tracking all live here, so richer topologies
// ($262.agent, Web Workers, worker_threads with per-agent channels) are
// adapters over this type rather than engine changes.
//
// Two delivery models, both over the same structured-clone transport:
//   - Broadcast(v) latches ONE value that every agent's broadcast receive
//     gets (test262's $262.agent). A SharedArrayBuffer in v SHARES its memory
//     with each receiver; everything else is a deep copy.
//   - Send(to, v) delivers v to ONE agent's FIFO inbox (postMessage to a
//     specific worker). The agent reads it with its inbox receive.
//
// Either direction back to the host is an agent's post(v), popped with
// Receive() — which reports the sender, so a Worker's onmessage can route.
type Agents struct {
	js *JS

	mu        sync.Mutex
	cond      *sync.Cond // signalled on Broadcast, Send, exit, and close
	closed    bool
	broadcast uint64              // latched clone handle; 0 = none yet
	inboxes   map[uint64][]uint64 // agent id -> its FIFO of clone handles (Send)
	posts     []postedMessage     // agent -> host queue, with sender, in order
	orphaned  []uint64            // clones for exited agents, freed by close()
	alive     map[uint64]bool     // spawned and not yet exited
	exited    map[uint64]bool     // exit events that raced ahead of Spawn's return
}

// postedMessage is one agent->host message plus the id of the agent that sent
// it, so Receive can tell an adapter which worker to route it to.
type postedMessage struct {
	from  uint64
	clone uint64
}

// Agents returns the interpreter's agent cluster.
func (js *JS) Agents() *Agents {
	js.agentsOnce.Do(func() {
		a := &Agents{
			js:      js,
			inboxes: map[uint64][]uint64{},
			alive:   map[uint64]bool{},
			exited:  map[uint64]bool{},
		}
		a.cond = sync.NewCond(&a.mu)
		js.agents = a
	})
	return js.agents
}

// agentPrelude composes the __agent__ surface IN JS from the raw native
// channels the engine installs on an agent's global (__agent_call__,
// __clone_read__, __clone_write__, __agent_leaving__), then deletes those
// from the global so the spawned source only ever sees __agent__. The
// engine ships no agent API of its own — this string IS the agent API, and
// a Worker-style adapter would simply be a different prelude.
const agentPrelude = `(() => {
	const call = globalThis.__agent_call__;
	const read = globalThis.__clone_read__;
	const write = globalThis.__clone_write__;
	const leave = globalThis.__agent_leaving__;
	delete globalThis.__agent_call__;
	delete globalThis.__clone_read__;
	delete globalThis.__clone_write__;
	delete globalThis.__agent_leaving__;
	const ok = (r) => {
		if (r[0] !== "R") { throw new Error(r.slice(1)); }
		return r.slice(1);
	};
	const NO_MSG = Symbol("no-message");
	globalThis.__agent__ = {
		receive: (cb) => cb(read(Number(ok(call("agent-receive"))))),
		recv: () => read(Number(ok(call("agent-inbox")))),
		tryRecv: () => { const r = ok(call("agent-try-inbox")); return r === "" ? NO_MSG : read(Number(r)); },
		NO_MSG,
		post: (v) => { ok(call("agent-post", write(v))); },
		sleep: (ms) => { ok(call("agent-sleep", ms)); },
		leaving: () => leave(),
		monotonicNow: () => Number(ok(call("agent-now"))),
	};
})();
`

// Spawn runs an agent on a new thread (a goroutine), its own realm, sharing
// only SharedArrayBuffer memory with this interpreter. `glue` is trusted
// adapter setup (evaluated with the agent-primitive prelude, which exposes
// globalThis.__agent__ = { receive(cb) (blocking broadcast), recv() (blocking
// inbox), tryRecv()/NO_MSG (non-blocking inbox poll), post(v), sleep(ms),
// leaving(), monotonicNow() }); `src` is the user source, evaluated as its OWN
// separate script so its strict directive, line numbers and module-ness are
// untouched. After both evaluate, the agent drains its job queue until
// leaving() (or Close). Returns the agent's id, so the host can Send to it and
// match its posts.
func (a *Agents) Spawn(glue, src string) (AgentID, error) {
	// The agent-primitive prelude plus the adapter's glue are trusted setup,
	// evaluated together as ONE script; the user src is a SEPARATE script (so
	// its "use strict"/line numbers/module-ness are its own).
	id, err := a.js.raw.AgentSpawn(agentPrelude+glue, src)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	if a.exited[id] {
		delete(a.exited, id) // the agent finished before Spawn returned
	} else {
		a.alive[id] = true
	}
	a.mu.Unlock()
	return AgentID(id), nil
}

// Send delivers v to one agent's FIFO inbox (postMessage to a specific worker)
// and wakes it if it is blocked in its inbox receive. A SharedArrayBuffer in v
// shares its memory with the agent; everything else is a deep copy.
func (a *Agents) Send(to AgentID, v Value) error {
	encoded, err := encodeValue(v)
	if err != nil {
		return err
	}
	clone, err := a.js.raw.CloneWrite(encoded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.inboxes[uint64(to)] = append(a.inboxes[uint64(to)], clone)
	a.mu.Unlock()
	a.cond.Broadcast()       // wake an agent blocked in a blocking inbox receive
	_ = a.js.raw.AgentWake() // wake an agent parked in its pump (message delivery)
	return nil
}

// Broadcast latches v as THE broadcast and wakes every agent blocked in
// receive. Agents that call receive later get the same value. A previously
// latched value is superseded (its clone is retained until Close, since a
// slow receiver may still be reading it).
func (a *Agents) Broadcast(v Value) error {
	encoded, err := encodeValue(v)
	if err != nil {
		return err
	}
	clone, err := a.js.raw.CloneWrite(encoded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.broadcast = clone
	a.mu.Unlock()
	a.cond.Broadcast()
	return nil
}

// Receive pops the next value an agent posted, with the id of the sender (so a
// Worker adapter can route it to the right onmessage). ok is false when the
// queue is empty. The value is deserialized into this interpreter's runtime: an
// object arrives as an *Object, a SharedArrayBuffer still shares its memory.
func (a *Agents) Receive() (from AgentID, v Value, ok bool, err error) {
	a.mu.Lock()
	if len(a.posts) == 0 {
		a.mu.Unlock()
		return 0, nil, false, nil
	}
	msg := a.posts[0]
	a.posts = a.posts[1:]
	a.mu.Unlock()

	encoded, err := a.js.raw.CloneRead(msg.clone)
	if err != nil {
		return 0, nil, false, err
	}
	if ferr := a.js.raw.CloneFree(msg.clone); ferr != nil && err == nil {
		err = ferr
	}
	val, derr := decodeValue(a.js, encoded)
	if derr != nil {
		return 0, nil, false, derr
	}
	return AgentID(msg.from), val, true, err
}

// Alive reports how many spawned agents have not yet exited.
func (a *Agents) Alive() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.alive)
}

// close releases every agent blocked in receive or inbox (they unwind with an
// error) so the engine's shutdown join can complete, then frees every clone
// handle still owned by the cluster. Called by JS.Close (on the main goroutine)
// before the runtime is destroyed, so CloneFree is safe here.
func (a *Agents) close() {
	a.mu.Lock()
	a.closed = true
	// Collect every clone the cluster still owns to free after waking agents.
	var free []uint64
	free = append(free, a.orphaned...)
	if a.broadcast != 0 {
		free = append(free, a.broadcast)
	}
	for _, box := range a.inboxes {
		free = append(free, box...)
	}
	for _, m := range a.posts {
		free = append(free, m.clone)
	}
	a.orphaned, a.broadcast, a.inboxes, a.posts = nil, 0, map[uint64][]uint64{}, nil
	a.mu.Unlock()
	a.cond.Broadcast() // wake agents blocked in receive/inbox

	for _, clone := range free {
		_ = a.js.raw.CloneFree(clone)
	}
}

// ---- reserved-key handlers (called from hostEnv.dispatch) ------------------
// These run on AGENT goroutines, concurrently with main-thread work: they only
// touch cluster state under a.mu and never call back into the interpreter.

// handleReceive blocks the calling agent's goroutine until a broadcast is
// latched (or the cluster closes), then replies with the clone handle. The
// agent deserializes it on its own thread.
func (a *Agents) handleReceive(id uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	for !a.closed && a.broadcast == 0 {
		a.cond.Wait()
	}
	if a.closed {
		return []byte("Ereceive: interpreter shutting down")
	}
	return []byte("R" + strconv.FormatUint(a.broadcast, 10))
}

// handleInbox blocks the calling agent's goroutine until its own inbox has a
// message (or the cluster closes), then pops and replies with the clone handle.
// The agent OWNS the handle from here (it frees it after __clone_read__).
func (a *Agents) handleInbox(id uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	for !a.closed && len(a.inboxes[id]) == 0 {
		a.cond.Wait()
	}
	if a.closed {
		return []byte("Einbox: interpreter shutting down")
	}
	clone := a.inboxes[id][0]
	a.inboxes[id] = a.inboxes[id][1:]
	return []byte("R" + strconv.FormatUint(clone, 10))
}

// handleTryInbox is the non-blocking form of handleInbox: it pops and returns
// this agent's next message if one is waiting, or an empty reply if not. An
// async Worker event loop polls it between timed Atomics.waitAsync yields, so
// the job queue keeps draining and an async onmessage's promises resolve.
func (a *Agents) handleTryInbox(id uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return []byte("Einbox: interpreter shutting down")
	}
	if len(a.inboxes[id]) == 0 {
		return []byte{'R'} // empty: no message waiting
	}
	clone := a.inboxes[id][0]
	a.inboxes[id] = a.inboxes[id][1:]
	return []byte("R" + strconv.FormatUint(clone, 10))
}

// handlePost takes ownership of the clone handle the agent wrote, tagged with
// its sender so Receive can report it.
func (a *Agents) handlePost(id, clone uint64) []byte {
	a.mu.Lock()
	a.posts = append(a.posts, postedMessage{from: id, clone: clone})
	a.mu.Unlock()
	return []byte{'R'}
}

// handleExit records that the agent's thread ended and drops any messages
// still queued to its inbox. It must NOT free their clones here: this runs on
// the exiting agent's goroutine (via go_host_call), and CloneFree goes through
// the invoke lock that js_close holds while joining agents — freeing here would
// deadlock. The dropped clones are freed by close() on the main goroutine.
func (a *Agents) handleExit(id uint64) []byte {
	a.mu.Lock()
	a.orphaned = append(a.orphaned, a.inboxes[id]...)
	delete(a.inboxes, id)
	if a.alive[id] {
		delete(a.alive, id)
	} else {
		a.exited[id] = true // beat Spawn's bookkeeping; reconciled there
	}
	a.mu.Unlock()
	return []byte{'R'}
}

// parseAgentArgs decodes the reserved-key JSON argument arrays ([id] or
// [id, handle]) the C++ agent primitives send.
func parseAgentArgs(argsJSON []byte, want int) ([]uint64, error) {
	var raw []uint64
	if err := json.Unmarshal(argsJSON, &raw); err != nil {
		return nil, err
	}
	if len(raw) < want {
		return nil, fmt.Errorf("agent call args %s: want %d values", argsJSON, want)
	}
	return raw, nil
}
