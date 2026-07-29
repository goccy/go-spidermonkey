# Plan: reach the level Bun and Deno are held to

The goal is not a number of our own choosing. Bun and Deno are measured against
the Node.js test suite and the Web Platform Tests, so those are the bar, and
[external-suites.md](external-suites.md) records what they actually run
(Deno: 3,411 of 3,786 registered Node tests; 26 WPT directories).

Everything below is ordered by MEASURED tests unlocked per unit of work, taken
from the runs in this repository rather than from intuition.

## Where it stands

| suite | measured |
|---|---|
| WPT | **35,694 / 43,142 subtests = 82.7%** |
| Node.js | 2,575 tests run, **558 pass**; 235 quarantined as hangs |
| Babel | 4,170 / 4,189 fixtures = 99.6% |
| test262 | 52,266 / 53,329 = 98.0% |

WPT by directory, largest first:

| directory | passing |
|---|---|
| WebCryptoAPI | 83.8% — and **93.8% on the stable spec** (see below) |
| mimesniff | 98.6% |
| compression | 95.6% |
| url | 81.1% |
| fetch/data-urls | 78.6% |
| html/webappapis | 76.9% |
| FileAPI | 73.3% |
| dom/events | 80% |
| user-timing | 70.8% |
| fetch/api | 65.3% |
| encoding | 60.2% |
| streams | 56.1% |
| urlpattern | 54.9% |
| webmessaging | 42.9% |

WebCryptoAPI splits by whether the spec is stable:

| | subtests | passing |
|---|---|---|
| `.tentative.` files (draft algorithms) | 8,544 | 51.4% |
| the stable Web Crypto spec | 27,378 | **93.8%** |

The tentative set is ML-DSA, AES-OCB, KMAC, cSHAKE, X448, Ed448 and
ChaCha20-Poly1305 — draft algorithms that neither Node nor Deno implements
either. ML-KEM was the largest of them and is implemented (768 and 1024; Go has
no 512, and that is reported rather than faked).

## The Node gap, stated plainly

558 of 2,575 is 21.7%. Bun and Deno are far higher on the tests they run, and
the reason is not one missing subsystem — it is per-API fidelity across the
whole surface. The failure histogram, from a clean sharded run:

| tests | first failure |
|---|---|
| 627 | exited with a non-zero code (a mix; the largest single cause was a refused subprocess, now fixed) |
| 183 | `assert.throws` saw no exception |
| 144 | `assert.throws` saw the wrong error |
| 86 | a spawned process reported no status |
| 38 | `node:repl` is not implemented |
| 34 | a class was called without `new` |
| 31 | `fs.symlink` is not supported |
| 20 | `child_process.fork` has no IPC channel |

The two `assert.throws` rows — 327 tests — are one thing: Node reports a bad
argument with a CODE (`ERR_INVALID_ARG_TYPE`, `ERR_OUT_OF_RANGE`), and its own
suite matches on that code. Adding it to Buffer and to fs paths moved `test-fs-`
from 59 to 61 passing and left `test-buffer-` unchanged, because each of those
files checks dozens of argument shapes and one flipped assertion is not one
flipped file. Closing that row means comprehensive validation on every API, not
a few entry points. It is mechanical, it is large, and it is the single biggest
lever on the Node number.

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

### Node argument validation — 327 tests

See above. `fs` (65 files), `buffer` (27), `crypto` (25), `tls` (21) are the
biggest clusters.

### `node:http2` — 276 tests

Go's `net/http2` provides the transport; the work is Node's `Http2Session` /
`Http2Stream` surface over it.

### `child_process.fork` IPC — 164 tests

The nested-interpreter machinery exists (`compat/nodejs/nested.go`); what is
missing is the message channel and `process.send`/`'message'` on both sides.

### fetch/api — 530 subtests

Still the largest WPT directory gap after WebCryptoAPI's tentative set. What
remains is spread thin: preflight cache semantics, upload streaming, a handful
of unported wptserve handlers.

### `node:repl` — 38 tests

### `fs.symlink` — 31 tests

## Not on this list, and why

- `node:inspector`, `node:v8`, native addons, tests of Node's private internals:
  these cannot work here (SpiderMonkey is not V8; wasm cannot load native code),
  and the nodetest runner classifies them as `Impossible` so they are excluded
  from the denominator rather than counted against it.
- WPT `css`, `wasm`, `service-workers`: no DOM, no WebAssembly in this engine
  build, no service worker scope.
- `websockets`, `webstorage`, `web-locks`, `eventsource`, `xhr`: real gaps, but
  each is a new subsystem rather than a fix.
