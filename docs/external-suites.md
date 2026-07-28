# External conformance suites

test262 measures the ENGINE. It says nothing about `compat/nodejs`, `compat/web`
or `compat/cfworkers`, which are where a real application actually lands. These
three runs close that gap, each against the upstream project's own tests rather
than tests written here:

| suite | what it measures | run it |
|---|---|---|
| [nodetest](../nodetest) | `compat/nodejs`, against the Node.js project's own test suite | `make nodetest` |
| [wpt](../wpt) | `compat/web`, against the Web Platform Tests | `make wpt` |
| [babeltest](../babeltest) | the whole stack, against Babel's fixture corpus | `make babeltest` |
| [test262](../test262) | the engine (ECMA-262) | `make test262` |

`make suites` runs all four.

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

## wpt — the Web Platform Tests

Only the `.any.js` / `.worker.js` forms can run without a browser, and only the
directories whose APIs `compat/web` provides are in the default set
(`wpt/suite_test.go`, `DefaultDirs`) — a DOM-dependent directory would produce
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
- Engine-side items live in [engine-followups.md](engine-followups.md); compat
  API coverage lives in [compat-gaps.md](compat-gaps.md).
