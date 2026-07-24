# Compat API gap closure — status

Goal: implement every missing core-module / global API that CAN be
implemented in this pure-Go, wasm-sandboxed runtime, with per-module unit
tests. This tracks what is done, what remains by necessity, and the honest
fidelity caveats.

## Genuinely out of scope (impossible or nonsensical here) — with reason

- `repl` — needs an interactive TTY the sandbox has no access to.
- `inspector` (real) / `inspector/promises` — V8 debugger wire protocol;
  SpiderMonkey exposes nothing equivalent. Load-only stub.
- `node:test` / `node:test/reporters` — a JS test runner; we test in Go.
- `wasi` — running a *second* wasm runtime inside the guest.
- `node:sea` — single-executable-application blob API; no host analogue.
- `trace_events` — V8 trace category plumbing.
- `domain` — deprecated; load-only stub.
- `node:sqlite` — would embed a SQL engine in the guest; the Go-asset story
  (Env bindings / ops, e.g. database/sql) is the sanctioned alternative.
- `vm` real context isolation — SpiderMonkey compartments are not exposed
  across the wasm bridge; runInThisContext (indirect eval) works,
  separate-realm variants throw.
- `cluster` — models forking the whole process to share a listening socket;
  there is no process to fork. Load-only stub.
- cfworkers `HTMLRewriter` — a large streaming HTML transformer; deferred.

## Implemented (with unit tests)

Core modules & globals: path(+posix/win32), events(+on async iterator/
statics), util(parseArgs/styleText/MIMEType/inspect/promisify), buffer
(utf8/hex/base64/base64url/latin1/utf16le/ucs2), process(env/argv/nextTick/
stdin/signals/hrtime/ref), os(+constants), querystring, url, timers(+promises),
assert(+strict), stream(objectMode + real backpressure + consumers/web),
string_decoder, console(group/count/table/time), constants, module (as the
Module class), punycode (real RFC 3492 + IDNA), readline, async_hooks
(AsyncLocalStorage + AsyncResource propagation).

Host-op-backed:
- fs: read/write/stat/readdir/mkdir/rm/cp/copyFile/mkdtemp/rename, fd APIs,
  createReadStream/createWriteStream, watch/watchFile (mtime polling),
  Stats/Dirent. Access control lives in Config.FS itself (a policy FS — e.g.
  a sheena Volume via a small adapter — enforces its own rules in its
  Open/OpenFile/… methods); writes need WritableFS.
- crypto: hash/hmac, AES-GCM/CBC/CTR + ChaCha20-Poly1305 cipheriv, sign/verify
  (RSA/ECDSA), pbkdf2/scrypt/hkdf, generateKeyPair, publicEncrypt/
  privateDecrypt (RSA-OAEP/PKCS1), DiffieHellman, X509Certificate parse,
  randomBytes/randomInt.
- crypto.subtle: digest, HMAC, ECDSA, RSA sign/verify, AES-GCM/CBC/CTR
  encrypt/decrypt, RSA-OAEP encrypt/decrypt, ECDH/HKDF/PBKDF2 deriveBits/
  deriveKey, wrapKey/unwrapKey. (JWS + JWE capable.)
- zlib: gzip/deflate/inflate(+raw)/brotli — sync/async/stream +
  CompressionStream/DecompressionStream.
- http: server over Go net/http with STREAMING request bodies; http.request/
  get client; https client. child_process: spawn/exec/execFile(+sync) over Go
  os/exec, gated by Config.Exec. net: raw TCP client+server. dgram: UDP.
  tls: connect + createServer; https.createServer with REAL TLS termination.
- worker_threads: real preemptive workers over js.Agents() (separate realm +
  goroutine + structured-clone messaging + SharedArrayBuffer).

Web globals: fetch (Resolve/Dial gated), Blob/File/FormData, full
structuredClone, URL/URLSearchParams, TextEncoder/Decoder(utf-8/latin1/
utf-16le) + streams, ReadableStream/WritableStream/TransformStream +
QueuingStrategy, AbortController, CustomEvent, performance, atob/btoa.

Web/Workers: compat/cfworkers fetch/scheduled/queue handlers, Env bindings,
Cache API, WebSocketPair.

## Known remaining gaps (would still affect some pure-JS apps)

Implementable, deferred for size:
- Server-side WebSocket upgrade (http 'upgrade' / hijack + raw duplex socket).
  A guest 'ws' server cannot be built yet. Client-side WS also absent.
- TextEncoderStream/TextDecoderStream cover utf-8 only for streaming decode.
- Encodings beyond utf-8/latin1/utf-16le (needs engine ICU).

Fidelity caveats (present but not behaviorally complete):
- async_hooks: AsyncLocalStorage is correct for synchronous run(), nested
  run(), and EXPLICIT propagation via AsyncResource.bind/runInAsyncScope /
  ALS.snapshot (the pattern tracing libraries use). BARE `await` interleaving
  across independent contexts on ONE instance still cannot be tracked without
  engine async-context hooks. Note: the cfworkers pool gives each request its
  own instance, where ALS is fully correct.
- process.nextTick does not strictly out-prioritize already-registered promise
  jobs (documented deviation); order-sensitive code may differ.
- worker_threads: worker code is self-contained — inside a worker only
  require('worker_threads'|'buffer') works (other node: ops are per-instance
  host functions on the main interpreter).
- Full Intl/ICU is the engine's domain, not the compat layer.

## Bottom line

Module presence is necessary but not sufficient — the reliable signal is
running the real app (jose, Hono, Express, Next.js all pass as flagships).
The remaining gaps are enumerated above rather than hidden.
