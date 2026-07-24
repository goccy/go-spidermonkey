# Node.js-Compatible Runtime on go-spidermonkey — Implementation Plan

Status: in progress (2026-07-23).

- Phase 0 landed: Config permission hooks, WritableFS + memfs, composable
  module resolvers.
- Phase 1 landed: compat/internal/eventloop; compat/web vocabulary
  (console, encodings, URL, AbortController, timers, Headers/Request/
  Response), fetch with per-request Resolve/Dial enforcement, and
  crypto.subtle (digest, HMAC, ECDSA, RSASSA-PKCS1-v1_5/RSA-PSS; raw/jwk/
  pkcs8/spki) — the JWS surface, cross-checked against Go crypto. Still
  open: running the real jose package (needs the examples/npm harness),
  WPT subset in CI, async fetch via the loop (today's fetch is synchronous
  under the hood), unifying fetch's Go-composed response with the JS
  Response class.
- Phase 2 landed: compat/cfworkers — warmed instance pool as http.Handler,
  module boot via the cfworkers: resolver, Env bindings (Static data and
  Go-function bindings), ctx.waitUntil drained before pool reuse,
  concurrency proven by test. Still open: database/sql Env binding demo,
  fresh-per-request mode.
- FLAGSHIPS ACHIEVED (2026-07-23): the unmodified **jose** package signs
  and verifies HS256/ES256 JWTs (HS256 signature cross-checked byte-exact
  against Go's HMAC) — compat/web flagship. The unmodified **Hono**
  framework serves routes/middleware/params/env-bindings through the pool
  — compat/cfworkers flagship. Both run via examples/{jose,hono} (`npm ci`
  there; tests skip when node_modules is absent) on compat/nodejs's
  ESMLoader (bare-specifier resolution over exports maps; a bare specifier
  answers with a re-export shim so the real file registers under its own
  path and its relative imports resolve — the engine pre-joins relative
  specifiers against the REGISTERED specifier, not the file's location).
- Phase 3 landed (2026-07-24): `nodejs.Install` — the Node runtime proper.
  Core modules (path, events, util, buffer, fs(+promises), querystring,
  os, url, timers(+promises), assert, process; child_process as a
  throw-on-call stub until Phase 5); Buffer as a Uint8Array subclass;
  process with env/argv/platform/stdout/nextTick; CommonJS require with
  the full resolution algorithm in Go (node_modules walk-up, exports maps
  with require/import condition sets, "#" imports, extension ladder,
  package "type" + source-sniff CJS/ESM classification); ESM⇄CJS interop
  (CJS surfaces as default export via a require-backed shim); setImmediate
  (check phase in the loop); nextTick implemented as a self-scheduling
  engine microtask — ticks run before promise jobs REGISTERED AFTER them
  and before any macrotask, but a tick queued before an already-registered
  promise job does not jump it, and ticks queued by a promise run after
  the current promise batch (documented deviation from Node's strict
  priority; fixing it needs an engine job-queue hook).
  Phase 3 FLAGSHIPS PASSED: unmodified **lodash** 4.18 (CJS require and
  ESM default import), **chalk** 5.6 (pure ESM, #-imports, ANSI output
  under a forced level), **commander** 12 (dual: esm.mjs wrapper over the
  CJS build through the interop shim). Via examples/nodejs (`npm ci`).
  Still open in Phase 3: fs callback flavors run on the microtask queue
  (not the poll phase), no cycle-tolerant partial CJS exports test,
  ref/unref, node:module/createRequire.
- Phase 4 landed (2026-07-24): node:stream (compact Readable/Writable/
  Duplex/Transform/PassThrough with pipe/pipeline/finished, sized for the
  Express tree), node:string_decoder (function-style constructor —
  iconv-lite `.call()`s it), fs.createReadStream/createWriteStream,
  node:crypto (createHash/createHmac over Go crypto: md5/sha1/sha256/384/
  512; randomBytes; timingSafeEqual), node:tty, node:zlib (load-only
  stub), node:net (isIP helpers; raw sockets still open), legacy
  url.parse/format, Error.captureStackTrace shim, and **node:http built
  directly over Go net/http** (no node:net layer — the Bun approach): Go
  goroutines own accept/parse/keep-alive, each request posts a dispatch
  onto the event loop and blocks until the guest ends the response via
  http_respond/write/end ops; Server/IncomingMessage/ServerResponse in JS;
  listen gated by Config.Listen; a listening server holds the loop alive.
  Resolution additions: string "browser" field honored (depd/debug resolve
  to sandbox-safe builds), non-string package.json fields tolerated
  ("main": false in math-intrinsics).
  Phase 4 FLAGSHIP PASSED: unmodified **Express 4.22** — routing, params,
  query, middleware chain, express.json body parsing (body-parser →
  raw-body → iconv-lite over our streams), ETag via node:crypto sha1,
  If-None-Match → 304, throwing route → 500, custom 404. Landmines
  recorded: Go keeps Content-Length in r.Header (don't re-add — "7, 7"
  breaks type-is), body-parser treats req._body as its parsed flag (never
  use that name on IncomingMessage).
  Still open in Phase 4: http.request client, node:net raw sockets,
  serve-static/sendFile demo, fastify stretch target.
- STRETCH FLAGSHIP PASSED (2026-07-24): the unmodified **Next.js 14.2**
  production server (pages router; app built on host Node — SWC natives
  are build-time only) serves SSR pages, SSG pages, API routes and .next
  static assets via the custom-server API (examples/nextjs). What it
  forced: Module as a real class so Next's require-hook patches work,
  function-style ctors for EventEmitter/streams/StringDecoder (legacy
  `.call(this)` packages), V8 captureStackTrace + prepareStackTrace
  protocol, WHATWG Writable/TransformStream + async ReadableStream reads,
  Readable 'end' only after a consumer exists, naive-but-serialized
  AsyncLocalStorage, fs.Stats duck-type fields, Timeout objects with
  unref, file: URL handling, and a raft of load-only stubs (v8, vm,
  inspector, worker_threads, dns, tls, http2, ...). All additions are
  REAL node: core-module names — the strategy stays demand-driven
  subsetting of Node's stdlib, not invention beyond it.

This document is the working plan for building
Node.js / Web / Cloudflare-Workers compatible runtimes on top of the
go-spidermonkey engine. It records the design decisions already made and
breaks the work into phases small enough to land as reviewable PRs.

## 1. Vision and positioning

Build JS runtime environments as **opt-in compat packages** layered on the
engine, rather than a monolithic runtime binary:

- Pure-Go, CGO-free, single-static-binary runtimes — embeddable in any Go
  application and cross-compilable everywhere (`go build` is the whole story).
- Every syscall-shaped capability (filesystem, network, subprocess) flows
  through Go host functions ("ops"), so `spidermonkey.Config` is a single
  enforcement point for Deno-style fine-grained permissions. The wasm
  sandbox guarantees there is no other exit: a capability that was not
  explicitly exposed does not exist.
- Go's ecosystem (`net/http`, `database/sql`, `crypto`, ...) replaces the
  role N-API native addons play in Node — exposed to JS as ops, under the
  same permission model.
- Server-side model: goroutine-per-request with a pool of warmed engine
  instances (`sql.DB`-style), not a single event loop multiplexing sockets.
  Go's `net/http` owns accept/TLS/HTTP2; JS sees Request → Response.

## 2. Architecture

```
┌──────────────────────────────────────────────────────┐
│  Application (Go)         /  CLI front-end (later)   │
├──────────────────────────────────────────────────────┤
│  compat/cfworkers   export default { fetch }, Env    │
│                     bindings, instance pool,          │
│                     http.Handler adapter              │
├──────────────────────────────────────────────────────┤
│  compat/nodejs      node: builtins, CJS require,      │
│                     node_modules resolution, Buffer,  │
│                     full event loop (nextTick, ...)   │
├──────────────────────────────────────────────────────┤
│  compat/web         WinterTC Minimum Common API:      │
│                     fetch, URL, TextEncoder, streams,  │
│                     crypto, timers, console            │
├──────────────────────────────────────────────────────┤
│  engine core (this repo's root package)               │
│  JS/Object/Value, RunJobs, Agents, bytes bridge,      │
│  composable module resolvers, Config permissions      │
└──────────────────────────────────────────────────────┘
```

Dependency rule: `cfworkers → web`, `nodejs → web`, everything → core.
`compat/web` provides the *vocabulary* (globals); `compat/cfworkers`
provides a *programming model* (module convention + lifecycle + Go-side
adapter); `compat/nodejs` provides the npm ecosystem surface.

### Package layout

```
compat/
  web/          web.Install(js *spidermonkey.JS, opts ...web.Option) error
  cfworkers/    cfworkers.NewPool(cfg PoolConfig) (*Pool, error)  // http.Handler
  nodejs/       nodejs.Install(js *spidermonkey.JS, opts ...nodejs.Option) error
  internal/
    eventloop/  shared macro-task loop (timers, I/O completions, nextTick)
    jsembed/    helpers to evaluate embedded JS builtins
memfs/          writable in-memory FS (WritableFS impl) for tests/isolation
```

Registration is **explicit** (`Install(js)`), not blank-import + global
registry: it returns errors, takes options, keeps ordering visible, and
binary-size benefits are identical (unimported packages don't link).

### Design decisions (already settled)

1. **Thin-core / JS-builtins split (Deno-style).** The wasm2go boundary is
   the cost center, so ops are coarse-grained (one round-trip per logical
   operation), bulk data always rides the bytes bridge
   (`NewBytes`/`Bytes`), and API shaping lives in embedded JS.
2. **Permissions ported from go-python** (`go-python/interpreter.go`
   Config): callback allow-lists, zero-value = fully sandboxed. Same field
   names and signatures where the domain matches, so goccy runtimes share
   one permission API. Difference: go-python enforces at the WASI boundary;
   here enforcement lives inside the ops themselves (SpiderMonkey has no
   I/O of its own, so there is nothing to intercept — ops consult
   `Config` hooks directly, which every host `Func` already receives).
3. **`Config.FS` stays `io/fs.FS`; writes via interface upgrade.** A
   `WritableFS` interface (OpenFile/Mkdir/Remove/Rename) is asserted by
   ops that need writes; plain `fs.FS` values behave as a read-only
   filesystem (writes fail with EROFS). Mirrors `fs.ReadDirFS` idiom.
4. **Composable module resolvers.** `SetModuleLoader` is a single slot;
   compat packages must be able to claim specifier namespaces
   (`node:`, ...) without clobbering each other or the user's fallback
   loader. Core gains `RegisterModuleResolver(prefix, loader)` with
   longest-prefix-wins dispatch; `SetModuleLoader` becomes the ""-prefix
   fallback (backward compatible).
5. **Two execution modes.** (a) Embedded-handler mode: instance pool, no
   long-lived event loop, drains jobs per request (cfworkers). (b) Runtime
   mode: full Node event loop owned by `compat/nodejs` (timers, nextTick
   before promise jobs, setImmediate, ref/unref) for long-lived scripts.
   The event loop is a compat-layer concern; core stays loop-free.
6. **Workers `env` is the injection point for Go assets.** `database/sql`
   handles, caches, arbitrary host services are passed as Env bindings —
   the sanctioned way to expose Go libraries to JS, always via ops.

## 3. Phases

### Phase 0 — Core: permissions + composable resolvers  ← current

Everything later builds on this; it is engine-core work in the root
package.

1. `Config` gains permission hooks (names/signatures from go-python where
   the domain matches):
   - `FSAccess func(path string, write bool) bool` — every file
     open/create/unlink by ops **and by the default module loader**.
   - `Dial func(network, ip string, port int) bool` — outbound connects.
   - `Resolve func(host string) bool` — name resolution.
   - `Listen func(network, addr string) bool` — inbound sockets
     (go-python's op-level `NetAccess` reshaped: sockets here are wholly
     Go-side, so gate at listen/dial/resolve instead of accept/recv/send).
   - `Exec func(path string, argv []string) bool` — subprocess spawns.
   nil = allow (matching go-python); zero-value Config stays fully
   sandboxed because no ops exist until a compat package installs them.
2. `WritableFS` interface + EROFS behavior contract; `memfs` package with
   a small writable in-memory implementation (test double + multi-tenant
   isolation story, analogous to go-python's `NewStdlibMemFS`).
3. `RegisterModuleResolver(prefix string, loader ModuleLoader)` on `*JS`;
   longest-prefix dispatch in `hostEnv.dispatchModuleLoad`;
   `SetModuleLoader` re-documented as the fallback ("" prefix). Default
   FS loader consults `FSAccess`.

Acceptance: unit tests — FSAccess denial surfaces as a module-load error;
prefixed resolver wins over fallback; fallback intact when no prefix
matches; memfs round-trips through WritableFS.

### Phase 1 — compat/web (WinterTC vocabulary)

- Globals implemented as embedded JS over ops: `TextEncoder/TextDecoder`,
  `URL/URLSearchParams`, `atob/btoa`, `structuredClone` (JSON-limited at
  first), `queueMicrotask`, `performance.now`, `console` (Config stdio),
  `crypto.getRandomValues/randomUUID` (subtle later),
  `setTimeout/setInterval` (needs the minimal event-loop pump),
  `AbortController/AbortSignal`.
- `fetch` + `Request/Response/Headers`: Go op over `net/http`; bodies over
  the bytes bridge; enforcement via `Dial`/`Resolve`. The existing
  fetch proof (fetch_test.go) is the seed.
- Streams: start with byte-oriented `ReadableStream` sufficient for
  Response.body; full WHATWG streams are a later milestone.
- `compat/internal/eventloop`: actor loop owning the instance — run JS →
  drain jobs (`RunJobs`) → `select` on timer heap / completion channel.
  Used by web timers; extended by nodejs in Phase 3.

Acceptance: WinterTC smoke tests per API; fetch against httptest with
Dial/Resolve allow-lists exercised; flagship: **jose** signs and
verifies a JWT (needs crypto.subtle — may close late in the phase).

### Phase 2 — compat/cfworkers (server model)

- Module convention: evaluate user module, capture `export default`
  handlers (`fetch` now; struct leaves room for `scheduled`/`queue`).
- `Pool` implementing `http.Handler`: N warmed instances, checkout per
  request, Go `net/http` Request ⇄ JS Request/Response conversion over
  the bytes bridge.
- `Env` bindings: named Go objects exposed per-instance at install;
  the sanctioned Go-asset injection point.
- `ctx.waitUntil`: response returns immediately; the instance is drained
  (RunJobs until pending promises settle) before returning to the pool.
- Modes: pooled-warm (state persists, documented) and fresh-per-request.

Acceptance: `ListenAndServe` demo serving a worker script; concurrent
load test (pool checkout correctness, no cross-request state bleed in
fresh mode); waitUntil completes after response. Early smoke:
**itty-router**; flagship: **Hono** app (routing + middleware + JSON)
served through the pool, with a `database/sql`-backed Env binding.

### Phase 3 — compat/nodejs (module system + first builtins)

- `node:` resolver registered via Phase 0 API; builtins embedded via
  `embed.FS`.
- Pure-JS builtins first: `node:path`, `node:events`, `node:util`,
  `node:querystring`, `node:url` (share URL with compat/web). Borrow from
  Node lib/ or Deno node-compat (both MIT) where it saves rewriting
  `stream`/`url`-class complexity.
- `Buffer` as `Uint8Array` subclass in JS; utf8/base64/hex hot paths as
  ops on the bytes bridge.
- `node:fs` (+`fs/promises`) over `Config.FS`/`WritableFS` with `FSAccess`
  enforcement; sync variants are free (host calls are synchronous).
- `process` (argv/env/cwd/exit/nextTick), full event loop semantics in
  eventloop: **nextTick queue drains before engine promise jobs after
  every callback**, `setImmediate` after poll, ref/unref counting for
  loop liveness.
- CJS: `require` in JS (wrap in `(function (exports, require, module,
  __filename, __dirname))`, cache); resolution algorithm in Go (shared
  with ESM): node_modules walk-up, `package.json` `exports`/`imports`/
  conditions/`main`, extension resolution; ESM⇄CJS interop per current
  Node (require(esm) supported, default-synthesis for CJS imports).

Acceptance: run real pure-JS npm packages from node_modules —
**lodash** (CJS), **commander** + **chalk** (CLI surface, ESM/CJS mix);
a script mixing CJS and ESM; nextTick/promise/setImmediate ordering
tests mirroring Node's documented semantics.

### Phase 4 — servers: stream, net, http

- `node:stream` (port, don't rewrite), `node:net` over Go net + Dial/
  Listen hooks, `node:http` server+client. Flagship gate: **Express 4**
  hello-world + middleware + router. Stretch (post-phase): **fastify**.

### Phase 5 — long tail

- `node:crypto` (Go stdlib covers most), `worker_threads` on Agents
  (needs structured clone + MessagePort semantics), `child_process` over
  `os/exec` + `Exec` hook, `node --version`-style CLI front-end if wanted.
- Compat measurement in CI: subset of Node's own test suite (the
  Bun/Deno approach), plus WPT subset for compat/web.

## 4. Flagship compatibility targets

Each compat package has a real, unmodified, published application or
framework whose working demo (examples/ + integration test) is the
definition of done for that layer. "The flagship runs" is the compat
claim; unit tests alone don't earn it.

### compat/web — jose (flagship), plus fetch-vocabulary smoke apps

- **jose** (JWT/JWS/JWE, pure JS, zero deps) is built exclusively on the
  WinterTC vocabulary — WebCrypto (`crypto.subtle`), `TextEncoder`,
  `Uint8Array`, `atob/btoa` — and officially supports every WinterCG
  runtime. Signing and verifying a JWT with jose proves the crypto/
  encoding surface end to end.
- Secondary: a plain `fetch` + `URL` + `AbortController` script against
  `httptest`, and later a WPT subset in CI.

### compat/cfworkers — Hono (flagship), itty-router (early smoke)

- **Hono** is the canonical web-standard framework: zero dependencies,
  `export default app` with a `fetch` handler, officially targets
  Cloudflare Workers/Deno/Bun. A Hono app with routing + middleware +
  JSON served through `cfworkers.NewPool` behind `net/http` is the
  Phase 2 definition of done. Env bindings demo: a Hono route reading
  from a `database/sql`-backed binding (the Go-assets story, live).
- **itty-router** (~1 kB Workers router) is the early smoke test while
  streams support is still minimal.

### compat/nodejs — Express (flagship), staged stepping stones

- **Express 4** is the community's shared bar for "Node compat works"
  (Bun used it the same way): CJS, pure-JS dependency tree, exercises
  require + node_modules resolution, `node:http`, `node:events`,
  streams, `Buffer`. Express hello-world + middleware + router through
  the runtime-mode event loop is the Phase 4 definition of done.
- Stepping stones, in phase order:
  - Phase 3 (module system): **lodash** (CJS require), **commander** +
    **chalk** (CLI-ish, process/tty touches, ESM/CJS mix).
  - Phase 4 (servers): **Express 4**.
  - Stretch, post-Phase 4: **fastify** (heavier: pino, sonic-boom,
    modern Node internals) as the "beyond the bar" target.

### Mechanics

- `examples/<target>/` holds each demo app with `package.json` +
  lockfile committed; CI runs `npm ci` to materialize node_modules
  (packages are executed from disk — installing them stays npm's job,
  per Non-goals). Integration tests live next to the compat package and
  skip when the example's node_modules is absent (same opt-in pattern
  as the test262 suite), so `go test ./...` stays fast and hermetic.
- Version-pin every flagship (exact versions in lockfiles) so compat
  regressions are attributable to us, not to upstream releases.

## 5. Non-goals

- **N-API / native `.node` addons: impossible by design** (no dlopen
  target). The compat story for native deps is Go libraries via Env
  bindings/ops. State this prominently in user docs.
- Inspector/debugger protocol, V8-specific APIs (`v8` module), `vm`
  module fidelity — out of scope until the core surface is proven.
- npm client (installing packages). We execute what's on disk; fetching
  packages stays the user's tool's job for now.

## 6. Risks / open questions

- **nextTick vs engine job queue ordering** may need a core hook to run a
  host callback between job-queue drains (`RunJobs` iterator granularity
  looks sufficient — verify in Phase 3).
- **Startup cost per instance** for pool-fresh mode: revisit SpiderMonkey
  XDR bytecode caching (snapshot analog) if warmup dominates.
- **Structured clone** for worker_threads/MessagePort: engine support vs
  host-side serializer — decide in Phase 5.
- **WHATWG streams fidelity**: full spec is large; track what the fetch
  body + Node stream interop actually need and grow incrementally.
- **Chatty-op perf**: watch benchmarks (bench/) as compat layers land;
  coarse ops + bytes bridge is the rule, but Buffer-heavy npm code will
  find hot paths we haven't planned for.
