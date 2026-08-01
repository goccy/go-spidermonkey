# External conformance suites

test262 measures the ENGINE. It says nothing about `compat/nodejs`, `compat/web`
or `compat/cfworkers`, which are where a real application actually lands. These
three runs close that gap, each against the upstream project's own tests rather
than tests written here:

| suite | what it measures | run it |
|---|---|---|
| [nodetest](../nodetest) | `compat/nodejs`, against the Node.js project's own test suite | `make nodetest` |
| [wpt](../internal/testutil/wpt) | `compat/web`, against the Web Platform Tests | `make wpt` |
| [babeltest](../babeltest) | the whole stack, against Babel's fixture corpus | `make babeltest` |
| [test262](../test262) | the engine (ECMA-262) | `make test262` |

`make suites` runs all four.

## Where they stand (measured on the pinned revisions)

| suite | result |
|---|---|
| test262 | 52,266 / 53,329 = **98.0%** |
| Babel | 4,170 / 4,189 fixtures = **99.55%** (890 skipped) |
| WPT | 35,694 / 43,142 subtests = **82.7%** |
| Node.js | 2,611 tests run, **795 passing = 30.4%** |

The Babel figure comes with a cross-check worth repeating whenever the pin
moves: every one of the 19 remaining failures ALSO fails under real Node.js
running the same published `@babel/*` packages — they are fixtures that moved
ahead of the released packages, or that depend on the Babel monorepo's own
layout. On that corpus this runtime is behaviourally identical to Node. To
redo it, run `babeltest/js/fixtures.js` under `node` with `__babeltest_root`
set to a fixtures directory and diff its failures against `expectations.json`.

WPT by directory, and the one split that the total hides:

| directory | passing |
|---|---|
| mimesniff | 98.6% |
| compression | 95.6% |
| WebCryptoAPI | 83.8% |
| url | 81.1% |
| dom/events | 80% |
| fetch/data-urls | 78.6% |
| html/webappapis | 76.9% |
| FileAPI | 73.3% |
| user-timing | 70.8% |
| fetch/api | 65.3% |
| encoding | 60.2% |
| streams | 56.1% |
| urlpattern | 54.9% |
| webmessaging | 42.9% |

WebCryptoAPI is 35.9k of the 43.1k subtests, so it dominates the total. Split
by whether the spec is stable, it is **93.8% on the stable Web Crypto spec**
and 51.4% on the `.tentative.` files — draft algorithms (ML-DSA, AES-OCB,
KMAC, cSHAKE, X448, Ed448, ChaCha20-Poly1305) that neither Node nor Deno
implements either.

The Node number is the honest weak axis, and
[conformance-plan.md](conformance-plan.md) records why with the measured
failure histogram rather than an estimate, along with the per-module before and
after of the argument-validation pass — `fs` 66 → 124, `Buffer` 11 → 25,
`repl` 8 → 20 — and the three defects that pass exposed, of which the
fork IPC deadlock was the one that mattered most.

## The bar: what Bun and Deno measure themselves against

These suites are the ones the other Node-compatible runtimes are held to, so
their coverage is the target — not a number of our own choosing. Measured from
their repositories (2026-07-28):

| | Node tests registered | actually run |
|---|---|---|
| Deno (`tests/node_compat/config.jsonc`) | 3,786 | **3,411** (375 disabled, each with a reason) |
| Bun (`test/js/node/test/parallel`, vendored) | 3,505 (+67 sequential) | ~all of them |
| here | 4,883 (the whole upstream corpus) | see `nodetest/expectations.json` |

Deno's disabled reasons read almost identically to ours — Node internals, CLI
flags, the inspector, its own permission model. The difference in what RUNS is
mostly one bucket: the ~650 tests that re-execute `process.execPath`. Deno runs
them because the `deno` binary answers as a node-compatible executable. Nothing
prevents the same here — a host can re-enter this runtime — so those are
counted as unimplemented, not impossible.

For WPT, Deno tracks **26 directories** (`tests/wpt/runner/expectations/`) with
the same per-file expectation approach used here. The ones it tracks that this
run does not, and why:

- **APIs we have, so these are simply not measured yet**: `compression`,
  `user-timing`, `webmessaging`, `mimesniff` — turning them on immediately
  showed `CompressionStream` and `MessageChannel` missing from `compat/web`
  (both exist only under `compat/nodejs`, though both are WinterTC web APIs).
- **APIs we do not have**: `websockets`, `webstorage`, `web-locks`,
  `eventsource`, `xhr`, `workers` (Web Workers, as opposed to
  `worker_threads`), `service-workers`.
- **Not applicable to this embedding**: `css`, `wasm` (no WebAssembly in this
  engine build), `schema`.

## How they are wired

Each suite is **pinned to an exact upstream revision** in the Makefile and
fetched on demand by `scripts/fetch-suite.sh` (a blobless, sparse, depth-1
clone). None of them is vendored: nodejs/node's test tree alone is ~80 MB and
web-platform-tests is gigabytes. The checkouts land in `<suite>/suite/` and are
gitignored.

Each has the same shape as the test262 run, so there is one thing to learn:

- every test executes in a **fresh interpreter**;
- what this embedding genuinely cannot host is **skipped with a reason**, and
  the reasons are counted and printed — never silently passed;
- a checked-in `expectations.json` lists the known failures, and the run **fails
  on a regression AND on a stale expectation**, so green means "exactly the
  documented delta", not "no worse than some remembered number";
- `<SUITE>_UPDATE=1` regenerates the expectations file, `<SUITE>_FILTER`
  narrows a run, `<SUITE>_REPORT=path` dumps failures as JSON.

## nodetest — the Node.js test suite

A Node test is entered exactly as Node enters it: the checkout is mounted
read-only with every write absorbed by an in-memory overlay (so `tmpdir` works
without touching the checkout, and tests cannot interfere with each other), and
the file is required as the entry module, so `require('../common')` resolves and
the `common.mustCall` assertions registered on `'exit'` are what judge the run.

`nodetest/policy.go` holds the one judgement call: whether a test is addressed
to a public API this layer implements, or to something only the `node` binary
can answer. Skipped, with the reason recorded:

- **tests node-private internals** — `require('internal/…')`, `internalBinding`.
  These test Node's implementation, not its API.
- **respawns the node binary** — anything touching `process.execPath`. This
  runtime is a library; there is no executable to re-exec.
- **loads a native addon** — impossible in a wasm sandbox.
- **needs node flag X** — the test configures a binary we are not. The honored
  set is a small allow-list in `policy.go`; as a flag becomes supported it moves
  into the list and its tests start running by themselves.
- **module M not implemented** — one entry per unimplemented core module, so the
  skip list doubles as the roadmap.

The policy is mechanical (source markers and the test's own `// Flags:` line)
rather than a hand-maintained list of filenames, so it cannot quietly go stale
as the suite moves.

### Run it in shards

`NODETEST_SHARD=i/n` takes every n-th test, so the suite can be spread over
separate PROCESSES:

```sh
go test -c -o /tmp/nodetest.bin ./nodetest        # build ONCE
for i in $(seq 0 7); do
  ( cd nodetest && NODETEST=1 NODETEST_SHARD=$i/8 NODETEST_REPORT=/tmp/shard-$i.json \
      /tmp/nodetest.bin -test.run TestNodeSuite -test.v -test.timeout 15m ) > /tmp/shard-$i.txt 2>&1
done
```

This is not only about speed. Some Node tests block inside a host call — a
socket read, a subprocess wait, an `Atomics.wait` nobody notifies — and the
engine's interrupt cannot reach those, so the call is abandoned at its deadline
with its interpreter still alive (see `JS.Close`). Enough abandoned
interpreters in ONE process and that process stops making progress: every
goroutine parks and even the harness's own watchdog timer stops firing. The
cause is not yet understood; it is under `docs/engine-followups.md`. Sharding
bounds it to one shard's worth of results while it is open.

Two details of the loop above are deliberate, and both cost real debugging time
on an Apple Silicon machine running an amd64 Go toolchain under Rosetta:

- **Build the binary once and run it directly.** The `go` command there
  reliably deadlocks in its own scheduler after a test binary exits, leaving
  the binary unreaped and the shard apparently hung at 0% CPU. Every thread
  sits in `findRunnable`/`stopm`; SIGQUIT clears it. Keeping the go command out
  of the loop avoids it entirely (and skips eight rebuilds).
- **`cd nodetest` first.** `go test` runs a test binary with the package
  directory as its working directory; a prebuilt binary does not, and the suite
  resolves its checkout relative to it.

For an unattended run, wait on each shard's OUTPUT reaching `^(PASS|FAIL|ok)`
rather than on its process: a test binary occasionally sticks in the kernel
exit path in state `?E` — where even SIGKILL does not reach it — after the
framework has already printed its verdict.

## wpt — the Web Platform Tests

Only the `.any.js` / `.worker.js` forms can run without a browser, and only the
directories whose APIs `compat/web` provides are in the default set
(`internal/testutil/wpt/suite_test.go`, `DefaultDirs`) — a DOM-dependent directory would produce
nothing but noise.

- The suite's own `testharness.js` drives each file in its **shell environment**.
  No DOM is faked: testharness has a first-class no-`Window` mode, so what runs
  is the real harness rather than an imitation of it.
- A **loopback HTTP server** serves the checkout, because a large share of the
  suite fetches its own fixtures — and `fetch` is itself one of the things under
  test.
- `location` and the base URL come from the harness, not from `compat/web`:
  a base URL is a property of the environment (a worker has one too), not an API
  the compat layer is missing.
- Results are judged **per subtest**. A `.any.js` file declares dozens of
  independent assertions; one unimplemented corner must not hide the rest.

## babeltest — Babel's fixture corpus

Babel is ~150 packages of ordinary, demanding JavaScript, and each fixture makes
it parse, transform and print a program whose expected output the Babel project
itself pins. A mismatch is a defect here, not an opinion.

The fixtures run against the **published `@babel/*` packages at the same version
as the pinned checkout** (`scripts/babel-suite-deps.sh` generates the dependency
list from the checkout, so the two cannot drift). That keeps the run hermetic
and reproducible while testing exactly the code Babel ships.

`babeltest/js/fixtures.js` reimplements Babel's fixture protocol against the
same rules its own helpers use — the options merge (root → suite → task), the
`throws` expectation, `BABEL_8_BREAKING`, the external-helpers plugin and its
load-bearing `helperVersion`, and babel-generator's separate parse-and-print
protocol. It runs INSIDE the runtime under test, which is the point: the merge,
the plugin resolution and the transform are all this engine doing real work.

One shard is one Babel package, run in one interpreter: that bounds the working
set and isolates a failure to a package.

### Known deviation: plugins are imported, not resolved by name

Babel resolves a plugin named in `options.json` by `require()`ing it first, and
every Babel 8 plugin is an ES module. Node ≥ 22 can `require()` a synchronous ES
module; this runtime cannot yet (see below), so the harness imports the plugins
itself and passes the objects — a supported programmatic form. That keeps each
fixture measuring the transform rather than Babel's config loader.

## Gaps these runs surfaced

Defects found by these suites are fixed in place, with a regression test named
for what it covers. The ones that are not yet closed:

- **`require()` of an ES module** (Node ≥ 22's `require(esm)`). The compat layer
  has no synchronous path from CJS into a module graph: the engine's dynamic
  import is asynchronous and the module loader cannot re-enter the interpreter.
  This is what makes Babel's own plugin loading unusable (worked around in the
  harness, above) and it will affect any package that `require()`s an
  ESM-only dependency.
- **Import attributes are not visible to the module loader.** The engine
  implements `with { type: "json" }` itself and JSON-parses whatever the loader
  returns, but the loader is not told which form the import used, so it cannot
  distinguish a JSON import from a JavaScript one and cannot report Node's
  `ERR_IMPORT_ATTRIBUTE_MISSING` for an attribute-less JSON import (it currently
  surfaces as a SyntaxError). See `jsonModuleSource` for how both cases are kept
  safe meanwhile.
- **A long single-process Node run stops making progress.** After some hundreds
  to thousands of tests, every goroutine is parked, the process sits at no CPU,
  and even a plain `time.After` in the harness does not fire. The tests involved
  are ones that block in a host call, which are abandoned at their deadline with
  their interpreter still live; the working theory is that enough of those
  exhaust something shared, but that is not yet demonstrated. Run the suite in
  shards (above) until it is understood.
- Engine-side items live in [engine-followups.md](engine-followups.md); compat
  API coverage lives in [compat-gaps.md](compat-gaps.md).
