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

# The checkout is identified by BOTH the revision and the set of paths: adding
# a directory to the list is a change to what is checked out even though the
# revision has not moved, and a run that ignored that left the new directory
# missing while reporting success.
want_paths=$(printf '%s\n' "$@" | sort | tr '\n' ' ')
if [ -d "$dest/.git" ]; then
	have=$(git -C "$dest" rev-parse HEAD 2>/dev/null || echo none)
	have_paths=$(git -C "$dest" sparse-checkout list 2>/dev/null | sort | tr '\n' ' ' || echo "")
	if [ "$have" = "$rev" ] && [ "$have_paths" = "$want_paths" ]; then
		echo "==> $dest already at $rev with the requested paths"
		exit 0
	fi
	if [ "$have" = "$rev" ]; then
		echo "==> $dest is at $rev but the paths differ; widening the checkout"
		git -C "$dest" sparse-checkout set "$@"
		echo "==> $dest now has $want_paths"
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
