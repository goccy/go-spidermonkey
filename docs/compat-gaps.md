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
- Real preemptive `worker_threads` / `cluster` — the engine is single
  linear-memory per instance; the Agents cluster is message-passing, not
  shared-heap threads. worker_threads stays a main-thread-only surface
  (isMainThread true) + throwing Worker ctor.

## In scope — implementing

Legend: [ ] todo  [x] done+tested

Pure-JS (no host op):
- [ ] alias modules: assert/strict, dns/promises, path/posix, path/win32,
      util/types (exists partially), sys, stream/consumers, readline/promises
- [ ] console: table, time/timeEnd/timeLog, group/groupEnd, count/countReset,
      dir, dirxml, trace, assert (exists), clear
- [ ] events statics: EventEmitter.on (async iterator), once (exists),
      getEventListeners, getMaxListeners/setMaxListeners, errorMonitor,
      captureRejectionSymbol
- [ ] util: parseArgs, formatWithOptions, stripVTControlCharacters,
      styleText, MIMEType/MIMEParams, toUSVString, isArray, debug/debuglog
- [ ] process: ref/unref, getBuiltinModule, hrtime (exists), constants
- [ ] path: matchesGlob, toNamespacedPath
- [ ] querystring: decode/encode aliases
- [ ] os: constants, devNull, getPriority/setPriority
- [ ] stream: static helpers (isReadable/isWritable/isErrored/isDisturbed,
      addAbortSignal, getDefaultHighWaterMark, Readable.toWeb/fromWeb)
- [ ] timers/promises: setInterval (async gen), scheduler
- [ ] buffer: Blob, File, isAscii, isUtf8, INSPECT_MAX_BYTES, transcode
- [ ] error/constants tables: node:constants, os.constants, dns error codes

Web globals (no host op):
- [ ] Blob, File, FormData
- [ ] structuredClone (full: Map/Set/Date/RegExp/typed arrays/ArrayBuffer/
      cycles) replacing the JSON-limited version
- [ ] TextEncoderStream / TextDecoderStream
- [ ] CustomEvent, EventTarget option completeness
- [ ] ReadableStream/Writable/Transform controller + reader classes exposed
      as globals; QueuingStrategy classes

Host-op-backed:
- [ ] zlib: gzip/gunzip/deflate/inflate(+raw)/brotli (Go compress + brotli)
      + CompressionStream/DecompressionStream
- [ ] crypto: createCipheriv/createDecipheriv (AES-GCM/CBC/CTR),
      createSign/createVerify (RSA/EC), pbkdf2(Sync), scrypt(Sync), hkdf,
      DiffieHellman, generateKeyPair(Sync), createHash streaming (exists),
      X509 (parse only)
- [ ] crypto.subtle: encrypt/decrypt (AES-GCM/CBC, RSA-OAEP), deriveBits/
      deriveKey (ECDH, HKDF, PBKDF2), generateKey for AES, wrapKey/unwrapKey
- [ ] http.request / http.get client (over Go net/http) + https
- [ ] net: raw TCP Socket.connect + createServer (Go net, Dial/Listen)
- [ ] fs: fd APIs (openSync/readSync/writeSync/closeSync/fstatSync),
      copyFile(Sync), rm(Sync), cp(Sync), mkdtemp(Sync), realpath,
      chmod/chown (no-op-ish), symlink/readlink, Stats/Dirent classes,
      ReadStream/WriteStream classes, watch (poll or unsupported)
- [ ] dgram: UDP sockets (Go net UDPConn) — Dial/Listen gated

cfworkers:
- [ ] scheduled + queue handler dispatch
- [ ] Cache API (in-memory caches.default / caches.open)
- [ ] WebSocketPair (in-process pair; no external upgrade yet)
