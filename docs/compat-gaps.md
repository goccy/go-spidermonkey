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
      chmod/chown (no-op-ish), symlink/readlink, Stats/Dirent classes,
      ReadStream/WriteStream classes, watch (poll or unsupported)
- [x] dgram: UDP sockets (Go net UDPConn) — Dial/Listen gated

cfworkers:
- [x] scheduled + queue handler dispatch (Pool.Scheduled / Pool.Queue)
- [x] Cache API (in-memory caches.default / caches.open)
- [x] WebSocketPair (in-process pair; no external upgrade yet)
