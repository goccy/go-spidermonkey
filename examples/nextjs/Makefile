# Self-contained setup + run for the go-spidermonkey Next.js example.
.PHONY: setup run test clean

# `make setup` installs pnpm (via corepack, bundled with Node >= 16.9; falls
# back to `npm i -g pnpm`), installs the npm dependencies, and BUILDS the app
# with real Node — `next build` runs SWC natives that are build-time only, so
# the app must be compiled on the host before the engine can serve it.
setup: node_modules .next

node_modules:
	@command -v pnpm >/dev/null 2>&1 || corepack enable pnpm 2>/dev/null || npm install -g pnpm
	# Next.js resolves transitive deps (styled-jsx, ...) from a flat layout, so
	# force the hoisted node-linker (what `.npmrc` requests; recent pnpm no longer
	# reads node-linker from .npmrc, so pass it explicitly).
	pnpm install --config.node-linker=hoisted

.next: node_modules
	pnpm run build

# Run the example (boots the Next.js production server and prints the JSON body).
run: node_modules .next
	go run .

# Verify the app produces the expected output.
test: node_modules .next
	go test ./...

clean:
	rm -rf node_modules .next
