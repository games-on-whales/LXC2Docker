#!/usr/bin/env bash
# Test the actual release script without contacting a remote or creating tags.
set -euo pipefail
cd "$(dirname "$0")/.."
fake=$(mktemp -d)
trap 'rm -rf "$fake"' EXIT
mkdir "$fake/bin"
cat > "$fake/bin/git" <<'GIT'
#!/usr/bin/env bash
case "$1" in
  fetch) exit 0 ;;
  status) [ "${DIRTY:-0}" = 0 ] || echo ' M tracked-file'; exit 0 ;;
  rev-parse)
    case "$2" in
      -q) [ "${TAG_EXISTS:-0}" = 1 ] ;;
      --abbrev-ref) echo beta ;;
      --short) echo abc123 ;;
    esac ;;
  tag|push) printf '%s\n' "$*" >> "$RELEASE_CALLS" ;;
esac
GIT
printf '#!/bin/sh\necho 2026.10\n' > "$fake/bin/date"
chmod +x "$fake/bin/"*
export PATH="$fake/bin:$PATH" RELEASE_CALLS="$fake/calls"
pass=0
for version in default v2026.10 2026.10; do
  : > "$RELEASE_CALLS"
  if [ "$version" = default ]; then bash scripts/release.sh >/dev/null; else bash scripts/release.sh "$version" >/dev/null; fi
  grep -qx 'push origin v2026.10' "$RELEASE_CALLS"
  pass=$((pass+1))
done
for version in v2026.00 v2026.13 v2026.10.1 --minor v1.2.3; do
  : > "$RELEASE_CALLS"
  if bash scripts/release.sh "$version" >/dev/null 2>&1; then exit 1; fi
  [ ! -s "$RELEASE_CALLS" ]
  pass=$((pass+1))
done
for setting in TAG_EXISTS=1 DIRTY=1; do
  : > "$RELEASE_CALLS"
  if env "$setting" bash scripts/release.sh v2026.10 >/dev/null 2>&1; then exit 1; fi
  [ ! -s "$RELEASE_CALLS" ]
  pass=$((pass+1))
done
echo "$pass calendar release checks passed"
