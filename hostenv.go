package spidermonkey

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-spidermonkey/internal"
	"github.com/goccy/spidermonkeywasm2go/base"
)

// agentClock0 anchors the monotonic clock agents observe ($262.agent
// .monotonicNow and friends): time.Since uses Go's monotonic reading.
var agentClock0 = time.Now()

func monotonicMs() float64 { return float64(time.Since(agentClock0)) / float64(time.Millisecond) }

// Func is a Go function callable from guest JavaScript. It receives the
// interpreter's Config (so it can reach Env, stdio and FS) and the call's
// arguments — primitives as data, objects and functions as *Object handles with
// identity preserved. The returned Value crosses back the same way. A returned
// error surfaces in the guest as a thrown Error.
//
// fn may re-enter its interpreter — navigate *Object arguments (Get/Set/Call)
// or even Eval — because the interpreter's invoke lock is released for the
// callback's duration: the guest is paused waiting for fn's reply, so re-entry
// continues from the current stack, exactly like a native function calling
// back into the engine. *Object arguments also stay valid after the evaluation
// returns (their handles pin the objects), so retaining them for later works
// too.
type Func func(cfg Config, args []Value) (Value, error)

// hostEnv implements base.EnvImports — the host side of the guest's env imports.
// It routes the reserved keys (module loader, agent primitives) to their
// handlers and every other key to a registered host Func. One hostEnv per
// interpreter. Calls arrive on the MAIN goroutine (under the instance's invoke
// lock) and, for the agent keys, on AGENT goroutines concurrently — so the
// oversized-reply stash is keyed by the calling thread's module instance.
type hostEnv struct {
	js        *JS // back-reference for decoding object handles; set by New
	cfg       Config
	funcs     map[string]Func
	loader    ModuleLoader     // fallback when no prefix resolver matches
	resolvers []prefixResolver // sorted longest-prefix-first

	stashMu sync.Mutex
	stash   map[*base.Module][]byte
}

func (e *hostEnv) Go_host_call(m *base.Module, keyPtr, keyLen, argsPtr, argsLen int32, thisID int64, outPtr, outCap int32) int32 {
	var key string
	var argsJSON []byte
	var badBounds bool
	base.AccessMemory(m, func(mem []byte) {
		// The pointers/lengths come from the guest; a forged or overflowing
		// range must not slice out of bounds and panic the whole host process.
		n := int64(len(mem))
		if keyPtr < 0 || keyLen < 0 || int64(keyPtr)+int64(keyLen) > n ||
			argsPtr < 0 || argsLen < 0 || int64(argsPtr)+int64(argsLen) > n {
			badBounds = true
			return
		}
		key = string(mem[keyPtr : keyPtr+keyLen])
		argsJSON = append([]byte(nil), mem[argsPtr:argsPtr+argsLen]...)
	})
	if badBounds {
		return 0 // an empty reply; the guest sees a failed host call
	}
	payload := e.safeDispatch(key, argsJSON)
	if int32(len(payload)) <= outCap {
		base.AccessMemory(m, func(mem []byte) { copy(mem[outPtr:], payload) })
	} else {
		e.stashMu.Lock()
		if e.stash == nil {
			e.stash = map[*base.Module][]byte{}
		}
		e.stash[m] = payload
		e.stashMu.Unlock()
	}
	return int32(len(payload))
}

func (e *hostEnv) Go_host_result(m *base.Module, outPtr int32) {
	e.stashMu.Lock()
	p := e.stash[m]
	delete(e.stash, m)
	e.stashMu.Unlock()
	base.AccessMemory(m, func(mem []byte) { copy(mem[outPtr:], p) })
}

// safeDispatch runs dispatch under a recover so a panic in ANY host op — a
// crypto primitive rejecting a malformed argument, a structured-clone decode on
// an agent goroutine, a map mutation in a facade — surfaces to the guest as a
// catchable thrown Error instead of tearing down the whole host process (and
// with it every other instance sharing it). The guest is a sandbox; a bad call
// from it must never crash the embedder.
func (e *hostEnv) safeDispatch(key string, argsJSON []byte) (payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			payload = append([]byte{'E'}, fmt.Sprintf("host call %q panicked: %v", key, r)...)
		}
	}()
	return e.dispatch(key, argsJSON)
}

// dispatch builds the reply the C++ side expects: 'R' + one value encoding on
// success, 'E' + message on error. The module loader replies with 'R' + raw
// source; the agent keys reply with clone handles ('R' + decimal) or a bare
// 'R'. Agent-key calls arrive on agent goroutines: their handlers only touch
// cluster state (no interpreter re-entry, no invoke-lock games).
func (e *hostEnv) dispatch(key string, argsJSON []byte) []byte {
	switch key {
	case internal.ModuleLoaderKey:
		return e.dispatchModuleLoad(argsJSON)
	case internal.AgentReceiveKey:
		args, err := parseAgentArgs(argsJSON, 1)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return e.js.Agents().handleReceive(args[0])
	case internal.AgentInboxKey:
		args, err := parseAgentArgs(argsJSON, 1)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return e.js.Agents().handleInbox(args[0])
	case internal.AgentTryInboxKey:
		args, err := parseAgentArgs(argsJSON, 1)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return e.js.Agents().handleTryInbox(args[0])
	case internal.AgentPostKey:
		args, err := parseAgentArgs(argsJSON, 2)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return e.js.Agents().handlePost(args[0], args[1])
	case internal.AgentSleepKey:
		args, err := parseAgentArgs(argsJSON, 2)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		time.Sleep(time.Duration(args[1]) * time.Millisecond)
		return []byte{'R'}
	case internal.AgentNowKey:
		return []byte("R" + strconv.FormatFloat(monotonicMs(), 'f', 3, 64))
	case internal.AgentExitKey:
		args, err := parseAgentArgs(argsJSON, 1)
		if err != nil {
			return append([]byte{'E'}, err.Error()...)
		}
		return e.js.Agents().handleExit(args[0])
	}
	fn, ok := e.funcs[key]
	if !ok {
		return []byte("Ehost function not registered: " + key)
	}
	// The arguments arrive as a JSON array of value encodings; each decodes to
	// a primitive Value or an *Object whose handle IS the caller's object.
	var raw []json.RawMessage
	if err := json.Unmarshal(argsJSON, &raw); err != nil {
		return []byte("Ehost call arguments undecodable: " + err.Error())
	}
	args := make([]Value, len(raw))
	for i, enc := range raw {
		v, err := decodeValue(e.js, string(enc))
		if err != nil {
			return []byte("Ehost call argument undecodable: " + err.Error())
		}
		args[i] = v
	}
	// Release the invoke lock for the callback so it can re-enter the
	// interpreter (the guest is paused inside go_host_call until we reply).
	ret, err := func() (Value, error) {
		if e.js != nil {
			relock := e.js.raw.UnlockForHostCallback()
			defer relock()
		}
		return fn(e.cfg, args)
	}()
	if err != nil {
		// A Throw(v) surfaces the wrapped value verbatim (its JS type intact);
		// any other error becomes a generic thrown Error with its message.
		if tv, ok := err.(*thrownValue); ok {
			enc, eerr := encodeValue(tv.v)
			if eerr != nil {
				return append([]byte{'E'}, ("host throw unencodable: " + eerr.Error())...)
			}
			return append([]byte{'T'}, enc...)
		}
		return append([]byte{'E'}, err.Error()...)
	}
	enc, eerr := encodeValue(ret)
	if eerr != nil {
		return append([]byte{'E'}, ("host result unencodable: " + eerr.Error())...)
	}
	return append([]byte{'R'}, enc...)
}

func (e *hostEnv) dispatchModuleLoad(argsJSON []byte) []byte {
	var a []string
	_ = json.Unmarshal(argsJSON, &a)
	spec, ref := "", ""
	if len(a) > 0 {
		spec = a[0]
	}
	if len(a) > 1 {
		ref = a[1]
	}
	// Longest registered prefix wins; the fallback loader takes the rest.
	load := e.loader
	for _, r := range e.resolvers {
		if strings.HasPrefix(spec, r.prefix) {
			load = r.load
			break
		}
	}
	if load == nil {
		return nil // no loader → total==0 → C++ falls back to missing-modules
	}
	src, err := load(e.cfg, spec, ref)
	if err != nil {
		return append([]byte{'E'}, err.Error()...)
	}
	return append([]byte{'R'}, src...)
}
