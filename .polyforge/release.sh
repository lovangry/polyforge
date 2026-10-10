#!/bin/sh
# polyforge's own release procedure (polyforge#123).
#
# The shared workflow release templates are project-generic dispatchers:
# they scan the capsule's repos/ for the one repo carrying this file (or
# .polyforge/release.md) and execute it as the release owner's procedure.
# This file IS that procedure for the polyforge repository itself; it was
# moved here verbatim from the shared templates so non-polyforge capsules
# defer instead of executing polyforge's private story.
#
# What it does, in order:
#   1. compute the version floor (below), bump one segment (arg, default
#      minor), record the decision;
#   2. build the two release artifacts with the repo's own release target,
#     `GOWORK=off make release VERSION=<tag>`, from the recorded release
#      commit (RELEASE_COMMIT, default HEAD — the caller is responsible
#      for the worktree sitting at the commit being released);
#   3. produce the SHA256SUMS manifest beside the two binaries;
#   4. create one annotated tag named exactly the version on that commit;
#   5. claim the version atomically with a plain mkdir into the shared
#      publish root (a taken version fails loudly — never publish over a
#      parallel capsule's release), then place aihub, pf and SHA256SUMS
#      into the claimed per-tag directory;
#   6. record artifacts/release-notes.md and print the published paths.
#
# Contract the dispatcher relies on:
#   - RELEASES_ROOT must name the shared publish root (the per-tag
#     directories live directly inside it). The floor is the UNION of this
#     worktree's tags and the versions already published there: a parallel
#     capsule's tag is invisible to this clone (every capsule is its own
#     clone), but the publish root carries every version any capsule
#     shipped — so sibling capsules cannot collide on versions.
#
# Usage:
#   RELEASES_ROOT=<publish-root> sh .polyforge/release.sh [minor|patch|major]
set -eu

: "${RELEASES_ROOT:?RELEASES_ROOT must name the shared publish root (the per-tag version directories live directly inside it)}"

BUMP="${1:-minor}"

# Step 1 - the version floor, printed for the record (dry-run face):
{ git tag -l 'v*'; ls "$RELEASES_ROOT"; } | grep -E '^v[0-9]' | sort -Vu | tail -1

FLOOR=$({ git tag -l 'v*'; ls "$RELEASES_ROOT"; } | grep -E '^v[0-9]' | sort -Vu | tail -1)
if [ -z "$FLOOR" ]; then
  echo "release: no floor version found (no v* tags, empty publish root) - refusing to invent one" >&2
  exit 1
fi

# Bump exactly one segment of the floor; never invent a version ad hoc.
_major=${FLOOR#v}; _rest=${_major#*.}
_maj=${_major%%.*}; _min=${_rest%%.*}; _pat=${_rest#*.}
case "$BUMP" in
  minor) _min=$((_min + 1)); _pat=0 ;;
  patch) _pat=$((_pat + 1)) ;;
  major) _maj=$((_maj + 1)); _min=0; _pat=0 ;;
  *) echo "release: bump must be minor, patch or major (got \"$BUMP\")" >&2; exit 1 ;;
esac
TAG="v${_maj}.${_min}.${_pat}"
echo "release: floor $FLOOR -> version $TAG ($BUMP bump)"

RELEASE_COMMIT="${RELEASE_COMMIT:-$(git rev-parse HEAD)}"

# Step 2 - build the two artifacts with the tag stamped in at link time.
GOWORK=off make release VERSION="$TAG"

# Step 3 - checksum manifest beside the two binaries.
( cd "artifacts/release-$TAG" && sha256sum aihub pf > SHA256SUMS )

# Step 4 - the tag IS the version: one annotated tag on the release commit.
git tag -a "$TAG" -m "release $TAG" "$RELEASE_COMMIT"

# Step 5 - the atomic version claim: a plain mkdir (never mkdir -p), which
# fails loudly when a parallel capsule already published this version.
mkdir "$RELEASES_ROOT/$TAG"
mv "artifacts/release-$TAG/aihub" "artifacts/release-$TAG/pf" "artifacts/release-$TAG/SHA256SUMS" "$RELEASES_ROOT/$TAG/"

# Step 6 - the release record.
cat > artifacts/release-notes.md <<NOTES
# release $TAG

- commit: $RELEASE_COMMIT
- bump: $BUMP from floor $FLOOR
- published: releases/$TAG/aihub releases/$TAG/pf releases/$TAG/SHA256SUMS
NOTES

echo "release: published releases/$TAG/aihub releases/$TAG/pf releases/$TAG/SHA256SUMS"
echo "release: recorded artifacts/release-notes.md"
