# go-spidermonkey

Run untrusted JavaScript from Go, on SpiderMonkey — Firefox's engine — with no
CGo and no WebAssembly runtime.

SpiderMonkey is compiled to `wasm32-wasi` by
[`goccy/spidermonkey-wasm`](https://github.com/goccy/spidermonkey-wasm), then
translated ahead of time into Go by
[`wasm2go`](https://github.com/goccy/wasm2go). What you import is ordinary Go
that builds and links like any other package.

```go
i, err := spidermonkey.NewInterpreter(spidermonkey.Config{})
if err != nil {
    log.Fatal(err)
}
defer i.Close()

r, err := i.Eval("[1, 2, 3].map(x => x * 2).join(',')")
if err != nil {
    log.Fatal(err) // a host/transport failure
}
fmt.Println(r.Ok, r.Result) // true 2,4,6
```

`Eval` returns an `EvalResult`, not a Go error, for anything the script does
wrong: `Ok` is false and `Error` carries the exception and its stack. A Go error
means the host side failed. Output the script writes with `print()` or
`console.log()` is captured into `Stdout` / `Stderr` rather than reaching the
host's.

The global persists across calls, so an `Interpreter` behaves like a REPL.
Microtasks are drained before `Eval` returns, so a top-level
`Promise.resolve().then(…)` has run by the time you see the result.

## Isolation

Each `Interpreter` owns its own wasm instance — its own linear memory, its own
JavaScript runtime. Two interpreters share nothing and run concurrently. A single
`Interpreter` serialises its own calls, so it is safe to use from several
goroutines, but it executes one script at a time.

## Stopping a runaway script

```go
ip, err := i.PrepareInterrupt()   // resolve the addresses BEFORE the Eval
if err != nil {
    log.Fatal(err)
}
go func() {
    time.Sleep(time.Second)
    ip.Fire()                     // safe from another goroutine
}()

r, _ := i.Eval("while (true) {}")
// r.Ok == false, r.Error == "JS execution interrupted"
```

`Fire` does not execute any code on the instance — it writes two words into the
instance's linear memory, and SpiderMonkey's interpreter notices at the next
bytecode loop head. That matters: running guest code on another goroutine would
corrupt the instance's C stack.

The termination is an **uncatchable** exception. A script cannot swallow it:

```js
try { while (true) {} } catch (e) { 'swallowed' }   // never yields 'swallowed'
```

`PrepareInterrupt` exists because resolving the addresses takes the instance
lock, which a running `Eval` holds. `Interrupt()` is the convenience form for
when no `Eval` is in flight.

Interruption only lands at bytecode loop heads, so a single long-running
primitive — a pathological regex, a huge sort — is not preempted until it returns
to the interpreter loop.

## What a script can reach

`print()` and `console.log/info/warn/error`, plus the JavaScript standard
library. Nothing else. There is no `fetch`, no timers, no file or network access:
SpiderMonkey has no I/O of its own, the embedding installs no builtin that
reaches any, and the wasm is linked with no host-socket or host-subprocess
capability. Every import the module has is `wasi_snapshot_preview1`.

## Bounding what a script consumes

| `Config` field | Bounds | On breach |
|---|---|---|
| `MaxHeapBytes` | the GC heap | catchable `out of memory`; the interpreter survives |
| `NativeStackQuotaBytes` | recursion depth | catchable `InternalError: too much recursion` |
| `MaxMemoryBytes` | the wasm linear memory | the instance traps and is dead |
| `PrepareInterrupt` / `Fire` | wall-clock time | uncatchable termination; the interpreter survives |

`MaxHeapBytes` is the limit to tune. `MaxMemoryBytes` is a backstop that protects
the *host process*, not a limit the guest recovers from: several SpiderMonkey
allocation paths are infallible and abort rather than throw, so reaching the wasm
cap kills the instance. The two are not interchangeable, and the heap cap must
stay well below the memory cap — the GC needs slack to fail gracefully.
`NewInterpreter` rejects a ratio above 1:4 rather than let it surface later as a
dead instance on whichever allocation shape happened to route through an
infallible path.

The zero `Config` is a usable, sandboxed interpreter: 64 MiB heap, 256 MiB wasm
memory, 512 KiB stack quota, an empty environment, and stdio that goes nowhere.

## Performance

`bench/` runs the same JavaScript on three engines. Apple M5, Go 1.26, `-benchtime 10x`:

| | fib(30) | loop sum (1e6) | boot | allocs on the loop |
|---|---|---|---|---|
| go-spidermonkey | 306 ms | 30 ms | 4.5 ms | 12 |
| [goja](https://github.com/dop251/goja) (pure Go) | 167 ms | 44 ms | 2 µs | 2,000,000 |
| node (V8, JIT) | ~4 ms | ~2 ms | 40 ms | — |

Node's row subtracts its 40 ms process startup, which every one of its iterations
pays. It is not a peer: V8 JITs, and a JIT cannot emit machine code from inside a
wasm sandbox, so this engine is SpiderMonkey's portable baseline interpreter. The
ceiling is there to be honest about the cost of the sandbox.

Against goja — the like-for-like comparison, since both interpret — the trade is
legible. goja is roughly twice as fast on the call-bound `fib`, where every
JavaScript call crosses go-spidermonkey's extra interpreter-in-Go layer.
go-spidermonkey is about a third faster on the dispatch-bound loop, and it does
that with **12 allocations against goja's two million**: the guest's values live
inside the wasm instance's linear memory, so they never touch the Go heap and
never enter a Go GC cycle.

Boot costs 4.5 ms and about 21 MiB of Go heap, essentially all of it the
instance's linear memory. goja boots in microseconds. If you create an
interpreter per request, that difference is the one to weigh; if you keep a pool
of them, it disappears.

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

