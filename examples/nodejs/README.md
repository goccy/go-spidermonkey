# Node runtime example

Runs the real, unmodified [lodash](https://lodash.com/) (CommonJS) and
[commander](https://github.com/tj/commander.js) (dual CJS/ESM) npm packages on
the go-spidermonkey Node runtime — no Node.js binary is involved. The packages
are loaded from `./node_modules` exactly as Node would resolve them, and the
whole program is a normal Go `main`.

## Try it

```sh
make setup   # installs pnpm (via corepack) + the npm dependencies
make run     # boots the runtime and prints the app's output
```

Expected output:

```json
{"greeting":"hello carol","names":["alice","bob","carol"],"byAge":{"30":2,"34":1}}
```

## Test

`make test` runs `main_test.go`, which boots the runtime and asserts the output
matches the value above — proving the packages run correctly.

## How it works

`main.go` creates a runtime over `os.DirFS(".")` (so `require` resolves against
`./node_modules`), installs the Node compat layer, runs the CommonJS script in
`main.js` (embedded with `//go:embed`), and captures what it prints via the
runtime's stdout. It depends on the parent module via a local `replace`:

```
replace github.com/goccy/go-spidermonkey => ../..
```
