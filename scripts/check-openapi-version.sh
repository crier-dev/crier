#!/usr/bin/env bash
# Check both OpenAPI copies against the tag being cut (or the nearest tag).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tag="${1:-$(git describe --tags --abbrev=0)}"
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "check-openapi-version: invalid release tag: $tag" >&2
  exit 2
fi
expected="${tag#v}"
source=docs/openapi.yaml
embedded=cmd/server/openapi.yaml
if ! cmp -s "$source" "$embedded"; then
  echo "check-openapi-version: OpenAPI copies missing or different" >&2
  exit 1
fi
# The source uses a quoted version directly under the top-level info mapping.
# Reject a missing, duplicated, unquoted, or malformed version rather than pass.
actual="$(awk '
  /^info:[[:space:]]*$/ { in_info=1; next }
  in_info && /^[^[:space:]#]/ { in_info=0 }
  in_info && /^  version:/ { count++; value=$0 }
  END { if (count == 1 && value ~ /^  version: "[^"[:space:]]+"[[:space:]]*$/) { sub(/^  version: "/, "", value); sub(/"[[:space:]]*$/, "", value); print value } else exit 1 }
' "$source")" || { echo "check-openapi-version: missing or invalid info.version" >&2; exit 1; }
if [[ "$actual" != "$expected" ]]; then
  echo "check-openapi-version: info.version $actual != $expected (tag $tag)" >&2
  exit 1
fi
printf 'PASS  check-openapi-version: info.version %s matches %s; copies identical\n' "$actual" "$tag"
