package nodejs

// ipc.go: the message channel child_process.fork() gives a parent and its
// child.
//
// fork() is spawn() plus one thing: an IPC channel, so `child.send(msg)` and
// `process.on('message')` reach each other. Without it fork threw outright,
// and 164 tests of Node's own suite are written against it — every one of the
// worker-pool, cluster-shaped and "talk to a helper process" patterns.
//
// A child here is a nested interpreter on a goroutine (see nested.go), not an
// OS process, so the channel is a pair of Go queues rather than a socketpair.
// Messages cross as JSON, which is what Node's IPC does too.

import (
	"sync"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// ipcChannel is one parent/child pair. Each side reads what the other wrote;
// closing it wakes both.
type ipcChannel struct {
	mu       sync.Mutex
	toChild  []string
	toParent []string
	childWak chan struct{} // signalled when toChild grows or the channel closes
	parentW  chan struct{}
	closed   bool
	// Whether each END holds a loop pending on the channel. The two runtimes
	// share this one object, so a single flag had the parent's ref cancel the
	// child's and left one side's pending never released.
	refed map[bool]bool // keyed by isChild
}

func newIPCChannel() *ipcChannel {
	return &ipcChannel{
		childWak: make(chan struct{}, 1),
		parentW:  make(chan struct{}, 1),
	}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// send queues a message for the other side. It reports false once the channel
// is closed, which is what makes `child.send()` return false after disconnect.
func (c *ipcChannel) send(toParent bool, msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if toParent {
		c.toParent = append(c.toParent, msg)
		poke(c.parentW)
	} else {
		c.toChild = append(c.toChild, msg)
		poke(c.childWak)
	}
	return true
}

// take removes the next message addressed to one side. readChildQueue selects
// the toChild queue — the messages a CHILD reads — so the caller passes
// whether it is the child.
func (c *ipcChannel) take(readChildQueue bool) (msg string, ok bool, open bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := &c.toChild
	if !readChildQueue {
		q = &c.toParent
	}
	if len(*q) > 0 {
		m := (*q)[0]
		*q = (*q)[1:]
		return m, true, !c.closed
	}
	return "", false, !c.closed
}

// setRefed records whether this side holds a loop pending, reporting whether
// the state actually changed so the caller adds or drops it exactly once.
func (c *ipcChannel) setRefed(isChild, on bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refed == nil {
		c.refed = map[bool]bool{}
	}
	if c.refed[isChild] == on {
		return false
	}
	c.refed[isChild] = on
	return true
}

// clearRefed drops the flag and reports whether a pending was still held, so
// the reader goroutine releases it exactly once however it got here.
func (c *ipcChannel) clearRefed(isChild bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.refed[isChild]
	if c.refed != nil {
		c.refed[isChild] = false
	}
	return was
}

func (c *ipcChannel) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *ipcChannel) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
	}
	c.mu.Unlock()
	poke(c.childWak)
	poke(c.parentW)
}

func (c *ipcChannel) wake(child bool) chan struct{} {
	if child {
		return c.childWak
	}
	return c.parentW
}

// ipcOps are installed on BOTH sides. The runtime knows which end it is: a
// child has rt.ipcChild set, a parent looks the channel up by child id.
func (rt *Runtime) ipcOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"ipc_send":       rt.opIPCSend,
		"ipc_start":      rt.opIPCStart,
		"ipc_ref":        rt.opIPCRef,
		"ipc_disconnect": rt.opIPCDisconnect,
	}
}

// opIPCSend(id, json) -> bool. id is 0 on the child side (it has exactly one
// peer) and the child's pid on the parent side.
func (rt *Runtime) opIPCSend(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.ValueOf(false), nil
	}
	ch, toParent := rt.ipcFor(int64(args[0].Float()))
	if ch == nil {
		return spidermonkey.ValueOf(false), nil
	}
	return spidermonkey.ValueOf(ch.send(toParent, args[1].String())), nil
}

// opIPCStart(id, onMessage, onClose) begins delivering messages to this side.
// The callbacks are guest functions invoked on the loop.
func (rt *Runtime) opIPCStart(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.Undefined(), nil
	}
	id := int64(args[0].Float())
	ch, toParent := rt.ipcFor(id)
	onMessage := args[1].Object()
	var onClose *spidermonkey.Object
	if len(args) > 2 {
		onClose = args[2].Object()
	}
	if ch == nil {
		freeObjects(onMessage, onClose)
		return spidermonkey.Undefined(), nil
	}
	// A REF'D channel keeps the loop alive exactly as a socket would: a program
	// waiting for a message from its peer has not finished. The CHILD end starts
	// unref'd, because a child that holds the channel open can never finish, and
	// the parent does not close the channel until the child finishes — the two
	// ends would wait on each other forever. ipc_ref flips it once the child is
	// genuinely listening for messages.
	// A side READS the queue the other side writes: the child writes toward the
	// parent (toParent) and reads toChild, and vice versa. Inverting this is
	// silent — each end simply never hears anything.
	isChild := toParent
	keepAlive := len(args) < 4 || args[3].Bool()
	ch.setRefed(isChild, keepAlive)
	if keepAlive {
		rt.loop.AddPending("ipc")
	}
	go func() {
		defer func() {
			if ch.clearRefed(isChild) {
				rt.loop.DonePending("ipc")
			}
		}()
		for {
			msg, ok, open := ch.take(isChild)
			if ok {
				m := msg
				rt.loop.Post(func() error {
					if onMessage != nil {
						onMessage.Call(spidermonkey.ValueOf(m))
					}
					return nil
				})
				continue
			}
			if !open {
				rt.loop.Post(func() error {
					if onClose != nil {
						onClose.Call()
					}
					freeObjects(onMessage, onClose)
					return nil
				})
				return
			}
			<-ch.wake(isChild)
		}
	}()
	return spidermonkey.Undefined(), nil
}

// opIPCRef(id, on) takes or releases the channel's loop pending. The guest
// calls it when the first 'message' listener is added and when the last one
// goes away, which is exactly when the channel starts and stops being a reason
// for the program to keep running.
func (rt *Runtime) opIPCRef(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return spidermonkey.Undefined(), nil
	}
	ch, toParent := rt.ipcFor(int64(args[0].Float()))
	// A closed channel will never deliver again, so taking a pending on it
	// would be a wait with nothing to wait for.
	if ch == nil || (args[1].Bool() && ch.isClosed()) {
		return spidermonkey.Undefined(), nil
	}
	if ch.setRefed(toParent, args[1].Bool()) {
		if args[1].Bool() {
			rt.loop.AddPending("ipc")
		} else {
			rt.loop.DonePending("ipc")
		}
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opIPCDisconnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	if ch, _ := rt.ipcFor(int64(args[0].Float())); ch != nil {
		ch.close()
	}
	return spidermonkey.Undefined(), nil
}

// ipcFor returns the channel for an id and which direction this side writes.
// A child holds one channel and ignores the id; a parent looks it up.
func (rt *Runtime) ipcFor(id int64) (*ipcChannel, bool) {
	if rt.ipcChild != nil {
		return rt.ipcChild, true // the child writes toward the parent
	}
	rt.child.mu.Lock()
	defer rt.child.mu.Unlock()
	return rt.child.ipc[id], false
}
