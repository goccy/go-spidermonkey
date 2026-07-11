SPIDERMONKEY_WASM_REPO     ?= goccy/spidermonkey-wasm
SPIDERMONKEY_WASM_VERSION  ?= v0.1.0
# spidermonkey-wasm emits its release attestations from release.yml (the v* tag
# workflow), NOT build.yml — releasing lives only in release.yml there.
SPIDERMONKEY_WASM_WORKFLOW ?= goccy/spidermonkey-wasm/.github/workflows/release.yml

# The one upstream-sourced file: the wasm2go bridge, pulled from the
# spidermonkey-wasm release and verified against its SLSA attestation. Asset
# name on the left, in-tree filename on the right (the rename is cosmetic;
# gh attestation verify matches by content digest). There is no stdlib to ship
# alongside it — SpiderMonkey's standard library is compiled into the engine,
# which lives in the spidermonkeywasm2go module, not here.
BRIDGE_ASSET := spidermonkey_wasm2go.go
BRIDGE_FILE  := spidermonkey.go
RELEASE_URL       = https://github.com/$(SPIDERMONKEY_WASM_REPO)/releases/download/$(SPIDERMONKEY_WASM_VERSION)
ATTESTATION_API   = https://api.github.com/repos/$(SPIDERMONKEY_WASM_REPO)/attestations

.PHONY: spidermonkey download verify test

## spidermonkey: refresh the bridge from the upstream release and verify its
## GitHub artifact attestation. Run whenever SPIDERMONKEY_WASM_VERSION bumps.
spidermonkey: download verify

## download: fetch the wasm2go bridge from the upstream release and drop it in
## place at $(BRIDGE_FILE).
download:
	curl -fSL --proto '=https' --tlsv1.2 -o $(BRIDGE_FILE) $(RELEASE_URL)/$(BRIDGE_ASSET)

## verify: confirm the bridge carries a valid GitHub artifact attestation signed
## by the upstream release.yml workflow. The bundle is fetched anonymously from
## the public attestation API and verified offline via `gh attestation verify
## --bundle`. No GH access token is required.
verify:
	@set -eu; \
	root=$$(mktemp -d); \
	trap 'rm -rf $$root' EXIT; \
	bundle=$$root/bundle.jsonl; \
	digest=$$(shasum -a 256 $(BRIDGE_FILE) | awk '{print $$1}'); \
	echo "==> fetching attestation bundle for $(BRIDGE_FILE) (sha256:$$digest)"; \
	curl -fsSL --proto '=https' --tlsv1.2 \
	  "$(ATTESTATION_API)/sha256:$$digest" \
	  | jq -c '.attestations[].bundle' > $$bundle; \
	echo "==> verifying $(BRIDGE_FILE)"; \
	GH_TOKEN= GITHUB_TOKEN= gh attestation verify "$(BRIDGE_FILE)" \
	  -R $(SPIDERMONKEY_WASM_REPO) \
	  --bundle $$bundle \
	  --signer-workflow $(SPIDERMONKEY_WASM_WORKFLOW)

## test: run the Go test suite.
test:
	go test ./...
