// Package jsbench compares three ways to run JavaScript from Go:
//
//   - go-spidermonkey: real SpiderMonkey compiled to wasm and transpiled to
//     pure Go by wasm2go (this project). Interpreter-only: the engine is built
//     --disable-jit --enable-portable-baseline-interp, because a JIT cannot
//     emit machine code from inside a wasm sandbox.
//   - goja:           github.com/dop251/goja, a pure-Go JavaScript VM (ES5.1
//     plus parts of ES6). Also interpreter-only.
//   - node:           the host's V8, with its JIT. Not a peer — it is here to
//     show what the ceiling looks like when a JIT is allowed.
//
// All three run the SAME JavaScript source. The go-spidermonkey and goja
// benchmarks keep a persistent global across iterations, so the timed region is
// the workload, not engine boot; node pays a fresh process per iteration, which
// its startup benchmark isolates.
//
//	go test -bench . -benchmem ./...
//	go test -run TestMemoryFootprint -v ./...
package jsbench

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/dop251/goja"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

// Workloads (identical source for every engine).
const (
	// The classic naive recursive Fibonacci. fib(30) drives 2.7M calls: a
	// deep-recursion, call-bound workload that stresses an interpreter's
	// dispatch and its call machinery rather than its arithmetic.
	fibDef  = `function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2) }`
	fibCall = `String(fib(30))`
	fib30   = "832040"

	// A tight arithmetic loop: dispatch-bound rather than call-bound. Wrapped
	// in an IIFE because the global persists across iterations, and a re-run
	// `let` would throw. String(...) makes the engines agree on the completion
	// value's text — the workload is the loop, and one final coercion is noise
	// against a million iterations.
	loopSum    = `String((() => { let s = 0; for (let k = 0; k < 1000000; k++) s += k; return s })())`
	loopSumVal = "499999500000"
)

// ---- go-spidermonkey (wasm2go SpiderMonkey) -------------------------------

func smInstance(tb testing.TB) *spidermonkey.JS {
	tb.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return js
}

func smEval(tb testing.TB, js *spidermonkey.JS, src, want string) {
	tb.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		tb.Fatalf("eval host error: %v", err)
	}
	if r.Error != nil {
		tb.Fatalf("eval js error: %v", r.Error)
	}
	if want != "" && r.Value.String() != want {
		tb.Fatalf("eval %q = %q, want %q", src, r.Value.String(), want)
	}
}

func BenchmarkFibRecursive_GoSpiderMonkey(b *testing.B) {
	js := smInstance(b)
	defer js.Close()
	smEval(b, js, fibDef, "")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		smEval(b, js, fibCall, fib30)
	}
}

func BenchmarkLoopSum_GoSpiderMonkey(b *testing.B) {
	js := smInstance(b)
	defer js.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		smEval(b, js, loopSum, loopSumVal)
	}
}

func BenchmarkStartup_GoSpiderMonkey(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		js := smInstance(b)
		if err := js.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// ---- goja (pure-Go JavaScript VM) -----------------------------------------

func gojaRun(tb testing.TB, vm *goja.Runtime, src, want string) {
	tb.Helper()
	v, err := vm.RunString(src)
	if err != nil {
		tb.Fatalf("goja run: %v", err)
	}
	if want != "" && v.String() != want {
		tb.Fatalf("goja %q = %q, want %q", src, v.String(), want)
	}
}

func gojaVM(tb testing.TB, setup string) *goja.Runtime {
	tb.Helper()
	vm := goja.New()
	if setup != "" {
		gojaRun(tb, vm, setup, "")
	}
	return vm
}

func BenchmarkFibRecursive_Goja(b *testing.B) {
	vm := gojaVM(b, fibDef)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		gojaRun(b, vm, fibCall, fib30)
	}
}

func BenchmarkLoopSum_Goja(b *testing.B) {
	vm := gojaVM(b, "")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		gojaRun(b, vm, loopSum, loopSumVal)
	}
}

func BenchmarkStartup_Goja(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		vm := goja.New()
		gojaRun(b, vm, "1", "1")
	}
}

// ---- node (V8, JIT) -------------------------------------------------------
//
// A fresh process per iteration, so each measurement carries node's own startup
// (tens of milliseconds). BenchmarkStartup_Node measures that alone; subtract it
// to read the workload cost. This is not a like-for-like comparison — V8 JITs
// and the other two do not — it is the ceiling.

func nodeRun(tb testing.TB, script, want string) {
	tb.Helper()
	out, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		tb.Fatalf("node: %v", err)
	}
	if got := strings.TrimSpace(string(out)); want != "" && got != want {
		tb.Fatalf("node = %q, want %q", got, want)
	}
}

func requireNode(tb testing.TB) {
	tb.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		tb.Skipf("node not available: %v", err)
	}
}

func BenchmarkFibRecursive_Node(b *testing.B) {
	requireNode(b)
	script := fibDef + "; process.stdout.write(String(" + fibCall + "))"
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		nodeRun(b, script, fib30)
	}
}

func BenchmarkLoopSum_Node(b *testing.B) {
	requireNode(b)
	script := "process.stdout.write(String(" + loopSum + "))"
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		nodeRun(b, script, loopSumVal)
	}
}

func BenchmarkStartup_Node(b *testing.B) {
	requireNode(b)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		nodeRun(b, "process.stdout.write('1')", "1")
	}
}

// ---- memory footprint (Go heap held by one live engine) --------------------

// keepAlive holds the engine under measurement so the GC cannot collect it
// between building it and reading MemStats.
var keepAlive any

// TestMemoryFootprint reports the Go heap a single live engine occupies after
// booting and running fib(30) once. node is absent by construction: its memory
// lives in another process. Run with -v.
func TestMemoryFootprint(t *testing.T) {
	// Deltas are signed: the GC can return more than the engine allocated (the
	// setup's garbage), and an unsigned subtraction would print that as a
	// gigabyte-sized footprint.
	mib := func(before, after uint64) float64 {
		return float64(int64(after)-int64(before)) / (1024 * 1024)
	}
	measure := func(name string, build func() any) {
		keepAlive = nil
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		keepAlive = build()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		t.Logf("%-18s heapAlloc=%7.2f MiB  heapObjects=%8d  sys=%7.2f MiB",
			name, mib(before.HeapAlloc, after.HeapAlloc),
			int64(after.HeapObjects)-int64(before.HeapObjects),
			mib(before.Sys, after.Sys))
		keepAlive = nil
	}

	measure("go-spidermonkey", func() any {
		js := smInstance(t)
		t.Cleanup(func() { js.Close() })
		smEval(t, js, fibDef, "")
		smEval(t, js, fibCall, fib30)
		return js
	})
	measure("goja", func() any {
		vm := gojaVM(t, fibDef)
		gojaRun(t, vm, fibCall, fib30)
		return vm
	})
}
