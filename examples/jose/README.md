# jose (WinterTC / web) example

Runs the real, unmodified [jose](https://github.com/panva/jose) npm package on
the go-spidermonkey WEB (WinterTC) layer — no Node.js binary is involved. It
signs a JWT with HS256 and verifies it using the WebCrypto surface, then reports
the verified claims. The package is loaded from `./node_modules` exactly as an
ESM resolver would find it, and the whole program is a normal Go `main`.

## Try it

```sh
make setup   # installs pnpm (via corepack) + the npm dependencies
make run     # boots the web layer and prints the app's output
```

Expected output:

```json
{"sub":"alice","role":"admin","iat":1720000000,"alg":"HS256","verified":true}
```

## Test

`make test` runs `main_test.go`, which boots the web layer and asserts the
output matches the value above — proving jose signs and verifies correctly.

## How it works

`main.go` creates a runtime over `os.DirFS(".")` (so imports resolve against
`./node_modules`), installs the `compat/web` (WinterTC) layer, sets the pure-ESM
module loader, and evaluates the ES module in `main.js` (embedded with
`//go:embed`) that imports `jose`, signs a JWT with a fixed secret and fixed
claims, verifies it, and prints the result — which `main.go` captures via the
runtime's stdout. It depends on the parent module via a local `replace`:

```
replace github.com/goccy/go-spidermonkey => ../..
```
