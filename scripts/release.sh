#!/usr/bin/env bash
# release.sh — cut a release. Tags vX.Y.Z on the current commit and pushes it;
# CI (.github/workflows/ci.yml) then builds the .deb and publishes the GitHub
# release with generated notes.
#
#   ./scripts/release.sh 0.2.0
#
set -euo pipefail

ver="${1:-}"
if [ -z "$ver" ]; then
  echo "usage: $0 <version>   e.g. $0 0.2.0" >&2
  exit 1
fi
ver="${ver#v}"
if ! printf '%s' "$ver" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.]+)?$'; then
  echo "error: version must look like X.Y.Z (got '$ver')" >&2
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
echo ">> tagging $tag on $branch ($sha)"
git tag -a "$tag" -m "$tag"
git push origin "$tag"

echo ">> pushed $tag. CI builds the .deb and publishes the release:"
echo "   https://github.com/games-on-whales/LXC2Docker/releases/tag/$tag"
