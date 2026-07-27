# Next.js example

Boots an unmodified [Next.js 15](https://nextjs.org/) App Router **production
server** — prerendered Server Components, a Route Handler, and static assets —
inside the go-spidermonkey Node runtime. No Node.js binary is involved at serve
time; the whole program is a normal Go `main`.

**Dynamic SSR does not work yet.** A Server Component that runs per request
needs `AsyncLocalStorage` to survive into work the render schedules and finishes
after the call that established the store returns, which needs engine
async-context hooks this build does not have — see `docs/engine-followups.md`
item 8. `/` therefore returns 500; `/about`, `/api/hello`, `/_next/static/*` and
Next's own 404 all work.

The app in `app/` is a stock Next.js App Router app. It is built ahead of time
with real Node (`next build`), because Next's compiler (SWC) uses native
binaries that only exist at build time. The compiled `.next` output is then
served by the go-spidermonkey engine via Next's custom-server API.

## Try it

```sh
make setup   # installs pnpm (via corepack) + npm deps, then runs `next build`
make run     # serves the production build on http://localhost:3000
```

The build step needs real Node on your PATH; serving does not. Set `PORT` to
serve on a different port.

Then, in another terminal:

```sh
curl http://localhost:3000/api/hello
# {"hello":"from route handler","method":"GET"}
```

## Test

`make test` runs `main_test.go`, which starts the server on a free port, makes
an HTTP request to the `/api/hello` Route Handler, and asserts its JSON —
proving an unmodified Next.js production server runs on the runtime. The test is
slow (a full server boot); that is expected.

## How it works

`main.go` creates a runtime over `os.DirFS(".")` (so `require` resolves against
`./node_modules` and Next reads `./.next`) with `process.env.PORT` set, installs
the Node compat layer, and runs the standard Next.js custom server in `main.js`
(embedded with `//go:embed`) that `require("next")`, calls `app.prepare()`, and
`listen(process.env.PORT)`. `rt.Wait` drives the Node event loop so the server
keeps serving. The test starts it on a free port, polls `GET /api/hello` until
it responds, and cancels to stop.

It depends on the parent module via a local `replace`:

```
replace github.com/goccy/go-spidermonkey => ../..
```
