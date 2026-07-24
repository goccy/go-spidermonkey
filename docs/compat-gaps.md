# Compat API gap closure — tracking

Goal: implement every missing core-module / global API that CAN be
implemented in this pure-Go, wasm-sandboxed runtime, with a per-module unit
test guaranteeing behavior (new AND previously-existing modules).

## Out of scope (genuinely impossible or nonsensical here) — with reason

- `repl` — needs an interactive TTY the sandbox has no access to.
- `inspector` (real) / `inspector/promises` — V8 debugger wire protocol;
  SpiderMonkey exposes nothing equivalent. Kept as load-only stub.
- `node:test` / `node:test/reporters` — a JS test runner; we test in Go.
- `wasi` — running a *second* wasm runtime inside the guest.
- `node:sea` — single-executable-application blob API; no host analogue.
- `trace_events` — V8 trace category plumbing.
- `domain` — deprecated, removed-in-spirit; load-only stub.
- `node:sqlite` — would embed a SQL engine in the guest; the Go-asset
  story (Env bindings / ops) is the sanctioned alternative.
- `vm` real context isolation — SpiderMonkey compartments are not exposed
  across the wasm bridge; `runInThisContext` (indirect eval) is provided,
  separate-realm variants throw.
- cfworkers `HTMLRewriter` — a large streaming HTML transformer; deferred.
- `cluster` — models forking the *whole process* to share a listening
  socket; there is no process to fork here. Load-only stub.

Note: `worker_threads` was previously listed here as impossible. That was
WRONG — Node's workers are separate isolates with their own heap that
communicate by structured-clone messaging (sharing only SharedArrayBuffer),
which is EXACTLY the engine's agent cluster (js.Agents(): goroutine thread +
separate realm + clone transport). It is now implemented as REAL preemptive
parallelism (see below).

## In scope — implementing

Legend: [x] done+tested  [ ] deferred

Status 2026-07-24: ALL in-scope items below implemented (incl. dgram + cfworkers) and covered by
per-module unit tests (compat/nodejs: crypto2_test, zlib_test, net_test,
fsextra_test, modules_test; compat/web: extended_test, subtle2_test).

Pure-JS (no host op):
- [x] alias modules: assert/strict, dns/promises, path/posix, path/win32,
      util/types (exists partially), sys, stream/consumers, readline/promises
- [x] console: table, time/timeEnd/timeLog, group/groupEnd, count/countReset,
      dir, dirxml, trace, assert (exists), clear
- [x] events statics: EventEmitter.on (async iterator), once (exists),
      getEventListeners, getMaxListeners/setMaxListeners, errorMonitor,
      captureRejectionSymbol
- [x] util: parseArgs, formatWithOptions, stripVTControlCharacters,
      styleText, MIMEType/MIMEParams, toUSVString, isArray, debug/debuglog
- [x] process: ref/unref, getBuiltinModule, hrtime (exists), constants
- [x] path: matchesGlob, toNamespacedPath
- [x] querystring: decode/encode aliases
- [x] os: constants, devNull, getPriority/setPriority
- [x] stream: static helpers (isReadable/isWritable/isErrored/isDisturbed,
      addAbortSignal, getDefaultHighWaterMark, Readable.toWeb/fromWeb)
- [x] timers/promises: setInterval (async gen), scheduler
- [x] buffer: Blob, File, isAscii, isUtf8, INSPECT_MAX_BYTES, transcode
- [x] error/constants tables: node:constants, os.constants, dns error codes

Web globals (no host op):
- [x] Blob, File, FormData
- [x] structuredClone (full: Map/Set/Date/RegExp/typed arrays/ArrayBuffer/
      cycles) replacing the JSON-limited version
- [x] TextEncoderStream / TextDecoderStream
- [x] CustomEvent, EventTarget option completeness
- [x] ReadableStream/Writable/Transform controller + reader classes exposed
      as globals; QueuingStrategy classes

Host-op-backed:
- [x] zlib: gzip/gunzip/deflate/inflate(+raw)/brotli (Go compress + brotli)
      + CompressionStream/DecompressionStream
- [x] crypto: createCipheriv/createDecipheriv (AES-GCM/CBC/CTR),
      createSign/createVerify (RSA/EC), pbkdf2(Sync), scrypt(Sync), hkdf,
      DiffieHellman, generateKeyPair(Sync), createHash streaming (exists),
      X509 (parse only)
- [x] crypto.subtle: encrypt/decrypt (AES-GCM/CBC, RSA-OAEP), deriveBits/
      deriveKey (ECDH, HKDF, PBKDF2), generateKey for AES, wrapKey/unwrapKey
- [x] http.request / http.get client (over Go net/http) + https
- [x] net: raw TCP Socket.connect + createServer (Go net, Dial/Listen)
- [x] fs: fd APIs (openSync/readSync/writeSync/closeSync/fstatSync),
      copyFile(Sync), rm(Sync), cp(Sync), mkdtemp(Sync), realpath,
      chmod/chown (no-op-ish), Stats/Dirent, ReadStream/WriteStream
- [x] dgram: UDP sockets (Go net UDPConn) — Dial/Listen gated

## Still MISSING — implementable, not yet done (audit 2026-07-24)

Presence audit found these are implementable in this runtime but are
currently stubs/absent. Completing the list above is NOT sufficient without
these:

- [ ] child_process: spawn/exec/execFile/fork over Go os/exec, gated by
      Config.Exec. Currently a throw-on-call stub. (Build tools, CLIs.)
- [ ] fs.watch / fs.watchFile — file watching (nodemon, chokidar, dev
      servers). Absent (the fs line above never shipped watch).
- [ ] tls: real TLS client + server. https.createServer currently ALIASES
      the plaintext http server — it does NOT do TLS. https client works
      (Go). TLS sockets and in-process HTTPS termination are missing.
- [ ] http server upgrade / 'connect' / Expect:100-continue / trailers —
      the Go net/http bridge does not expose connection upgrade, so a
      server-side WebSocket ('ws' server) cannot be built in the guest.
- [ ] objectMode streams — our streams are byte-only (strings coerced to
      Buffer). gulp/through2/csv-parse and any objectMode pipeline break.
- [ ] stream backpressure — Writable.write always returns true; no real
      highWaterMark. Large streams have no flow control (memory growth).
- [ ] streaming request/response bodies — http server buffers the whole
      request body before dispatch; fetch is synchronous under the hood.
      Large uploads/downloads and incremental streaming don't stream.
- [ ] crypto extras: publicEncrypt/privateDecrypt (RSA-OAEP in node:crypto),
      X509Certificate, KeyObject, DiffieHellman class, non-AES ciphers
      (chacha20-poly1305, des). subtle: RSA-OAEP encrypt, wrapKey/unwrapKey.
- [ ] encodings: Buffer utf16le/ucs2 in toString; TextDecoder for anything
      but utf-8 (latin1, utf-16le, ...) — needs tables or engine ICU.
- [ ] process.stdin reading (readline/inquirer interactive CLIs); readline
      Interface is a throwing stub.
- [ ] punycode/IDNA in URL (internationalized domains).

## Fidelity caveats — present but not behaviorally complete

Even where a module EXISTS, these semantic gaps can break real apps:

- async_hooks AsyncLocalStorage is naive: the store is a single slot held
  across a run()'s async lifetime. Correct only under serialized execution;
  concurrent interleaved contexts (APM/tracing, some frameworks) get the
  wrong store.
- process.nextTick does not strictly out-prioritize already-registered
  promise jobs (documented deviation); order-sensitive code can differ.
- worker_threads: worker code is self-contained — inside a worker only
  require('worker_threads'|'buffer') works (other node: ops are per-instance
  host functions on the main interpreter). piscina/jest/webpack workers that
  require fs/etc inside the worker won't run.
- process signals (SIGINT/SIGTERM handlers) never fire — graceful-shutdown
  code doesn't trigger. process.stdin/tty are inert.
- Full Intl/ICU is the engine's domain, not the compat layer.
- Native ESM edges (import.meta.url population, top-level await, dynamic
  import()) are unverified — need explicit tests.

Bottom line: presence of a module != an app runs. The only reliable
signal is running the real app (the flagship approach), which is why
Bun/Deno track compatibility with real test suites rather than a checklist.

worker_threads (real parallelism over js.Agents()):
- [x] Worker (eval + file), postMessage both ways, workerData, threadId,
      parentPort, 'online'/'message'/'exit'/'error', terminate(),
      SharedArrayBuffer sharing (Atomics across threads). Limitation:
      worker code is self-contained — inside a worker only require
      ('worker_threads'|'buffer') is available (other node: ops live on the
      main interpreter).

cfworkers:
- [x] scheduled + queue handler dispatch (Pool.Scheduled / Pool.Queue)
- [x] Cache API (in-memory caches.default / caches.open)
- [x] WebSocketPair (in-process pair; no external upgrade yet)
