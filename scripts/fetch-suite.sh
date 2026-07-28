#!/usr/bin/env bash
# fetch-suite.sh REPO_URL REV DEST PATH...
#
# Checks out a pinned revision of an upstream test suite into DEST, taking only
# the listed paths. The suites are large (nodejs/node's test tree alone is
# ~80 MB, web-platform-tests is gigabytes) and none of them belongs in this
# repository's history, so they are fetched on demand: a blobless, sparse,
# depth-1 clone pinned to REV, which is what makes a run reproducible.
#
# Re-running is cheap and idempotent: an existing DEST already at REV is left
# alone.
set -euo pipefail

if [ "$#" -lt 4 ]; then
	echo "usage: $0 REPO_URL REV DEST PATH..." >&2
	exit 2
fi

repo=$1
rev=$2
dest=$3
shift 3

if [ -d "$dest/.git" ]; then
	have=$(git -C "$dest" rev-parse HEAD 2>/dev/null || echo none)
	if [ "$have" = "$rev" ]; then
		echo "==> $dest already at $rev"
		exit 0
	fi
	echo "==> $dest is at $have, want $rev; refetching"
	rm -rf "$dest"
fi

mkdir -p "$(dirname "$dest")"
echo "==> cloning $repo at $rev into $dest"
git clone --filter=blob:none --no-checkout --sparse "$repo" "$dest"
git -C "$dest" sparse-checkout set "$@"
# A pinned SHA needs an explicit fetch: it is not necessarily an advertised tip.
git -C "$dest" fetch --depth 1 origin "$rev"
git -C "$dest" checkout --detach FETCH_HEAD
echo "==> $dest now at $(git -C "$dest" rev-parse HEAD)"
