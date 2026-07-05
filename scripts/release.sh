#!/usr/bin/env bash
# release.sh — cut a release. Auto-increments the version from the latest
# vX.Y.Z tag (patch by default), tags the current commit, and pushes it; CI
# (.github/workflows/ci.yml) then builds the .deb + tarball and publishes the
# GitHub release with generated notes. No manual version bookkeeping needed.
#
#   ./scripts/release.sh            # bump patch:  v0.1.4 -> v0.1.5
#   ./scripts/release.sh --minor    # bump minor:  v0.1.4 -> v0.2.0
#   ./scripts/release.sh --major    # bump major:  v0.1.4 -> v1.0.0
#   ./scripts/release.sh 1.4.0      # set an explicit version
#
set -euo pipefail

bump="patch"
explicit=""
case "${1:-}" in
  --major) bump="major" ;;
  --minor) bump="minor" ;;
  --patch | "") bump="patch" ;;
  v[0-9]* | [0-9]*) explicit="${1#v}" ;;
  *)
    echo "usage: $0 [--major|--minor|--patch|X.Y.Z]" >&2
    exit 1
    ;;
esac

# Consider tags on the remote too, so releases keep incrementing even from a
# fresh clone that hasn't fetched them yet.
git fetch --tags --quiet origin 2>/dev/null || true
latest="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -n1)"
latest="${latest:-v0.0.0}"

if [ -n "$explicit" ]; then
  ver="$explicit"
else
  base="${latest#v}"
  major="${base%%.*}"
  rest="${base#*.}"
  minor="${rest%%.*}"
  patch="${rest#*.}"
  patch="${patch%%[-+.]*}" # drop any prerelease/build suffix
  case "$bump" in
  major)
    major=$((major + 1))
    minor=0
    patch=0
    ;;
  minor)
    minor=$((minor + 1))
    patch=0
    ;;
  patch) patch=$((patch + 1)) ;;
  esac
  ver="${major}.${minor}.${patch}"
fi

if ! printf '%s' "$ver" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "error: computed version '$ver' is not X.Y.Z" >&2
  exit 1
fi
tag="v$ver"

if [ -n "$(git status --porcelain)" ]; then
  echo "error: working tree is not clean — commit or stash first" >&2
  exit 1
fi
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  echo "error: tag $tag already exists" >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
sha="$(git rev-parse --short HEAD)"
echo ">> latest $latest -> next $tag  (on $branch $sha)"
git tag -a "$tag" -m "$tag"
git push origin "$tag"

echo ">> pushed $tag. CI builds the .deb + tarball and publishes the release:"
echo "   https://github.com/games-on-whales/LXC2Docker/releases/tag/$tag"
