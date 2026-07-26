# Self-contained setup + run for the go-spidermonkey Node example.
.PHONY: setup run test clean

# `make setup` installs pnpm (via corepack, bundled with Node >= 16.9; falls
# back to `npm i -g pnpm`) and then the npm dependencies into node_modules.
setup: node_modules

node_modules:
	@command -v pnpm >/dev/null 2>&1 || corepack enable pnpm 2>/dev/null || npm install -g pnpm
	# Force a flat node_modules (what the module resolver expects); recent pnpm
	# no longer reads node-linker from .npmrc, so pass it explicitly.
	pnpm install --config.node-linker=hoisted

# Run the example (prints the app's JSON output).
run: node_modules
	go run .

# Verify the app produces the expected output.
test: node_modules
	go test ./...

clean:
	rm -rf node_modules
