# Plan: reach the level Bun and Deno are held to, then pass all of WPT

The goal is not a number of our own choosing. Bun and Deno are measured against
the Node.js test suite and the Web Platform Tests, so those are the bar. Beyond
that bar the target is all of WPT; Bun and Deno are the first milestone, not the
destination.

## What the suite is for

The suite defines what to support. It is not a report on what already works.

This was wrong for a long time, and the wrong version is worth stating so it is
not reintroduced: the WPT directory list used to be "the directories whose APIs
compat/web implements", so a directory was added when a capability was added. A
missing API therefore had no failing tests, and the score went UP by not
implementing something. That inverts what a conformance run is for. Worse, the
resulting number was quoted as "WPT", when it covered a subset chosen to
flatter it.

The list is now the union of what Bun and Deno cover, whether or not this
runtime implements the API. A directory where everything fails is the point.

## Where it stands

Measured with the harness in `wpt/`, which runs each `.any.js` / `.worker.js`
file once. See "What the harness does not run yet" below: the number is not
comparable to a browser's until that list is empty.

| suite | measured |
|---|---|
| WPT | **42,591 / 43,401 subtests** in the directories run so far |
| Node.js | 2,611 tests run, **795 pass = 30.4%** |
| Babel | 4,170 / 4,189 fixtures = 99.6% |
| test262 | 52,266 / 53,329 = 98.0% |

### Against Deno, like for like

Deno publishes its expectations (`tests/wpt/runner/expectations/`), so its rate
is computable rather than guessed. Comparing the metric both sides can produce —
the share of test files with ZERO failing subtests, over the `.any.*` corpus:

| | files | zero failures | rate |
|---|---|---|---|
| Deno | 2,190 | 1,361 | **62.2%** |
| this runtime | 564 | 336 | **59.6%** |

Per directory, where both cover it:

| directory | Deno | here |
|---|---|---|
| streams | 90.4% | 37.0% |
| encoding | 92.4% | 64.9% |
| FileAPI | 88.9% | 52.5% |
| user-timing | 95.2% | 69.6% |
| webmessaging | 80.0% | 40.0% |
| console | 100% | 57.1% |
| WebCryptoAPI | 85.8% | 82.9% |
| fetch | 45.8% | 44.9% |
| compression | 50.0% | 52.6% |
| urlpattern | 28.6% | 57.1% |
| url | 75.9% | 95.8% |

Only `url` and `urlpattern` are ahead. Bun publishes no WPT results, so Bun can
only be compared by API surface, not by rate.

## The API surface Bun and Deno have and this runtime does not

Measured by probing the globals after `web.Install`:

| API | Bun | Deno | here |
|---|---|---|---|
| `WebSocket` | yes | yes | **no** |
| `Worker`, `SharedWorker` | yes | yes | **no** |
| `BroadcastChannel` | yes | yes | **no** (compat/nodejs only) |
| `WebAssembly` | yes | yes | **no** |
| `localStorage`, `sessionStorage`, `Storage` | no | yes | **no** |
| `caches`, `CacheStorage` | no | yes | **no** |
| `navigator` (and `navigator.locks`) | yes | yes | **no** |
| `EventSource` | no | yes | **no** |
| `CloseEvent` | yes | yes | **no** |
| `alert`, `confirm`, `prompt` | yes | yes | **no** |
| `ShadowRealm` | yes | no | **no** |

## The standard behind the milestone

ECMA-429 — the WinterTC Minimum Common Web API, WinterCG's successor housed at
Ecma TC55 — is the standard "WinterTC conformant" refers to when Bun and Deno
are compared, and the WPT subset WinterTC is assembling as its test suite will
cover exactly that surface. The suite does not exist yet (checked 2026-07-30:
the WinterTC55 org has no test-suite repository; the TPAC 2025 session says the
subset is being identified, first API snapshot December 2025). Until it does,
the specification's own enumeration is the checklist, and
`compat/web/ecma429_test.go` executes it: every required interface, global and
member, with `known429Gaps` as the recorded exceptions. The one entry is
nothing: `known429Gaps` is empty. `WebAssembly` was the last entry, and it is
now provided host-side over wazero rather than by the engine — the engine
cannot run wasm at all, for reasons recorded in docs/engine-followups.md
item 9 (an architecture gap, not a build flag and not a js.cc export).

## The order of work

Ordered by measured test files unlocked per unit of work.

1. ~~**Harness scope.**~~ DONE for `.any.js`/`.worker.js`: the Bun∪Deno
   directory union, scope and variant expansion, TLS listeners, wptserve
   `pipe=sub` and `{{headers[...]}}`, idlharness scope markers. Still open:
   `.window.js` and testharness `.html` (see below).
2. ~~**`WebSocket`**~~ DONE — 1.80% → 71.29% of subtests, 354/428 cases clean.
   Remaining: WebSocketStream (tentative), cookie-based fixtures.
3. ~~**`EventSource`**~~ DONE — 100% of subtests, 68/68 cases clean.
4. **`Worker` / `SharedWorker` / `BroadcastChannel`** — 52 files in `workers`,
   plus the worker scopes of every `.any.js` that declares one, plus the
   Worker-dependent subtests in web-locks and others.
5. **`webstorage`** — `localStorage` / `sessionStorage`. NOTE: zero
   `.any.js`/`.worker.js` files — this directory only pays after `.window.js`
   support, so it moves after item 1's remainder.
6. **`caches`** — the Cache API (service-workers/cache-storage).
7. ~~**`web-locks`**~~ DONE — 90.76% of subtests, 26/32 cases clean.
   Remaining: two Worker-dependent subtests, idlharness details,
   storage-buckets (tentative).
8. ~~**`WebAssembly`**~~ DONE — 2.55% -> 79.81% of subtests, 88/201 cases
   clean, via the JS API host-side over wazero's interpreter. No engine
   primitive was needed after all: mem.buffer is a guest ArrayBuffer synced
   around each crossing, scoped to the instance's own memories. Remaining:
   WebAssembly.Function and exception handling (post-MVP proposals), table
   plumbing (wazero exposes none), tentative ESM integration.
9. **The directories already covered where Deno is ahead**: streams, FileAPI,
   encoding, webmessaging, user-timing, console, dom/observable, xhr fixtures.

## What the harness does not run yet

Each of these makes the current number higher than a browser-comparable one.

- **Only `.any.js` and `.worker.js`.** `.window.js` files and testharness
  `.html` files are not run at all. In the covered directories that is 59 and
  620 files respectively.
- **No scope expansion.** WPT expands one `.any.js` into every scope its
  `META: global=` names — window, dedicatedworker, sharedworker,
  serviceworker, shadowrealm. Each file runs once here. Of 564 files, 532
  declare two scopes or more, so a browser runs roughly twice as many tests.
- **No variant expansion.** Ten files declare `META: variant=`, 59 variants in
  total; each runs once here.
- **reftests, crashtests, manual tests, wdspec.** Rendering, human interaction
  and the WebDriver protocol are out of scope for a runtime with no DOM.
  Crashtests are not, and are not run either.

## The Node gap, stated plainly

Bun and Deno are far higher on the tests they run, and the reason is not one
missing subsystem — it is per-API fidelity across the whole surface. The
failure histogram, from a clean sharded run:

| tests | first failure |
|---|---|
| 572 | exited with a non-zero code (a mix, diagnosable only per test) |
| 196 | `assert.throws` saw no exception |
| 143 | `assert.throws` saw the wrong error |
| 47 | a spawned process reported no status |
| 38 | `node:repl` is not implemented |
| 14 | a class was called without `new` |

The two `assert.throws` rows are one thing: Node reports a bad argument with a
CODE (`ERR_INVALID_ARG_TYPE`, `ERR_OUT_OF_RANGE`), and its own suite matches on
that code. Two structural findings made that row tractable, and both were worth
more than any single API:

- The callback flavours must validate **synchronously**.
  `fs.readFile(p, "bogus-encoding", cb)` throws at the call; it does not report
  through `cb`. Running the checks inside the deferred operation meant
  `assert.throws` saw nothing, however correct the check itself was.
- A per-API check is worth a fraction of a test on its own, because each of
  these files asserts dozens of argument shapes. Whole FAMILIES have to be
  covered before a file flips. Doing that for `fs` moved it from 66 to 125
  passing, and for `Buffer` from 11 to 25.

Per-module results from that pass, each measured with `NODETEST_FILTER` before
and after:

| module | before | after |
|---|---|---|
| `fs` | 66 | 125 |
| `fs.cp` alone | 30 | 58 |
| `Buffer` | 11 | 25 |
| `repl` | 8 | 20 |
| `process` | 21 | 27 |
| `whatwg-url` | 9 | 16 |
| `vm` | 31 | 34 |
| `zlib` | 7 | 11 |
| `crypto` | 28 | 31 |
| `stream` | 31 | 38 |
| `diagnostics_channel` | 9 | 14 |
| `http` | 83 | 86 |
| `dgram` | 33 | 35 |
| `worker_threads` | 35 | 36 |
| `child_process` | 23 | 24 |
| `console` | 7 | 8 |
| `tls` | 11 | 13 |

Three of those were not validation at all but a defect the validation work
exposed:

- **fork's IPC channel deadlocked.** A child held a loop pending until the
  channel closed, and the parent only closed it once the child finished — so a
  child that finished *normally* never did. Only a crashing child ever
  completed. Node's rule is that the channel keeps a child alive only while it
  is listening.
- **`readline.Interface.write()` had its direction backwards**, writing to the
  output where Node writes to the input as if typed. Every caller that drives an
  Interface programmatically echoed its script instead of running it.
- **`assert.throws(fn, /regexp/)` matched against `err.message`** where Node
  matches against the error's string representation — so an anchored pattern
  could never match, and a bare `/ERR_X/` could not assert on a code.

## The hangs

`nodetest/quarantine.txt` went from 477 to 235. Diagnosing them was hopeless
while the only evidence was `3 pending host op(s)`, so `Loop.Alive()` names the
KIND of handle held — `net.conn`, `net.server`, `http`, `tls`, `dgram`,
`worker`, `stdio`, `http.client`. That turned the list into a histogram, and
the histogram into causes:

1. **A connected socket never started reading.** Node calls `self.read(0)` in
   `afterConnect`; without it a peer's FIN on an idle connection was never seen.
2. **A stream that finished was never destroyed**, so `_destroy` never ran and
   the host connection was never released.
3. **Only the first resolved address was dialled.** `localhost` resolves to
   `::1` and `127.0.0.1`, and a server bound to one refuses the other.
4. **An unhandled exception was reported and execution continued.** Node's
   contract is print-and-exit-1; carrying on left every open handle holding the
   loop. This was the largest single cause.
5. An https request's own TLS settings never reached the transport, so every
   request to a self-signed server failed the handshake.
6. `dgram.send` decided its overload by counting arguments, and zero-length
   datagrams were dropped on receive.
7. A throwing HTTP request handler was swallowed rather than being fatal.

The remaining 235 hang individually as well as in a group — they are not a
scheduling artefact — and the per-test first-error histogram shows about 100
distinct causes. There is no single lever left there.

## Remaining, ordered by measured tests

### `node:tls` — 106 tests

The weakest module by a wide margin: 11 of 117 pass. Unlike the rest of this
list it is not validation — `server.addContext`, SNI callbacks, session
resumption, and upgrading an existing socket to TLS are all absent, and 18 of
the failures are hangs. It needs host work, not JS.

### `node:http2` — 276 tests

Go's `net/http2` provides the transport; the work is Node's `Http2Session` /
`Http2Stream` surface over it.

### Argument validation on what is left

The pass above covered `fs`, `Buffer`, `zlib`, `vm`, `process`, `net`, `dgram`,
`crypto`, `child_process`, `worker_threads`, `tls` and `URLSearchParams` — every
module where a bad argument used to reach the host and come back as an error
about the transport rather than about the call. What remains in this row is
mostly `http` and `stream`, where the checks live on many small methods rather
than a few entry points.

### fetch/api — 530 subtests

Still the largest WPT directory gap after WebCryptoAPI's tentative set. What
remains is spread thin: preflight cache semantics, upload streaming, a handful
of unported wptserve handlers.

### `fs.symlink` — 31 tests

## The hang that is not ours

On an Apple Silicon machine running an amd64 Go toolchain under Rosetta, two
things below this code wedge a long run, and both cost real debugging time
before they were identified:

- the `go` command deadlocks in its own scheduler after a test binary exits,
  leaving the binary unreaped;
- a test binary itself sometimes sticks in the kernel exit path in state `?E`,
  where even SIGKILL does not reach it — after the framework has already
  printed its verdict.

Neither is reproducible from a small run, and neither is a defect in the
runtime. Shard the suite from a PREBUILT binary (`go test -c`) and wait on the
shard's OUTPUT rather than on its process, and both stop mattering.

## Not on this list, and why

- `node:inspector`, `node:v8`, native addons, tests of Node's private internals:
  these cannot work here (SpiderMonkey is not V8; wasm cannot load native code),
  and the nodetest runner classifies them as `Impossible` so they are excluded
  from the denominator rather than counted against it.
- WPT `css`, `wasm`, `service-workers`: no DOM, no WebAssembly in this engine
  build, no service worker scope.
- `websockets`, `webstorage`, `web-locks`, `eventsource`, `xhr`: real gaps, but
  each is a new subsystem rather than a fix.
