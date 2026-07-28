# Plan: reach the level Bun and Deno are held to

The goal is not a number of our own choosing. Bun and Deno are measured against
the Node.js test suite and the Web Platform Tests, so those are the bar, and
[external-suites.md](external-suites.md) records what they actually run
(Deno: 3,411 of 3,786 registered Node tests; 26 WPT directories).

Everything below is ordered by MEASURED tests unlocked per unit of work, taken
from the runs in this repository rather than from intuition. Each item names the
number so the ordering can be re-checked when the numbers move.

## Where we start

| | today | bar |
|---|---|---|
| Node tests judged | ~2,100 of 4,883 | Deno runs 3,411 |
| Node tests passing | see `nodetest/expectations.json` | — |
| WPT subtests | 21,857 / 43,133 = 50.7% | Deno tracks 26 dirs, we track 21 |
| Babel fixtures | 4,170 / 4,189 = 99.55% | (already at parity with Node) |

## 1. Web APIs that live in the wrong layer — 323 WPT subtests

`CompressionStream`/`DecompressionStream` and `MessageChannel`/`MessagePort`
exist only under `compat/nodejs`, but both are WinterTC web APIs. `compression`
scores 0/303 and `webmessaging` 0/20 purely because they are not defined in
`compat/web`.

Smallest work in this document for the largest immediate gain, and it corrects
an architectural mistake rather than adding a feature.

## 2. The 409 quarantined hangs — 409 Node tests

`nodetest/quarantine.txt` lists tests that never finish. They are BUGS, not
missing features: the test's own logic completes and the runtime never decides
it is done, which points at reference counting in the event loop (a handle that
is never released, or an op counted twice).

These are worth attacking as a group rather than one at a time: the list is
dominated by a few families (`dgram`, `net`, `http`, `worker`, crypto streams),
so one ref-counting fix plausibly clears dozens. `NODETEST_QUARANTINE=only`
runs just this set and names every test that starts completing.

## 3. `process.execPath` — 648 Node tests

The single biggest skip bucket, and the main reason Deno runs 3,411 tests where
we judge ~2,100. Those tests re-execute the node binary to check flag handling,
exit codes, stdio and crash behaviour.

Deno passes them because the `deno` binary answers as a node-compatible
executable. The equivalent here is a host that re-enters this runtime: a
`child_process` spawn of `process.execPath` starts a NESTED interpreter (a fresh
instance on a goroutine, wired to pipes) instead of an OS process. The pieces
already exist — `spidermonkey.New` per instance, the agent machinery for
goroutine-hosted interpreters, and `Config.Exec` as the policy gate.

Note it is a real capability, not test scaffolding: tooling that shells out to
`node` (npm scripts, Next.js's own build steps) needs exactly this.

## 4. WebCryptoAPI validation order — ~3,700 WPT subtests

The suite asks for `SyntaxError` when key usages are wrong and `TypeError` for a
malformed algorithm; we answer `NotSupportedError` because support is checked
before the shape. Fixing the order in `compat/web`'s subtle entry points is
mechanical and the corpus is enormous (WebCryptoAPI is 35.9k of the 43.1k
subtests).

Careful: a share of the remaining WebCryptoAPI failures are genuinely
unimplemented algorithms, several of them `.tentative` (AES-OCB, Argon2). Those
are not in this item.

## 5. `URLPattern` — 809 WPT subtests

Not implemented at all (`urlpattern` scores 5/814). A self-contained,
spec-driven implementation with no host dependencies.

## 6. `data:` URLs in fetch — 152 WPT subtests

`fetch/data-urls` scores 2/154. Parsing and decoding a data URL is small and
entirely local.

## 7. `node:http2` — 276 Node tests

Go's `net/http2` provides the transport; the work is Node's `Http2Session` /
`Http2Stream` surface over it. Large, but it is the last big skip bucket after
execPath.

## Not on this list, and why

- `node:inspector`, `node:v8`, native addons, tests of Node's private internals:
  these cannot work here (SpiderMonkey is not V8; wasm cannot load native code),
  and the nodetest runner already classifies them as `Impossible` so they are
  excluded from the denominator rather than counted against it.
- WPT `css`, `wasm`, `service-workers`: no DOM, no WebAssembly in this engine
  build, no service worker scope.
- `websockets`, `webstorage`, `web-locks`, `eventsource`, `xhr`: real gaps, but
  each is a new subsystem rather than a fix, and none is on the critical path to
  the Node/WPT numbers above.
