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
//
// Ownership of an argument's handle: taking an argument AS an object — with
// Value.Object or Value.Export — transfers the handle's GC pin to fn, which
// must Free it (immediately, or once it is done with a retained callback).
// Every other argument is released when the call returns, so an op that reads
// an argument as a primitive need not care whether the guest passed one.
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
	loaderFor ModuleLoaderFor  // the same, with the import's declared type
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
			// Do NOT leak the panic value to the sandboxed guest — Go runtime
			// panics embed host paths/internals. A generic, catchable error is
			// enough for the guest; the key identifies which op for the embedder.
			payload = append([]byte{'E'}, fmt.Sprintf("host call %q failed", key)...)
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
	case internal.AgentHostCallKey:
		// [id, name, payload]: the id is a number, the rest are strings — the
		// shape the engine can carry now that agent calls are not
		// number-only.
		var raw []any
		if err := json.Unmarshal(argsJSON, &raw); err != nil || len(raw) < 2 {
			return append([]byte{'E'}, "agent host call: want [id, name, payload?]"...)
		}
		idF, _ := raw[0].(float64)
		name, _ := raw[1].(string)
		payload := ""
		if len(raw) > 2 {
			payload, _ = raw[2].(string)
		}
		// The agent's goroutine is the caller, so the interpreter lock is not
		// held for it: the function runs directly.
		return e.js.Agents().handleHostCall(uint64(idF), name, payload)
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
		v, verr := fn(e.cfg, args)
		// Still inside the unlocked window: releasing a root re-enters the
		// interpreter exactly as the op's own Free calls do, and doing it after
		// the relock would deadlock against the paused guest.
		releaseArgRoots(args, v)
		return v, verr
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

// releaseArgRoots drops the GC roots the argument decoder minted for the
// object-typed arguments a host Func did not take ownership of.
//
// Every non-primitive argument crosses the bridge as a handle, and a handle IS
// a persistent GC root: decoding one pins that object — and everything it
// reaches, which for a typed array is its whole backing store — until someone
// releases it. An op that reads such an argument as a PRIMITIVE (Int, String,
// Float, Bool) never sees the handle and so cannot release it, and a guest can
// reach that path deliberately: pass an object where a number is expected,
// through a JS wrapper that forwards the value uncoerced, and every call leaks
// a root. Unbounded, guest-triggerable, and no host code is at fault.
//
// So ownership is explicit at the one place it can be: asking for the argument
// AS an object (Value.Object or Value.Export) takes the handle and the duty to
// Free it; anything else leaves it here, and it is released when the call
// returns. The op's own return value is never released — it is about to cross
// back to the guest — and a handle the op already freed releases as a no-op.
func releaseArgRoots(args []Value, ret Value) {
	for _, a := range args {
		switch v := a.(type) {
		case *Object:
			if !v.taken && Value(v) != ret {
				v.Free()
			}
		case opaqueValue: // a symbol or bigint: by handle, but with no *Object
			if Value(v) != ret {
				v.free()
			}
		}
	}
}

func (e *hostEnv) dispatchModuleLoad(argsJSON []byte) []byte {
	var a []string
	_ = json.Unmarshal(argsJSON, &a)
	spec, ref := "", ""
	typ := ModuleTypeUnknown
	if len(a) > 0 {
		spec = a[0]
	}
	if len(a) > 1 {
		ref = a[1]
	}
	// The third argument is the import's declared type. An engine that predates
	// it sends two, and the request then reports ModuleTypeUnknown — which is
	// also what an attribute-less import means, so the absent case reads the
	// same either way.
	if len(a) > 2 {
		switch ModuleType(a[2]) {
		case ModuleTypeJSON:
			typ = ModuleTypeJSON
		case ModuleTypeJavaScript:
			typ = ModuleTypeJavaScript
		}
	}
	// Longest registered prefix wins; the fallback loader takes the rest.
	load := e.loader
	if e.loaderFor != nil {
		lf := e.loaderFor
		load = func(cfg Config, specifier, referrer string) (string, error) {
			return lf(cfg, ModuleRequest{Specifier: specifier, Referrer: referrer, Type: typ})
		}
	}
	for _, r := range e.resolvers {
		if strings.HasPrefix(spec, r.prefix) {
			load = r.load
			break
		}
	}
	if load == nil {
		return nil // no loader → total==0 → C++ falls back to missing-modules
	}
	// Release the invoke lock for the loader, exactly as an ordinary host call
	// does: the guest is paused inside this call, so the loader may re-enter the
	// interpreter — which is what lets it ask SourceIsModule to classify a file
	// instead of guessing from the source text.
	src, err := func() (string, error) {
		if e.js != nil {
			relock := e.js.raw.UnlockForHostCallback()
			defer relock()
		}
		return load(e.cfg, spec, ref)
	}()
	if err != nil {
		return append([]byte{'E'}, err.Error()...)
	}
	return append([]byte{'R'}, src...)
}
