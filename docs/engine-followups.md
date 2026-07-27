# Engine follow-ups (TODO)

These are defects and gaps that CANNOT be fixed in the pure-Go compat layer:
their root cause is in the wasm-compiled SpiderMonkey engine
(`github.com/goccy/spidermonkeywasm2go`) or the engine bridge, and closing
them needs a change on the engine side (a C++ `js.cc` change and/or a new
bridge primitive, then a wasm regen). They are collected here so the compat
layer stays honest about what it does not yet cover. Each item lists the
observable symptom, the root cause, and what the engine must provide.

Items are removed from this file as they close; the reasoning that closed one
lives in its commit, not here.

## 1. Per-agent memory floor: a runtime holds three whole GC chunks

An agent (a `worker_threads.Worker`) costs ~1144 KiB resident to hold ~168 KiB
of live JavaScript. Two different quantities are involved and they have
different causes — keep them apart:

- **Linear memory** (~7 MiB per agent) is the `Config.MaxMemoryBytes` BUDGET.
  It decides how many agents fit; it is not RAM.
- **Resident memory** is the actual physical cost, measured with `mincore` over
  the instance's mapping.

Where the linear memory goes, from instrumenting each startup step: 1 MiB
thread stack, 2 MiB at `JS_NewContext`, 4 MiB at `JS_NewGlobalObject`.
`InitSelfHostedCode` and `InitRealmStandardClasses` cost ZERO — the stencil is
shared with the parent runtime and standard classes are lazy.

Where the RESIDENT memory goes. Scanning linear memory for 1 MiB-aligned GC
chunk headers, and counting resident pages per chunk, one agent adds **exactly
three 1 MiB chunks**:

| chunk | `ChunkKind` | holding |
|---|---|---|
| tenured arenas | `TenuredArenas` | 41 of 252 arenas = 164 KiB live |
| buffers | `Buffers` | — |
| nursery | `NurseryToSpace` | 256 KiB nursery capacity |

`JS_GetGCParameter(JSGC_TOTAL_CHUNKS)` reports `1`, which is what made this
hard to see: it counts only TENURED chunks. The nursery and buffers chunks are
just as real and just as resident, and they hold almost nothing in a worker
that does not allocate.

No GC parameter changes any of it. `JSGC_MAX/MIN_NURSERY_BYTES` (256 KiB and
64 KiB), `JSGC_MIN_EMPTY_CHUNK_COUNT=0`, `JSGC_ALLOCATION_THRESHOLD` and a
2 MiB heap max were all swept: residency stayed put in every configuration. A
smaller nursery still occupies a whole chunk — SpiderMonkey's sub-chunk nursery
mode bounds what it USES, not what it holds.

**Root cause of half of it:** SpiderMonkey's GC chunk is 1 MiB and must be
1 MiB-ALIGNED (`js/HeapAPI.h`, `ChunkShift = 20`). wasi-libc's dlmalloc cannot
place an aligned block without over-allocating, so **every 1 MiB chunk grows
linear memory by 2 MiB**. Reproduced standalone, outside SpiderMonkey
entirely: eight `aligned_alloc(1 MiB, 1 MiB)` calls under wasmtime grow memory
by 2048 KiB each and return pointers exactly 2 MiB apart, and it makes no
difference whether dlmalloc has a large free region to work with — the
leftover fragments are unaligned and can never satisfy the next chunk.

So an agent's 6 MiB of engine memory is 3 GC chunks: 3 MiB used, 3 MiB lost to
alignment. The same waste applies to the main runtime's chunks.

**Engine fix needed:** a wasm-specific aligned-page allocator for GC chunks —
`js::gc::MapAlignedPages` growing linear memory directly (growth is already
64 KiB-page aligned) and sub-allocating 1 MiB chunks from it, so alignment
costs one region-sized remainder instead of 100% per chunk. That is a
SpiderMonkey source patch, which `scripts/build-engine-intl.sh` already has a
verified-targeted-edit mechanism for; the build itself runs through the normal
host pipeline (`make wasm`), so no container is required.

The lever after that is the chunk COUNT: two of the three chunks are near-empty
in a worker that barely allocates. Even with both fixed the floor is ~3 MiB of
GC chunks plus the thread stack per agent. Sub-MiB per worker is not reachable
while each worker is a full JSRuntime; the 0.25 MiB shape is a realm, which
cannot have its own thread. (For contrast, a whole extra REALM on an existing
runtime — a fresh global plus all the standard classes — costs 0.25 MiB. The
cost is per-RUNTIME, not per-global, and an agent needs its own runtime because
a JSContext is single-threaded.) Per-runtime sharing with the parent IS already
working: `staticStrings`, `commonNames`, `permanentAtoms` and `selfHostStencil`
are owned only by the parent runtime, and `InitSelfHostedCode` measures at zero
linear-memory growth. What a child still owns outright is its atoms table, GC
markers, `tempLifoAlloc`, interpreter stack, source cache, nursery and store
buffer.

### Why the copy-on-write image does not help a worker

Measured in one process, RSS delta over 32 additions: an extra INSTANCE costs
0.42 MiB, an extra WORKER inside one instance costs 4.00 MiB.

An instance is cheap because it is a RESTORE. Its linear memory is
`mmap(MAP_PRIVATE)` of a snapshot file of an already-started interpreter, and
the snapshot path re-runs nothing — no start section, no `_initialize`, no
`js_new`; it inherits the snapshot's runtime handle. It touches almost no
pages, so the 33 MiB image stays physically shared.

A worker is a BUILD: a real `JS_NewContext` + `JS_NewGlobalObject` + GC chunks,
constructed at run time. No file holds those pages, so there is nothing for
copy-on-write to share — it is not that CoW is disabled for workers, it is
that no identical prior copy exists. Nor can a worker be given its own
mapping: wasi-threads requires every thread of an instance to share ONE linear
memory, which is exactly what makes `SharedArrayBuffer` share memory and where
the agent cluster's `Atomics.wait` waiter list lives. The worker therefore
grows the parent's mapping, and `memory.grow` adds anonymous pages.

The only route to sub-MiB workers is to make each worker its own instance —
the model the cfworkers pool already uses per request. Message passing would
survive: the agent protocol already goes through the host (`\0agent-post` /
`\0agent-inbox` with host-owned clone handles) and the agent→host direction
already serializes fully (`DifferentProcess` scope). What breaks is
cross-worker `SharedArrayBuffer` and `Atomics.wait`, which `js_clone_write`
supports today (`allowSharedMemoryObjects`) and which both Node's
`worker_threads` and test262's agent tests depend on. It is a trade, not a
free win.

### Host-backed decommit was tried, and does not help — do not rebuild it

Releasing GC pages back to the OS looks like the obvious next lever, so this
records that it was built, measured, and removed, and why.

`js::gc::MarkPagesUnusedSoft` is `return 0` on wasi: wasm cannot hand linear
memory back to anyone. An embedder whose linear memory IS a host mapping can
`madvise` it, so the engine was patched to route that one function through a
registered hook and the Go host answered it on a reserved key. It worked end to
end. It changed nothing that matters:

| | darwin/arm64 | linux/arm64 |
|---|---|---|
| of what the engine asked to release, how much was resident | 4.8 MiB of 28 MiB | 27.2 MiB of 27 MiB |
| residency actually returned by `madvise` | none | all of it |
| process RSS, decommit on vs off | no difference | −3.5 MiB |
| **resident per agent, decommit on vs off** | **1084 → 1084 KiB** | **5120 → 5120 KiB** |

The per-agent figure is byte-identical with the mechanism on and off. The engine
only ever asks to release chunks it has just CREATED, and those pages were never
touched — there is nothing to give back. The pages that are genuinely resident
get re-touched by the GC immediately after release, which is why 27 MiB of
successful `MADV_DONTNEED` on linux nets 3.5 MiB of process RSS and no per-agent
change at all.

It also needed `pageSize = PageSize` on wasi to make `DecommitEnabled()` true at
all, which flips chunk init from `initAsCommitted` to `decommitAllArenas`
globally. Paying a global GC behaviour change for a 3.5 MiB one-off on one
platform is not a good trade, so both engine patches and the host plumbing were
reverted.

Platform behaviour, measured against a 64 MiB private anonymous mapping that had
been fully touched, not inferred:

- linux: `MADV_DONTNEED` takes residency to zero and RSS down by the whole
  mapping. `MADV_FREE` changes neither immediately, as documented.
- darwin: `MADV_FREE`, `MADV_FREE_REUSABLE` and `MADV_DONTNEED` **all** return
  success and **all** leave residency and RSS exactly where they were. There is
  no advice on darwin that gives the memory back.

Two traps worth keeping, because both left the mechanism completely dead with
NO symptom — no error, no log line, memory simply unchanged:

- A reserved dispatch key declared one byte longer than its literal never
  matches, and the host-call protocol reports that as "no host implements this",
  which is indistinguishable from a host that legitimately does not.
- **No host-dependent answer may be cached in guest static memory.** A C++
  static lives in linear memory, and linear memory is exactly what the
  copy-on-write instance image snapshots. The snapshot is built once against a
  stub host that answers nothing, so a cached "this host cannot do it" was baked
  into the image and inherited by every restored instance — and restored
  instances never re-run `js_new`, so nothing ever cleared it.

## 2. Intermittent test262 Atomics structured-clone CI flake

- **Symptom:** a test262 Atomics case using structured clone across agents
  intermittently fails in CI with "extra data after end".
- **Root cause:** a wasi-threads / agent structured-clone race in the engine.
- **Engine fix needed:** fix the race in the engine's agent clone transport.

## 3. Two module-loader heuristics no module compile can answer

- **Symptom:** the module loader still extracts a CommonJS module's export
  names, and detects whether a target has a default export, by matching source
  text. These are the load-bearing exceptions to the "no heuristics" rule.
- **`cjs_exports.go`** asks which string keys a CommonJS module would end up
  putting on `module.exports` — `exports.foo = …`,
  `Object.defineProperty(exports, …)`, `module.exports = {…}`. None of that is
  ESM syntax; it is ordinary script code whose result is only knowable by
  RUNNING the module, which the loader (synchronous and re-entrancy-locked)
  cannot do. Removing it needs either a script-AST introspection primitive or a
  `cjs-module-lexer` equivalent in C++ — module introspection does not reach it.
- **`hasDefaultExport`** (`nodejs.go`) needs a module's export-name set, which
  the public JSAPI only exposes through a module NAMESPACE — and that requires
  the whole dependency graph to be loaded and linked, far more than a
  classification sniff should cost. It stays a heuristic until that trade
  changes.

## 4. `async_hooks`: a store cannot outlive the call that established it

This is the item with the widest blast radius, and it stopped being theoretical:
it is what makes **dynamic SSR fail on Next.js 15**.

- **Symptom:** `AsyncLocalStorage` is correct for synchronous `run()`, nested
  `run()`, and explicit propagation (`AsyncResource.bind`/`runInAsyncScope`/
  `ALS.snapshot`). It is NOT correct when work SCHEDULED inside `run()` runs
  after `run()` has returned. (The cfworkers pool gives each request its own
  instance, where ALS is fully correct.)
- **Root cause:** the store is a plain slot held for the duration of `run()`,
  because associating a continuation with the context it was created in needs
  engine async-context hooks that are not exposed. Nothing in userland can
  recover it: a bare `await` on a native promise schedules a reaction job in the
  engine without passing through `then`, `queueMicrotask`, or any other hook the
  compat layer owns.
- **What it costs, concretely.** Next.js 15 enters its per-request store as

  ```js
  const flightReadableStream = workUnitAsyncStorage.run(
      requestStore, ctx.componentMod.renderToReadableStream, RSCPayload, …);
  ```

  `renderToReadableStream` returns a stream SYNCHRONOUSLY; the render happens
  later, as the stream is pulled. The slot is restored the moment `run()`
  returns, so by the time React asks for the store it is gone and the render
  dies with `InvariantError: Expected workUnitAsyncStorage to have a store`.
  Every dynamic Server Component render 500s. Prerendered pages, Route Handlers,
  static assets and Next's own 404 are unaffected, which is exactly the split
  `TestNextJSFlagship` now asserts.
- **Why the obvious workarounds are not taken:** never restoring the slot does
  fix Next (measured), but it leaks a store into every subsequent context, and
  deferring the restore to the next macrotask breaks nested `run()` — the queued
  restores run FIFO where correctness needs LIFO. Both trade a documented
  limitation for an undocumented wrong answer.
- **Engine fix needed:** expose async-context (host-defined async op) hooks from
  the engine so continuations can be associated with their originating context.
