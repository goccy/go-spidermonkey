# Plan: reach the level Bun and Deno are held to

The goal is not a number of our own choosing. Bun and Deno are measured against
the Node.js test suite and the Web Platform Tests, so those are the bar, and
[external-suites.md](external-suites.md) records what they actually run
(Deno: 3,411 of 3,786 registered Node tests; 26 WPT directories).

Everything below is ordered by MEASURED tests unlocked per unit of work, taken
from the runs in this repository rather than from intuition. Each item names the
number so the ordering can be re-checked when the numbers move.

## Done so far

| item | was | now |
|---|---|---|
| 1. Web APIs in the wrong layer | compression 0%, webmessaging 0% | 74%, 39% |
| 4. Web Crypto validation and key agreement | WebCryptoAPI 46% | 80%+ |
| 5. URLPattern | 0.6% | 49% |
| 6. `data:` URLs in fetch | 1.3% | 79% |
| (found on the way) media types | mimesniff 13% | 99% |
| 3. `process.execPath` | 648 tests skipped | runs as a nested interpreter |
| 2. quarantined hangs | 477 never finished | see "The hangs" below |

WPT total over the same period: **45.8% -> 63.8% -> higher**, measured over a
corpus that also grew from 40,888 to 43,065 subtests.

## The hangs: what the event loop was holding

`nodetest/quarantine.txt` listed 477 tests that never finish. Diagnosing them
one at a time was hopeless while the only evidence was `3 pending host op(s)`,
so `Loop.Alive()` now names the KIND of handle held — `net.conn`, `net.server`,
`http`, `tls`, `dgram`, `worker`, `stdio`, `http.client`. That turned the list
into a histogram, and the histogram into four causes:

1. **A connected socket never started reading.** Node calls `self.read(0)` in
   `afterConnect`, which is what makes libuv start reading; without it a peer's
   FIN on an otherwise-idle connection was never seen, so `'end'`/`'close'`
   never fired and the socket stayed open forever.
2. **A stream that finished was never destroyed.** `'close'` was emitted when
   both halves completed, but `_destroy` never ran — so the host connection (and
   the loop handle with it) was never released. Node's `autoDestroy` destroys
   the stream and emits `'close'` from that; sockets now do the same.
3. **Only the first resolved address was dialled.** `localhost` resolves to
   `::1` and `127.0.0.1`, and a loopback server bound to one refuses the other.
   Node tries them in turn (`autoSelectFamily`); so does every dial path here.
4. **An unhandled exception was reported and execution continued.** Node's
   contract is that an uncaught exception with no `'uncaughtException'` listener
   prints and exits 1. Carrying on left every handle the program had opened
   holding the loop, so any test that threw simply never terminated. This was
   the single largest cause.

A fifth, narrower one: an https request's own TLS settings
(`rejectUnauthorized`, `ca`, `servername`) never reached the host transport, so
every request to a self-signed server failed the handshake — which is how
essentially every https test is set up.

## Where the remaining WPT gap is

WebCryptoAPI is 35.9k of the 43.1k subtests, so it dominates the total. Split
by whether the spec is stable:

| | subtests | passing |
|---|---|---|
| `.tentative.` files (draft algorithms) | 8,544 | 39% |
| the stable Web Crypto spec | 27,378 | **93%** |

The tentative set is ML-KEM, ML-DSA, AES-OCB, KMAC, cSHAKE, X448, Ed448 and
ChaCha20-Poly1305 — draft algorithms that neither Node nor Deno implements
either. ML-KEM is now implemented for the two parameter sets Go's `crypto/mlkem`
provides (768 and 1024; there is no 512, and it reports that rather than faking
it), which is the largest of them.

## Remaining, ordered by measured tests

### `node:http2` — 276 Node tests

Go's `net/http2` provides the transport; the work is Node's `Http2Session` /
`Http2Stream` surface over it. The last big skip bucket after execPath.

### `child_process.fork` IPC — 164 Node tests

`fork()` needs an IPC channel between parent and child. The nested-interpreter
machinery already exists (`compat/nodejs/nested.go`); what is missing is the
message channel and `process.send`/`'message'` on both sides.

### Individual API gaps surfaced by the hang triage

Each is small, and each was found by reading what a hanging test actually
printed rather than by guessing:

- `net.connect({ lookup })` — a caller-supplied resolver.
- `req.addTrailers` on the server request object.
- `server.on('connection')` for http/https servers: the Go `http.Server` owns
  the connection, so the guest never sees a socket to track or destroy. Tests
  that shut down by destroying tracked connections cannot terminate.
- IPv6 loopback listen/connect paths.

### Streams — WPT `streams` is at 48%

A whole-subsystem gap rather than a set of fixes.

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
