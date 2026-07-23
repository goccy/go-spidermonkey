#!/bin/sh
# Fetches the Octane 2.0 benchmark sources for bench/octane_test.go.
# Pinned to a commit so scores stay comparable across machines/time.
# The sources are BSD-licensed (see LICENSE in the checkout) and are
# intentionally not vendored: ~10 MB of third-party JS.
set -eu
cd "$(dirname "$0")"
PIN=570ad1ccfe86e3eecba0636c8f932ac08edec517
if [ -d octane/.git ]; then
	git -C octane fetch --depth 1 origin "$PIN"
else
	rm -rf octane
	git clone https://github.com/chromium/octane octane
fi
git -C octane checkout -q "$PIN"
echo "octane ready at $(git -C octane rev-parse --short HEAD)"
