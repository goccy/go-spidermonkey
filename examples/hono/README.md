# Hono on the Cloudflare-Workers layer

Runs the real, unmodified [Hono](https://hono.dev/) framework on the
go-spidermonkey Cloudflare-Workers compat layer — the warmed instance pool
that sits behind `net/http` — with no Node.js or Workers runtime involved. The
`hono` package is loaded from `./node_modules` exactly as a bundler would
resolve it, and the whole program is a normal Go `main`.

## Try it

```sh
make setup   # installs pnpm (via corepack) + the npm dependencies
make run     # serves the Hono app on http://localhost:8787
```

Then, in another terminal:

```sh
curl 'http://localhost:8787/api/items/42?v=7'
# {"id":"42","v":"7","greeting":"bound from Go"}
```

## Test

`make test` runs `main_test.go`, which serves the app through an in-process HTTP
server, makes a request, and asserts the response body — proving the framework
runs correctly and that the Go-bound `env` reaches the app.

## How it works

`main.go` warms a one-instance `cfworkers` pool over the ESM Hono worker in
`main.js` (`export default app`, embedded with `//go:embed`), with
`os.DirFS(".")` so its `import { Hono } from "hono"` resolves against
`./node_modules` via `nodejs.ESMLoader`. The pool implements `http.Handler`, so
`main` serves it directly with `http.ListenAndServe`; the test serves the same
handler through `httptest.NewServer`.

The `GREETING` value read via `c.env.GREETING` is bound from Go through the
pool's `Env` (`cfworkers.Static("bound from Go")`), demonstrating host-supplied
Workers bindings.

It depends on the parent module via a local `replace`:

```
replace github.com/goccy/go-spidermonkey => ../..
```
