SPIDERMONKEY_WASM_REPO     ?= goccy/spidermonkey-wasm
SPIDERMONKEY_WASM_VERSION  ?= v0.2.2
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
BRIDGE_FILE  := internal/spidermonkey.go
RELEASE_URL       = https://github.com/$(SPIDERMONKEY_WASM_REPO)/releases/download/$(SPIDERMONKEY_WASM_VERSION)
ATTESTATION_API   = https://api.github.com/repos/$(SPIDERMONKEY_WASM_REPO)/attestations

# --------------------------------------------------------------------------
# External conformance suites. test262 covers the ENGINE; these three cover the
# compat layers, which test262 says nothing about: the Node.js project's own
# test suite for compat/nodejs, the Web Platform Tests for compat/web, and
# Babel's fixture corpus as a whole-toolchain workload. Each is pinned to an
# exact upstream revision and fetched on demand (blobless sparse clone) — none
# of them belongs in this repository.
NODE_SUITE_REPO ?= https://github.com/nodejs/node.git
NODE_SUITE_REV  ?= 1e320ec51f092604ac90d05c0d9942fc80de8c5b   # v26.5.0
NODE_SUITE_DIRS := test/common test/parallel test/es-module test/fixtures

WPT_SUITE_REPO ?= https://github.com/web-platform-tests/wpt.git
WPT_SUITE_REV  ?= f4b24b414258bfdca10fbb0f8d646b97fc6657ec
WPT_SUITE_DIRS := resources common url encoding streams WebCryptoAPI console \
                  hr-time performance-timeline FileAPI urlpattern fetch dom \
                  html/webappapis service-workers/service-worker/resources \
                  compression user-timing webmessaging mimesniff

BABEL_SUITE_REPO ?= https://github.com/babel/babel.git
BABEL_SUITE_REV  ?= 6d0dbd2a92aefe03cf1f7d49ebb39acd56e11c72   # v8.0.4
BABEL_SUITE_DIRS := packages

.PHONY: spidermonkey download verify test test262 \
        nodetest-fetch nodetest wpt-fetch wpt babeltest-fetch babeltest suites

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

## test262: run the official ECMAScript conformance suite (tc39/test262,
## vendored as the test262/suite submodule) against this embedding. Takes
## about 45 minutes; see test262_suite_test.go for the skip policy and knobs.
test262:
	git submodule update --init --depth 1 test262/suite
	TEST262=1 go test -run TestTest262 -v -timeout 3h .

## nodetest-fetch: check out the pinned nodejs/node test tree into nodetest/suite.
nodetest-fetch:
	./scripts/fetch-suite.sh $(NODE_SUITE_REPO) $(NODE_SUITE_REV) nodetest/suite $(NODE_SUITE_DIRS)

## nodetest: run the Node.js project's own test suite against compat/nodejs.
## Takes about a minute. Tests addressed to the node binary itself (private
## internals, respawn, V8 flags) are skipped with an accounted reason
## (nodetest/policy.go), and the tests that HANG are quarantined by name
## (nodetest/quarantine.txt) — a list to shrink, not a permanent allowance.
##
## Run in a few SEQUENTIAL shards, not one process: a single process that gets
## through several thousand tests intermittently stops making progress near the
## end (docs/engine-followups.md), while a shard of ~1100 never has. Sequential
## because the stall also gets likelier the more processes run at once.
NODETEST_SHARDS ?= 4
nodetest: nodetest-fetch
	@set -e; fail=0; i=0; while [ $$i -lt $(NODETEST_SHARDS) ]; do \
		echo "==> nodetest shard $$i/$(NODETEST_SHARDS)"; \
		NODETEST=1 NODETEST_SHARD=$$i/$(NODETEST_SHARDS) \
			go test ./nodetest/ -run TestNodeSuite -v -timeout 20m || fail=1; \
		i=$$((i + 1)); \
	done; exit $$fail

## wpt-fetch: check out the pinned web-platform-tests tree into wpt/suite.
wpt-fetch:
	./scripts/fetch-suite.sh $(WPT_SUITE_REPO) $(WPT_SUITE_REV) wpt/suite $(WPT_SUITE_DIRS)

## wpt: run the Web Platform Tests against compat/web. Judged per subtest.
wpt: wpt-fetch
	WPT=1 go test ./wpt/ -run TestWPTSuite -v -timeout 2h

## babeltest-fetch: check out Babel's fixture corpus and install the matching
## @babel/* packages the fixtures are run through.
babeltest-fetch:
	./scripts/fetch-suite.sh $(BABEL_SUITE_REPO) $(BABEL_SUITE_REV) babeltest/suite $(BABEL_SUITE_DIRS)
	./scripts/babel-suite-deps.sh babeltest/suite babeltest/package.json
	cd babeltest && npm install --no-audit --no-fund

## babeltest: run Babel's fixture corpus through @babel/core on this runtime.
babeltest: babeltest-fetch
	BABELTEST=1 go test ./babeltest/ -run TestBabelSuite -v -timeout 2h

## suites: every external conformance suite, in ascending runtime.
suites: wpt nodetest babeltest test262
