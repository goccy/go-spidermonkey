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
| Node.js | 2,611 tests run, **766 pass = 29.3%** (see the note below) |
| Babel | 4,170 / 4,189 fixtures = 99.6% |
| test262 | 52,266 / 53,329 = 98.0% |

The Node figure is a full sharded sweep, and it is the number to quote. It
lags HEAD slightly: a sweep takes about half an hour, so anything committed
after its binary was built is measured per module instead — the callable
constructors (`Buffer` 22 → 25) and the HTTP response validation (`http`
83 → 85) are the two currently in that position. Re-run the sweep before
quoting a new total rather than adding module deltas to an old one.

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
  covered before a file flips. Doing that for `fs` moved it from 66 to 124
  passing, and for `Buffer` from 11 to 22.

Per-module results from that pass, each measured with `NODETEST_FILTER` before
and after:

| module | before | after |
|---|---|---|
| `fs` | 66 | 124 |
| `fs.cp` alone | 30 | 58 |
| `Buffer` | 11 | 22 |
| `repl` | 8 | 20 |
| `process` | 21 | 27 |
| `whatwg-url` | 9 | 16 |
| `vm` | 31 | 34 |
| `zlib` | 7 | 11 |
| `crypto` | 28 | 31 |

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

The mechanical pass above covered `fs`, `Buffer`, `zlib`, `vm`, `process`,
`net` and `URLSearchParams`. `crypto` (50 remaining), `tls` and `child_process`
still report the wrong error or none for a bad argument.

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
