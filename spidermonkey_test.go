package spidermonkey_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func newInterp(t *testing.T, cfg spidermonkey.Config) *spidermonkey.Interpreter {
	t.Helper()
	i, err := spidermonkey.NewInterpreter(cfg)
	if err != nil {
		t.Fatalf("NewInterpreter: %v", err)
	}
	t.Cleanup(func() {
		if err := i.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return i
}

// eval runs src and fails the test on a transport error (as opposed to a
// JavaScript-level throw, which the caller inspects).
func eval(t *testing.T, i *spidermonkey.Interpreter, src string) spidermonkey.EvalResult {
	t.Helper()
	r, err := i.Eval(src)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return r
}

func TestEval(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})

	for _, tc := range []struct{ src, want string }{
		{"1 + 2", "3"},
		{"'a' + 'b'", "ab"},
		{"[1,2,3].map(x => x * 2).join(',')", "2,4,6"},
		{"JSON.stringify({a: [1, 2]})", `{"a":[1,2]}`},
		{"(0.1 + 0.2).toFixed(2)", "0.30"},
		{"typeof Symbol.iterator", "symbol"},
		{"new Date(0).toISOString()", "1970-01-01T00:00:00.000Z"},
	} {
		if got := eval(t, i, tc.src); !got.Ok || got.Result != tc.want {
			t.Errorf("Eval(%q) = %+v, want result %q", tc.src, got, tc.want)
		}
	}
}

// The workload go-perl and go-python are benchmarked on: naive recursive
// Fibonacci (call-bound) and a tight accumulate loop (dispatch-bound). fib(30)
// drives 2.7M calls, which is also a fair exercise of the guest's C stack under
// the default native-stack quota.
func TestFibonacciAndLoop(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})

	if r := eval(t, i, "function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2) }"); !r.Ok {
		t.Fatalf("define fib: %+v", r)
	}
	for _, tc := range []struct{ src, want string }{
		{"fib(10)", "55"},
		{"fib(20)", "6765"},
		{"fib(30)", "832040"},
	} {
		if r := eval(t, i, tc.src); !r.Ok || r.Result != tc.want {
			t.Errorf("%s = %+v, want %s", tc.src, r, tc.want)
		}
	}

	r := eval(t, i, "(() => { let s = 0; for (let k = 0; k < 1000000; k++) s += k; return s })()")
	if !r.Ok || r.Result != "499999500000" {
		t.Errorf("loop sum = %+v, want 499999500000", r)
	}
}

// The global persists across calls, so an Interpreter behaves like a REPL.
func TestEvalGlobalPersists(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	if r := eval(t, i, "globalThis.x = 41"); !r.Ok {
		t.Fatalf("assign: %+v", r)
	}
	if r := eval(t, i, "x + 1"); !r.Ok || r.Result != "42" {
		t.Errorf("x + 1 = %+v, want 42", r)
	}
}

// print()/console.* are the only output a script has; they are captured rather
// than written to the host's stdio.
func TestEvalCapturesOutput(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	r := eval(t, i, "print('out'); console.log('log'); console.error('err'); 'done'")
	if !r.Ok || r.Result != "done" {
		t.Fatalf("result = %+v", r)
	}
	if r.Stdout != "out\nlog\n" {
		t.Errorf("Stdout = %q, want %q", r.Stdout, "out\nlog\n")
	}
	if r.Stderr != "err\n" {
		t.Errorf("Stderr = %q, want %q", r.Stderr, "err\n")
	}
}

// An uncaught throw is a result, not a Go error: the interpreter stays usable.
func TestEvalThrow(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	r := eval(t, i, "throw new Error('boom')")
	if r.Ok {
		t.Fatalf("expected the throw to be reported, got %+v", r)
	}
	if !strings.Contains(r.Error, "boom") {
		t.Errorf("Error = %q, want it to mention boom", r.Error)
	}
	if r := eval(t, i, "1 + 1"); !r.Ok || r.Result != "2" {
		t.Errorf("interpreter unusable after a throw: %+v", r)
	}
}

// Microtasks queued by a script are drained before Eval returns.
func TestEvalDrainsPromiseJobs(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	if r := eval(t, i, "globalThis.p = 0; Promise.resolve(7).then(v => { p = v })"); !r.Ok {
		t.Fatalf("queue: %+v", r)
	}
	if r := eval(t, i, "p"); !r.Ok || r.Result != "7" {
		t.Errorf("promise job did not run: %+v", r)
	}
}

// The sandbox: a script gets print/console and the standard library, and no way
// to reach the host. Anything added here is new attack surface.
func TestNoHostSurface(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	for _, name := range []string{
		"fetch", "setTimeout", "setInterval", "queueMicrotask",
		"XMLHttpRequest", "WebAssembly", "read", "readFile", "os", "process",
	} {
		r := eval(t, i, "typeof "+name)
		if !r.Ok || r.Result != "undefined" {
			t.Errorf("%s is exposed to guest scripts (typeof = %q)", name, r.Result)
		}
	}
}

// Interpreters are fully isolated: each owns its own wasm instance, so a global
// set in one is invisible to the other, and they can run concurrently.
func TestInterpretersAreIsolated(t *testing.T) {
	a := newInterp(t, spidermonkey.Config{})
	b := newInterp(t, spidermonkey.Config{})

	if r := eval(t, a, "globalThis.secret = 'a'"); !r.Ok {
		t.Fatal(r)
	}
	if r := eval(t, b, "typeof secret"); !r.Ok || r.Result != "undefined" {
		t.Errorf("b sees a's global: %+v", r)
	}

	var wg sync.WaitGroup
	for _, i := range []*spidermonkey.Interpreter{a, b} {
		wg.Add(1)
		go func(i *spidermonkey.Interpreter) {
			defer wg.Done()
			// The global persists across evals, so the script must not
			// re-declare a `let` — it would throw on the second call.
			const src = "(() => { let s = 0; for (let k = 0; k < 1000; k++) s += k; return s })()"
			for n := 0; n < 20; n++ {
				if r, err := i.Eval(src); err != nil || r.Result != "499500" {
					t.Errorf("concurrent eval: %+v %v", r, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// A watchdog goroutine must be able to stop a runaway script. The addresses are
// resolved up front because resolving them needs the instance lock that the
// running Eval holds.
func TestInterrupt(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	ip, err := i.PrepareInterrupt()
	if err != nil {
		t.Fatalf("PrepareInterrupt: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		ip.Fire()
	}()

	start := time.Now()
	r := eval(t, i, "while (true) {}")
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("interrupt took %v", elapsed)
	}
	if r.Ok {
		t.Fatalf("infinite loop was not interrupted: %+v", r)
	}
	if !strings.Contains(r.Error, "interrupted") {
		t.Errorf("Error = %q, want it to report an interrupt", r.Error)
	}

	if r := eval(t, i, "1 + 1"); !r.Ok || r.Result != "2" {
		t.Errorf("interpreter unusable after an interrupt: %+v", r)
	}
}

// The security property that makes the interrupt trustworthy: guest JavaScript
// cannot swallow it.
func TestInterruptIsUncatchable(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	ip, err := i.PrepareInterrupt()
	if err != nil {
		t.Fatalf("PrepareInterrupt: %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		ip.Fire()
	}()

	r := eval(t, i, "try { while (true) {} } catch (e) { 'swallowed' } finally { 'ignored' }")
	if r.Ok {
		t.Fatalf("try/catch swallowed the interrupt: %+v", r)
	}
	if strings.Contains(r.Result, "swallowed") {
		t.Errorf("catch block ran: %+v", r)
	}
}

// An interrupt consumes SpiderMonkey's armed state, so the runtime has to
// re-arm. Without that, only the first interrupt on an Interpreter fires and
// every later one hangs.
func TestInterruptRepeats(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	ip, err := i.PrepareInterrupt()
	if err != nil {
		t.Fatalf("PrepareInterrupt: %v", err)
	}
	for n := 0; n < 3; n++ {
		go func() {
			time.Sleep(150 * time.Millisecond)
			ip.Fire()
		}()
		if r := eval(t, i, "while (true) {}"); r.Ok {
			t.Fatalf("interrupt %d did not fire: %+v", n, r)
		}
	}
	if r := eval(t, i, "'alive'"); !r.Ok || r.Result != "alive" {
		t.Errorf("interpreter unusable after repeated interrupts: %+v", r)
	}
}

// A script that runs to completion must not be interrupted by an interrupt
// nobody requested.
func TestNoSpuriousInterrupt(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	r := eval(t, i, "let s = 0; for (let k = 0; k < 3000000; k++) s += k; s > 0")
	if !r.Ok || r.Result != "true" {
		t.Errorf("long loop did not complete: %+v", r)
	}
}

// Runaway recursion must raise a catchable JavaScript error rather than
// overflow the guest's C stack, which would trap the whole instance.
func TestRecursionIsBounded(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	r := eval(t, i, "function f() { return f() } try { f() } catch (e) { e.name }")
	if !r.Ok || r.Result != "InternalError" {
		t.Fatalf("expected a catchable InternalError, got %+v", r)
	}
	if r := eval(t, i, "1 + 1"); !r.Ok {
		t.Errorf("interpreter unusable after over-recursion: %+v", r)
	}
}

// The GC heap cap turns runaway allocation into a catchable out-of-memory,
// leaving the interpreter usable. (The wasm memory cap, by contrast, is a
// backstop that kills the instance — see Config.MaxMemoryBytes.)
func TestHeapCap(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{MaxHeapBytes: 16 << 20, MaxMemoryBytes: 256 << 20})
	r := eval(t, i, "let a = []; try { for (;;) a.push(new Array(100000).fill(0)) } finally { a = null }")
	if r.Ok {
		t.Fatalf("runaway allocation was not stopped: %+v", r)
	}
	if !strings.Contains(r.Error, "out of memory") {
		t.Errorf("Error = %q, want an out-of-memory report", r.Error)
	}
	if r := eval(t, i, "1 + 1"); !r.Ok {
		t.Errorf("interpreter unusable after out-of-memory: %+v", r)
	}
}

// A heap cap the GC cannot fail gracefully under is rejected at construction
// rather than left to trap the instance later, on whichever allocation shape
// happens to route through an infallible path.
func TestHeapCapRatioRejected(t *testing.T) {
	_, err := spidermonkey.NewInterpreter(spidermonkey.Config{
		MaxHeapBytes:   128 << 20,
		MaxMemoryBytes: 256 << 20,
	})
	if err == nil {
		t.Fatal("expected a too-large MaxHeapBytes to be rejected")
	}
	if !strings.Contains(err.Error(), "MaxHeapBytes") {
		t.Errorf("error = %v, want it to name MaxHeapBytes", err)
	}
}
