# go-spidermonkey

[![PkgGoDev](https://pkg.go.dev/badge/github.com/goccy/go-spidermonkey)](https://pkg.go.dev/github.com/goccy/go-spidermonkey)

**SpiderMonkey in pure Go — run untrusted JavaScript anywhere Go runs. No cgo,
no WebAssembly runtime, one static binary.**

The engine is Firefox's SpiderMonkey, and every guest script is sandboxed: a
host watchdog can stop a runaway loop from outside, and the guest gets no
filesystem, network, or host environment unless you hand it one.

It is measurably SpiderMonkey: CI runs the official
[test262](https://github.com/tc39/test262) conformance suite — the full
suite, ICU, ES modules, `SharedArrayBuffer`/`Atomics` and multi-agent tests
included — and this embedding passes 52,266 of the 53,406 tests in the
revision [test262.fyi](https://test262.fyi/) measures the big engines against
(97.9% counting skips against us; 98.0% of the 53,329 it hosts). That is
within 0.4 points of the SpiderMonkey nightly shell (98.3% of the same
revision), and in the core `language` and `built-ins` categories it matches or
exceeds it — the whole gap is `Intl`/ICU ([details](#conformance-test262)).

SpiderMonkey is compiled to `wasm32-wasi` by
[`goccy/spidermonkey-wasm`](https://github.com/goccy/spidermonkey-wasm), then
translated ahead of time into Go by
[`wasm2go`](https://github.com/goccy/wasm2go). What you import is ordinary Go
that builds and links like any other package.

```go
js, err := spidermonkey.New(spidermonkey.Config{})
if err != nil {
    log.Fatal(err)
}
defer js.Close()

r, err := js.Eval(context.Background(), "[1, 2, 3].map(x => x * 2).join(',')")
if err != nil {
    log.Fatal(err) // a host/transport failure, or ctx cancelled
}
if r.Error != nil {
    log.Fatal(r.Error) // the script threw
}
fmt.Println(r.Value.String()) // 2,4,6
```

`Eval` reports anything the script does wrong in the `Result`, not as a Go
error: `Error` is non-nil and carries the exception and its stack, and `Value`
is valid only when `Error` is nil. A Go error means the host side failed or
`ctx` was cancelled. Output a host `console` writes goes to `Config.Stdout` /
`Stderr` — SpiderMonkey has no I/O of its own, so nothing reaches the host's
streams unless a host function puts it there.

The global persists across calls, so one `JS` instance behaves like a REPL.
Microtasks are drained before `Eval` returns, so a top-level
`Promise.resolve().then(…)` has run by the time you see the result.

## Isolation

Each `JS` instance owns its own wasm instance — its own linear memory, its own
JavaScript runtime. Two instances share nothing and run concurrently. A single
instance serialises its own calls, so it is safe to use from several goroutines,
but it executes one script at a time.

## Stopping a runaway script

Pass a `context` to `Eval`; cancelling it (a timeout, a deadline, an explicit
`cancel()`) interrupts the running script:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

_, err := js.Eval(ctx, "while (true) {}")
// err == context.DeadlineExceeded — the loop was interrupted
```

Cancellation runs no guest code: the harness sets an interrupt flag in the
instance's linear memory from another goroutine, and SpiderMonkey's interpreter
notices it at the next bytecode loop head. That matters — running guest code on
another goroutine would corrupt the instance's C stack.

The termination is an **uncatchable** exception. A script cannot swallow it:

```js
try { while (true) {} } catch (e) { 'swallowed' }   // never yields 'swallowed'
```

Interruption only lands at bytecode loop heads, so a single long-running
primitive — a pathological regex, a huge sort — is not preempted until it returns
to the interpreter loop.

## What a script can reach

`print()` and `console.log/info/warn/error`, plus the JavaScript standard
library. Nothing else. There is no `fetch`, no timers, no file or network access:
SpiderMonkey has no I/O of its own, the embedding installs no builtin that
reaches any, and the wasm is linked with no host-socket or host-subprocess
capability. Every import the module has is `wasi_snapshot_preview1`.

## Conformance: test262

The claim "the engine is SpiderMonkey" is measured, not asserted. CI runs the
official ECMAScript conformance suite — [tc39/test262](https://github.com/tc39/test262),
vendored as the `test262/suite` submodule, pinned to revision `f2d1435` — the
same revision [test262.fyi](https://test262.fyi/) measures the big engines
against, so the numbers below are directly comparable — on every change:

| area | pass / run | rate |
|---|---|---|
| language | 23,285 / 23,710 | 98.2% |
| built-ins | 23,316 / 23,594 | 98.8% |
| intl402 | 3,013 / 3,341 | 90.2% |
| annexB | 1,058 / 1,086 | 97.4% |
| harness | 116 / 116 | 100% |
| staging | 1,478 / 1,482 | 99.7% |
| **total** | **52,266 / 53,329** | **98.0%** |

That is the FULL suite: ICU is compiled in (`Intl.*`, `Temporal`, regexp
`\p{...}`, Unicode normalization and case folding all run), ES modules load
through the host registry (`module`-flagged tests, `dynamic-import`), and the
engine is built for wasi-threads, so `SharedArrayBuffer`, the whole `Atomics`
family including `Atomics.waitAsync`, and test262's multi-agent tests
(`$262.agent.*`) run for real — every agent is a SpiderMonkey thread that
wasm2go hosts on a goroutine.

### Measured against SpiderMonkey

Counting skipped tests against us — the way test262.fyi counts — this
embedding passes 52,266 of all 53,406 tests (**97.9%**). On the same revision
the SpiderMonkey nightly shell (154.0a1) passes 52,489 (**98.3%**); with
experimental flags, 52,640 (98.6%). The 223-test, 0.4-point gap is not spread
across the suite — it is almost entirely `Intl`:

| area | this embedding (FF 147) | SpiderMonkey nightly (154) | Δ |
|---|---|---|---|
| built-ins | 23,316 | 23,272 | **+44** |
| language | 23,285 | 23,230 | **+55** |
| staging | 1,478 | 1,451 | **+27** |
| harness | 116 | 116 | 0 |
| annexB | 1,058 | 1,084 | −26 |
| intl402 | 3,013 | 3,336 | **−323** |

In the core ECMAScript categories this build matches or *exceeds* the nightly
shell — it does not carry the nightly's regressions — and the only material
deficit is `intl402`, where the engine is seven Firefox versions behind (147
vs 154) and its bundled ICU4X data does not yet cover every locale the newer
Intl proposals exercise. Closing that gap is engine-build work in
[`goccy/spidermonkey-wasm`](https://github.com/goccy/spidermonkey-wasm), not a
limit of running SpiderMonkey as Go.

Only 77 tests are skipped, each accounted with its printed reason: 64
`ShadowRealm` (a proposal off by default in the stock SpiderMonkey shell too),
11 `$262.AbstractModuleSource` (host hook not exposed), and 2 `CanBlockIsFalse`
(this embedding's main agent may block, so the premise does not apply). The
skip set is decided by probing the running engine, so a feature the build
actually ships is never hidden — `Atomics.pause`, for one, is shipped and runs.

The 1,063 failures are pinned one by one in
[`test262/expectations.json`](./test262/expectations.json) — CI is green
exactly when the delta is the documented one, so a regression and a silent
improvement both fail the run. The negative-test judge matches each expected
error by its exact constructor name and phase, so a green run cannot be bought
with a lenient check.

```sh
make test262   # inits the submodule, then TEST262=1 go test -run TestTest262 .
```

## Bounding what a script consumes

| `Config` field | Bounds | On breach |
|---|---|---|
| `MaxMemoryBytes` | the wasm linear memory — GC heap, engine data and the C stack together | the instance traps and is dead |

`MaxMemoryBytes` is the one cap. It sizes the wasm linear memory the guest grows
into, and everything the engine allocates — the GC heap, its own data, the C
stack — lives inside it. It protects the *host process*, not the script: several
SpiderMonkey allocation paths are infallible and abort rather than throw, so
reaching the cap kills the instance rather than surfacing a catchable error.
Runaway *recursion* is the case the engine still catches on its own — it raises a
catchable `InternalError: too much recursion` well before the stack exhausts the
cap.

Raising `MaxMemoryBytes` costs interpreter *construction time*, not resident
memory: the pages are mapped, not touched, and a booted instance's resident set
is the same whatever the cap. Linear memory only ever grows — wasm has no shrink,
and the guest's allocator reuses freed space only when its free list can serve
the request — so a script that churns large allocations creeps toward the cap
even with a small live set. Raise the cap before concluding a script leaks.

To stop a script that runs too *long* rather than allocates too much, cancel the
`context` you pass to `Eval` (see [Stopping a runaway script](#stopping-a-runaway-script)).

The zero `Config` is a usable, sandboxed instance: 256 MiB wasm memory, an empty
environment, and stdio that goes nowhere.

## Performance

`bench/` runs the same JavaScript on three engines. Apple M5, Go 1.26, `-benchtime 10x`:

| | fib(30) | loop sum (1e6) | boot | allocs on the loop |
|---|---|---|---|---|
| go-spidermonkey | 171 ms | 37 ms | 0.3 ms | 27 |
| [goja](https://github.com/dop251/goja) (pure Go) | 182 ms | 48 ms | 2 µs | 2,000,000 |
| node (V8, JIT) | ~3 ms | ~1 ms | 43 ms | — |

Node's row subtracts its 43 ms process startup, which every one of its iterations
pays. It is not a peer: V8 JITs, and a JIT cannot emit machine code from inside a
wasm sandbox, so this engine runs SpiderMonkey's portable baseline
interpreter — enabled at runtime since the v0.2.4 bundle (spidermonkey-wasm
v0.2.5), which also transpiles the engine's atomic accesses to inline Go
intrinsics. The ceiling is there to be honest about the cost of the sandbox.

Against goja — the like-for-like comparison, since both interpret —
go-spidermonkey is now faster on both workloads: a little ahead on the
call-bound `fib`, about a quarter faster on the dispatch-bound loop. And it does
that with **27 allocations against goja's two million**: the guest's values live
inside the wasm instance's linear memory, so they never touch the Go heap and
never enter a Go GC cycle.

Boot costs about 0.3 ms. The instance's 256 MiB linear memory is an mmap'd
copy-on-write mapping, so it reserves address space but stays almost entirely
non-resident until the guest writes to it — and it never lands on the Go heap (a
live instance holds ~0.1 MiB of it). goja boots in microseconds. If you create
an interpreter per request, that difference is the one to weigh; if you keep a
pool of them, it disappears.

```sh
cd bench && go test -bench . -benchmem ./...
cd bench && go test -run TestMemoryFootprint -v ./...
```

## License

- **The Go source code of this repository is licensed under [MIT](./LICENSE).**
  That covers everything written or generated here — `interpreter.go`, the
  generated bridge `spidermonkey.go`, the tests and the benchmarks.
- **The SpiderMonkey engine is not MIT.** It reaches your program through the
  [`spidermonkeywasm2go`](https://github.com/goccy/spidermonkeywasm2go)
  dependency — SpiderMonkey (Mozilla Firefox 147.0.4) translated to Go — which
  is a derivative work of SpiderMonkey and keeps SpiderMonkey's own license, the
  Mozilla Public License, Version 2.0. That license text lives in that
  repository; no SpiderMonkey-derived bytes are vendored here.

### Using go-spidermonkey in your own project

- **As a library dependency** (source distribution): your repository contains no
  SpiderMonkey-derived bytes — only an import path and a `go.mod` entry. License
  your own code however you like (MIT, proprietary, ...); no MPL text needs to
  accompany it. Your users receive go-spidermonkey and spidermonkeywasm2go from
  their own origins, under their own licenses.
- **Shipping a compiled binary**: the binary embeds the translated engine, whose
  files are under the MPL 2.0. The MPL is file-level copyleft: it reaches only
  those already-MPL engine files (their source form must remain available under
  the MPL), and expressly does not extend to your own code that merely links
  against them (§1.10, §3.3). So your application keeps its own license; only the
  engine files retain theirs.

