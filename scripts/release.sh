#!/usr/bin/env bash
# release.sh — publish an immutable calendar release (vYYYY.MM).
# Defaults to the current UTC month; an existing tag is never rewritten.
# CI builds the .deb and tarball, then the release-channel workflow updates stable.
# Usage: ./scripts/release.sh [v2026.10]
set -euo pipefail

version="${1:-v$(date -u +%Y.%m)}"
[[ "$version" == v* ]] || version="v$version"
[[ "$version" =~ ^v[0-9]{4}\.(0[1-9]|1[0-2])$ ]] || {
  echo "usage: $0 [vYYYY.MM]" >&2
  exit 1
}
tag="$version"
git fetch --tags --quiet origin
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
echo ">> release $tag (on $branch $sha)"
git tag -a "$tag" -m "$tag"
git push origin "$tag"

echo ">> pushed $tag. CI builds the .deb + tarball and publishes the release:"
echo "   https://github.com/games-on-whales/LXC2Docker/releases/tag/$tag"
