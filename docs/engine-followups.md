# Engine follow-ups (TODO)

These are defects and gaps that CANNOT be fixed in the pure-Go compat layer:
their root cause is in the wasm-compiled SpiderMonkey engine
(`github.com/goccy/spidermonkeywasm2go`) or the engine bridge, and closing
them needs a change on the engine side (a C++ `js.cc` change and/or a new
bridge primitive, then a wasm regen). They are collected here so the compat
layer stays honest about what it does not yet cover. Each item lists the
observable symptom, the root cause, and what the engine must provide.

## 1. Worker-teardown deadlock with many live agents — FIXED

- **Symptom:** if a guest spawned ~16 or more `worker_threads.Worker`
  instances that were still alive at teardown (e.g. idle workers each holding
  a `parentPort.on('message', …)` listener), the embedder's teardown hung
  forever. `Runtime.Close()` returned promptly; the hang was in the subsequent
  `js.Close()`. Deterministic: N≤15 tore down in a few hundred ms, N≥16 hung.
- **Root cause (measured, and not the one previously recorded here — there is
  no thread pool at any layer):** every guest thread's stack is malloc'd out of
  LINEAR MEMORY, and a thread created with a null `pthread_attr_t` takes
  wasi-libc's default stack size — which is the LINKED main-stack size, 8 MiB.
  That is right for the main thread and wildly wrong as a per-thread tax:
  16 agents cost 128 MiB of stacks against a 256 MiB instance budget before a
  single JS object exists, and each concurrent SpiderMonkey helper task took
  another 8 MiB exactly when teardown GC needed them most. Past the budget
  `pthread_create` returns EAGAIN, whereupon the helper dispatcher runs engine
  work INLINE on the calling thread; the resulting lost wakeup parked a thread
  in a guest futex, which has no timeout and no trap, while the main goroutine
  held the instance's invoke lock for the whole of `js_close`.
- **Engine fix (landed in `js.cc`):** give agent and helper threads explicit,
  right-sized stacks (`kAgentThreadStackBytes` / `kHelperThreadStackBytes`)
  instead of inheriting the linker's main-stack size, and bound the agent
  context's native recursion inside its own stack. Teardown additionally sets
  each agent's terminate flag, so shutdown does not depend on an agent
  reaching a cooperative poll point. `js.Close()` with 16 live workers now
  returns in tens of milliseconds.
- **Also fixed — agents accumulated permanently.** A finished thread holds its
  entire stack until someone joins it, and nothing joined one until the
  instance closed. An instance that created and retired workers over time grew
  without bound even while never running two at once: 24 sequential
  spawn/exit cycles took linear memory from 33 MiB to 195 MiB. Finished
  threads are now reaped at the next spawn and at close; the same 24 cycles
  now plateau at 40 MiB and stay there.
- **Remaining, by design:** an instance still has a finite agent budget — it
  is now a function of what agents actually use, and `Config.MaxMemoryBytes`
  is the knob that sets it. A spawn past the budget fails with an error
  ("agent spawn failed") rather than hanging.

## 1b. Per-agent memory floor: whole GC chunks are touched for 168 KiB of data — PARTLY FIXED, 3488 → 1144 KiB per agent

**What was fixed.** A worker cost ~3.4 MiB of RAM to hold ~0.16 MiB of live
JavaScript, and the cause was a single line that exists only on this platform —
`js/src/gc/Memory.cpp`, `MapAlignedPages`, under `#ifdef __wasi__`:

```c
posix_memalign(&region, alignment, length);
memset(region, 0, length);      // writes the whole 1 MiB chunk
```

Every other platform writes nothing there: `mmap` hands back zero pages and they
fault in only as the GC actually uses them. On wasm the memset makes each chunk
fully resident the moment it is created, and linear memory can never be returned
to the OS, so it stays that way for the instance's life. **Gecko does not pay
this** — the memset is wasi-only.

Removing it outright is not safe: `ArenaChunkBase`'s mark bitmap and its
free/decommitted page bitmaps are never cleared explicitly and do rely on zeroed
memory. The arena bodies past `FirstArenaOffset` do not — each arena is
initialized when it is handed out. So the patch in
`spidermonkey-wasm/scripts/build-engine-intl.sh` zeroes only the header, taking
the write from **1 MiB to 16 KiB per chunk**, scoped to exactly the chunk case
(`length == alignment == ChunkSize`) so every other caller keeps the
fully-zeroed contract. Measured per agent: **3488 → 1144 KiB resident**.

The diagnosis that led there is below, and still describes the pre-fix numbers.
The remaining floor — three 1 MiB chunks per runtime, of which two are
near-empty in a worker that barely allocates — is unchanged, and is where the
next lever is.

Two different quantities, and they have different causes — keep them apart:

- **Linear memory** (~7 MiB per agent) is the `Config.MaxMemoryBytes` BUDGET.
  It decides how many agents fit; it is not RAM.
- **Resident memory** (~3.3 MiB per agent, measured with `mincore` over the
  instance's mapping; ~4.0 MiB of process RSS once host-side Go is counted) is
  the actual physical cost.

Where the linear memory goes, from instrumenting each startup step: 1 MiB
thread stack, 2 MiB at `JS_NewContext`, 4 MiB at `JS_NewGlobalObject`.
`InitSelfHostedCode` and `InitRealmStandardClasses` cost ZERO — the stencil is
shared with the parent runtime and standard classes are lazy.

Where the RESIDENT memory goes. Scanning linear memory for 1 MiB-aligned GC
chunk headers, and counting resident pages per chunk with `mincore`, one agent
adds **exactly three 1 MiB chunks, each fully resident**:

| chunk | `ChunkKind` | resident | holding |
|---|---|---|---|
| tenured arenas | `TenuredArenas` | 1024/1024 KiB | 41 of 252 arenas = 164 KiB live |
| buffers | `Buffers` | 1024/1024 KiB | — |
| nursery | `NurseryToSpace` | 1024/1024 KiB | 256 KiB nursery capacity |

3072 KiB of the 3488 KiB an agent makes resident is those three chunks; the
remaining ~416 KiB is allocator metadata and small per-runtime structures. The
thread stack is NOT part of it — 48 KiB of its reservation is ever touched.
The main runtime has the identical three-chunk shape.

**So 164 KiB of live JS data costs 3 MiB of resident chunks, a factor of ~19.**

`JS_GetGCParameter(JSGC_TOTAL_CHUNKS)` reports `1`, which is what made this
hard to see: it counts only TENURED chunks. The nursery and buffers chunks are
just as real and just as resident, and they hold almost nothing in a worker
that does not allocate.

No GC parameter changes any of it. `JSGC_MAX/MIN_NURSERY_BYTES` (256 KiB and
64 KiB), `JSGC_MIN_EMPTY_CHUNK_COUNT=0`, `JSGC_ALLOCATION_THRESHOLD` and a
2 MiB heap max were all swept: residency stayed at exactly 3424 KiB in every
configuration. A smaller nursery still occupies a whole chunk — SpiderMonkey's
sub-chunk nursery mode bounds what it USES, not what it holds.

Two mechanisms were checked in the engine source at the pinned tag and are NOT
the cause of the full residency: `ArenaChunk::init`'s
`Poison(ptr, ..., ChunkSize, ...)` and the nursery's `poisonRange` both compile
to nothing in a release build (`Poison` is guarded by
`JS_GC_ALLOW_EXTRA_POISONING`), and both chunk-init branches
(`decommitAllArenas` / `initAsCommitted`) only set header bitmaps. What touches
the chunk bodies is still unidentified.

**The lever, and it is the one worth taking:** a fresh runtime allocates THREE
1 MiB chunks, two of which (buffers, nursery) are near-empty for a worker that
barely allocates. Cutting the chunk count, or making a chunk's untouched tail
stay untouched, is where the per-worker megabytes are. Note also that
per-runtime sharing with the parent IS already working — `staticStrings`,
`commonNames`, `permanentAtoms` and `selfHostStencil` are all owned only by the
parent runtime (`JSRuntime::addSizeOfIncludingThis`), and `InitSelfHostedCode`
measures at zero linear-memory growth. What a child still owns outright is its
atoms table, GC markers, `tempLifoAlloc`, interpreter stack, source cache,
nursery and store buffer.

For contrast, a whole extra REALM on an existing runtime — a fresh global plus
all the standard classes — costs 0.25 MiB. The cost is per-RUNTIME, not
per-global, and an agent needs its own runtime because a JSContext is
single-threaded.

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

Even with that fixed the floor is ~3 MiB of GC chunks plus the thread stack
per agent. Sub-MiB per worker is not reachable while each worker is a full
JSRuntime; the 0.25 MiB shape is a realm, which cannot have its own thread.

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
only ever asks to release chunks it has just CREATED, and since the chunk-header
fix above those pages were never touched — there is nothing to give back. The
pages that are genuinely resident get re-touched by the GC immediately after
release, which is why 27 MiB of successful `MADV_DONTNEED` on linux nets 3.5 MiB
of process RSS and no per-agent change at all.

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

**Where the per-agent megabytes actually are is unchanged by all of this**:
three 1 MiB chunks per runtime, two of them near-empty in a worker that barely
allocates. Cutting the chunk COUNT is the lever; releasing pages afterwards is
not.

## 2. `worker.terminate()` cannot stop a synchronous infinite loop — FIXED

- **Symptom:** `worker.terminate()` on a worker stuck in an infinite
  SYNCHRONOUS loop (e.g. `new Worker('while(true){}', {eval:true})`) never
  stopped it, because terminate was purely cooperative: it posted a
  `__terminate__` sentinel the agent acts on between job-queue drains, and a
  worker that never drains its inbox never sees it.
- **Engine fix (landed):** `js_agent_interrupt(handle, agent_id)` in `js.cc`
  trips that agent's own `JSContext` interrupt and raises a per-agent terminate
  flag, so the agent's script ends with the same UNCATCHABLE exception the host
  interrupt raises on the main context, and the agent leaves rather than
  resuming its pump. Surfaced as `Agents.Interrupt`; `worker.terminate()` now
  sends the sentinel (which reaches a worker blocked in the host, where no
  interrupt lands) AND interrupts (which reaches a worker executing JS, where
  no message lands).

## 3. `unhandledRejection` / `unhandledrejection` never fire — FIXED

- **Symptom:** `process.on('unhandledRejection', …)` and the web
  `unhandledrejection` event never fired; a rejection with no handler was
  silently dropped.
- **Root cause:** SpiderMonkey reports unhandled rejections only through an
  embedder callback (`JS_SetPromiseRejectionTrackerCallback`), which the
  engine wasm did not expose. Native-promise rejection state is not observable
  from pure JS (async-function promises bypass any Promise wrapper).
- **Engine fix (landed):** `js.cc` registers the rejection tracker and exposes
  `js_take_unhandled_rejections`, which hands the host the rejections still
  unhandled and forgets them. The event loop drains it at each microtask
  checkpoint — so a rejection the guest handles in the same tick is never
  reported — and delivers each to a guest hook: the web layer dispatches a
  cancelable `unhandledrejection` on `globalThis`, and `compat/nodejs`
  replaces the hook with `process.emit('unhandledRejection', …)`, falling back
  to `uncaughtException` with origin `unhandledRejection` as Node does.
- **Deliberate divergence:** Node terminates the process when NEITHER listener
  is registered. Here the rejection is reported on stderr and the loop
  continues, so enabling this cannot turn a working embedding into a crashing
  one.

## 4. Intermittent `JS_DestroyContext` teardown SIGSEGV — FIXED, and it was NOT an engine bug

- **Symptom:** the full `compat/nodejs` package intermittently faulted during
  teardown, measured at 3 runs in 16, always inside generated engine code
  reached via `js.Close()` → `JS_DestroyContext`, on a wild pointer (fault
  addresses several GB up, far outside a 256 MiB linear memory).
- **Actual root cause — a double free in the COMPAT layer, not the engine.**
  `fetchAPI.closeAll` released its cached engine handles without clearing the
  fields, and `Object.Free` had no guard. A handle IS a pointer to the guest's
  GC root for that object, so closing a `Runtime` twice — reachable whenever an
  embedder closes explicitly and a deferred close also runs — deleted five GC
  roots twice. Nothing failed at that moment; the corrupted root list only
  surfaced later, when teardown walked it, in whatever unrelated test happened
  to tear down next. Fixed by making both `Object.Free` and `closeAll`
  idempotent. 3/16 → 0/16.
- **Why it read as an engine bug for so long, and the two lessons:**
  - *Never benchmark a local build against the PUBLISHED
    `spidermonkeywasm2go` artifact.* It is CI-built from a different engine
    archive and an older `js.cc`, so it differs in far more than the change
    under test. It measured 0/16 while a local build of the SAME `main` source
    measured 3/16 — which made it look like whatever branch was being tested
    had caused the fault. Build both arms locally.
  - *Get a fast reproducer before forming hypotheses.* The full package is 84 s
    a run, so each guess cost 20+ minutes and several were spent on plausible
    but wrong engine-side theories. One test at `-count=4` reproduced it in
    4.2 s; from there, isolating it took two comparisons — the same script with
    one `Close` (0/6) versus two (6/6).

## 5. Intermittent test262 Atomics structured-clone CI flake

- **Symptom:** a test262 Atomics case using structured clone across agents
  intermittently fails in CI with "extra data after end".
- **Root cause:** a wasi-threads / agent structured-clone race in the engine.
- **Engine fix needed:** fix the race in the engine's agent clone transport.

## 6. Module-loader classification heuristics — engine side landed, loader not yet rewired

- **Symptom:** the module loader uses regex/string heuristics to classify a
  module — CommonJS-exports detection (`cjs_exports.go`), default-export
  detection (`nodejs.go` `hasDefaultExport`), and ESM-syntax detection
  (`resolve.go` `esmSyntax`). These are the load-bearing exceptions to the
  "no heuristics" rule.
- **Engine side (landed):** `js_source_is_module` compiles the source twice —
  once as a module, once inside the CommonJS wrapper function — and reports
  whether it needs module semantics, which is Node's own detection rule and
  the exact question `esmSyntax` is guessing at. It is strictly more correct
  than the regex, which is line-anchored (so it misses a minified one-line
  bundle) and unaware of comments and string literals (so it fires on the word
  `export` inside either). Exposed as `internal.JS.SourceIsModule`.
- **Still to do (loader side):** `resolve.go`'s `classifyJS` has to call it
  instead of `esmSyntax`. That is blocked on one thing: `dispatchModuleLoad`
  (`hostenv.go`) does NOT release the instance's invoke lock the way an
  ordinary host function does, so any bridge call made from inside the module
  loader self-deadlocks. `JS.UnlockForHostCallback` is the intended escape
  hatch and is legal there; wiring it is the prerequisite.
- **Not answerable by ANY module compile:** `cjs_exports.go`. It asks which
  string keys a CommonJS module would end up putting on `module.exports` —
  `exports.foo = …`, `Object.defineProperty(exports, …)`, `module.exports =
  {…}`. None of that is ESM syntax; it is ordinary script code whose result is
  only knowable by RUNNING the module, which the loader (synchronous and
  re-entrancy-locked) cannot do. Removing that heuristic needs either a
  script-AST introspection primitive or a `cjs-module-lexer` equivalent in
  C++, not module introspection.
- **`hasDefaultExport`** needs a module's export-name set, which the public
  JSAPI only exposes through a module NAMESPACE — and that requires the whole
  dependency graph to be loaded and linked, far more than a classification
  sniff should cost. It stays a heuristic until that trade changes.

## 7. ICU / Intl and non-core text encodings

- **Symptom:** full `Intl`/ICU behavior and text encodings beyond
  utf-8/latin1/utf-16le are not available.
- **Root cause:** these are the engine's domain (ICU data compiled into the
  wasm build), not implementable in the compat layer.
- **Engine fix needed:** build the engine wasm with the required ICU data.

## 8. `async_hooks`: a store cannot outlive the call that established it

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
