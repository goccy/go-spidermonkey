# Engine follow-ups (TODO)

These are defects and gaps that CANNOT be fixed in the pure-Go compat layer:
their root cause is in the wasm-compiled SpiderMonkey engine
(`github.com/goccy/spidermonkeywasm2go`) or the engine bridge, and closing
them needs a change on the engine side (a C++ `js.cc` change and/or a new
bridge primitive, then a wasm regen). They are collected here so the compat
layer stays honest about what it does not yet cover. Each item lists the
observable symptom, the root cause, and what the engine must provide.

## 1. Worker-teardown deadlock with many live agents

- **Symptom:** if a guest spawns ~16 or more `worker_threads.Worker`
  instances that are still alive at teardown (e.g. idle workers each holding
  a `parentPort.on('message', …)` listener), the embedder's teardown hangs
  forever. `Runtime.Close()` returns promptly; the hang is in the subsequent
  `js.Close()`.
- **Trigger (fully public API):** spawn N benign, still-alive workers, then
  tear the instance down. Deterministic: N≤15 tears down in a few hundred ms,
  N≥~16 hangs (~16 is the engine agent thread-pool ceiling). The workers need
  not misbehave — benign echo workers that merely exist are enough. In a
  pooled deployment a single request that spawns ~16 workers wedges that
  instance's teardown for the process lifetime (liveness DoS).
- **Root cause:** `js.Close()` calls `agents.close()` (which wakes agents
  blocked in a blocking `receive`/`inbox`) and then the wasm `midClose`
  agent-cluster join (`internal/js.go` → engine). That join deadlocks on wasm
  thread-pool exhaustion once the live-agent count saturates the pool: the
  join occupies the main thread while the remaining parked agent threads can
  no longer be scheduled to observe shutdown and exit.
- **Why not fixable in Go:** a compat-layer cooperative drain — send a
  `__terminate__` sentinel to every live agent and re-wake the pumps before
  teardown — drains most agents but cannot reliably drain the last one (it
  gets stuck in a state no host-side `Send`/`Wake`/`Broadcast` can reach), so
  it does not close the deadlock. Below the ceiling `js.Close()` already
  succeeds without any drain, so the mitigation only matters exactly where it
  is unreliable. Not landed.
- **Engine fix needed:** make the agent-cluster shutdown join bounded and not
  dependent on every parked agent thread reaching a cooperative poll point —
  e.g. a per-agent interrupt (see item 2) the teardown can use to force each
  agent thread to unwind, or a join that can proceed once agents are signalled
  rather than waiting for each to self-exit.

## 2. `worker.terminate()` cannot stop a synchronous infinite loop

- **Symptom:** `worker.terminate()` on a worker stuck in an infinite
  SYNCHRONOUS loop (e.g. `new Worker('while(true){}', {eval:true})`) never
  stops it.
- **Root cause:** terminate is cooperative — it posts a `__terminate__`
  sentinel the agent acts on between job-queue drains. A worker that never
  drains its inbox never sees the sentinel. The host context-interrupt only
  targets the main `JSContext`, not a spawned agent's.
- **Engine fix needed:** a per-agent engine interrupt (a `js_agent_interrupt`
  primitive in `js.cc` that trips that agent's `JSContext` `interruptBits_`),
  exposed through the bridge. This same primitive is what item 1 needs.

## 3. `unhandledRejection` / `unhandledrejection` never fire

- **Symptom:** `process.on('unhandledRejection', …)` and the web
  `unhandledrejection` event never fire; a rejection with no handler is
  silently dropped.
- **Root cause:** SpiderMonkey reports unhandled rejections only through an
  embedder callback (`JS_SetPromiseRejectionTrackerCallback`), which the
  published engine wasm does not expose. Native-promise rejection state is not
  observable from pure JS (async-function promises bypass any Promise wrapper).
- **Engine fix needed:** wire `JS_SetPromiseRejectionTrackerCallback` in the
  bridge, surface pending-unhandled rejections to the host after each job
  drain, and route them to `process.emit('unhandledRejection', …)` /
  `globalThis` `unhandledrejection`.

## 4. Intermittent `JS_DestroyContext` teardown SIGSEGV

- **Symptom:** the full `compat/nodejs` package intermittently SIGSEGVs during
  teardown (roughly 1 run in 4). The suite passes clean on rerun.
- **Root cause:** the crashing goroutine faults inside generated engine code
  reached via `js.Close()` → engine `midClose` / `JS_DestroyContext`. It is an
  engine teardown fault, not a compat bug; present at baseline.
- **Engine fix needed:** investigate and fix the engine teardown fault in the
  wasm build. Until then, reruns are expected; do not attribute it to compat
  changes.

## 5. Intermittent test262 Atomics structured-clone CI flake

- **Symptom:** a test262 Atomics case using structured clone across agents
  intermittently fails in CI with "extra data after end".
- **Root cause:** a wasi-threads / agent structured-clone race in the engine.
- **Engine fix needed:** fix the race in the engine's agent clone transport.

## 6. Module-loader classification heuristics

- **Symptom:** the module loader uses regex/string heuristics to classify a
  module — CommonJS-exports detection (`cjs_exports.go`), default-export
  detection (`nodejs.go` `hasDefaultExport`), and ESM-syntax detection
  (`resolve.go` `esmSyntax`). These are the load-bearing exceptions to the
  "no heuristics" rule.
- **Root cause:** the engine exposes no module-introspection API, so the
  loader cannot ask the engine whether a compiled module has ESM syntax / a
  default export / which names it exports; it must guess from source text.
- **Engine fix needed:** a module-introspection bridge (compile-and-query the
  module record: is-module, export names, has-default) so the heuristics can
  be deleted in favor of asking the engine.

## 7. ICU / Intl and non-core text encodings

- **Symptom:** full `Intl`/ICU behavior and text encodings beyond
  utf-8/latin1/utf-16le are not available.
- **Root cause:** these are the engine's domain (ICU data compiled into the
  wasm build), not implementable in the compat layer.
- **Engine fix needed:** build the engine wasm with the required ICU data.

## 8. `async_hooks` bare-`await` interleaving

- **Symptom:** `AsyncLocalStorage` is correct for synchronous `run()`, nested
  `run()`, and explicit propagation (`AsyncResource.bind`/`runInAsyncScope`/
  `ALS.snapshot`), but bare `await` interleaving across independent contexts on
  ONE instance cannot be tracked. (The cfworkers pool gives each request its
  own instance, where ALS is fully correct.)
- **Root cause:** correct tracking of bare-`await` continuations needs engine
  async-context hooks that are not exposed.
- **Engine fix needed:** expose async-context (host-defined async op) hooks
  from the engine so continuations can be associated with their originating
  context.
