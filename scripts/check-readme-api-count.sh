#!/usr/bin/env bash
# check-readme-api-count.sh — verify README API inventory matches docs/openapi.yaml
# Fails if the hand-maintained sentence in README.md drifts from the spec.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Parse OpenAPI spec
if ! command -v python3 &>/dev/null; then
  echo "ERROR: python3 required" >&2
  exit 2
fi

read -r spec_paths spec_ops < <(python3 -c "
import yaml, sys
try:
    d = yaml.safe_load(open('docs/openapi.yaml'))
    paths = len(d.get('paths', {}))
    ops = sum(len(v) for v in d.get('paths', {}).values())
    print(paths, ops)
except Exception as e:
    print(f'ERROR: {e}', file=sys.stderr)
    sys.exit(2)
")

# Extract README sentence
readme_line=$(grep -n "documented as.*paths.*operations.*docs/openapi.yaml" README.md | head -1)
if [[ -z "$readme_line" ]]; then
  echo "FAIL: README.md has no API inventory sentence matching 'documented as X paths / Y operations in docs/openapi.yaml'" >&2
  exit 1
fi

# Parse the counts from the README sentence
readme_counts=$(echo "$readme_line" | grep -oE '[0-9]+ paths / [0-9]+ operations')
if [[ -z "$readme_counts" ]]; then
  echo "FAIL: could not parse 'X paths / Y operations' from README line: $readme_line" >&2
  exit 1
fi

read readme_paths readme_ops < <(echo "$readme_counts" | awk '{print $1, $4}')

# Compare
if [[ "$readme_paths" != "$spec_paths" || "$readme_ops" != "$spec_ops" ]]; then
  echo "FAIL: README says '$readme_paths paths / $readme_ops operations' but docs/openapi.yaml has $spec_paths paths / $spec_ops operations" >&2
  echo "Update README.md to match the spec, or regenerate the sentence." >&2
  exit 1
fi

echo "PASS: README API inventory matches docs/openapi.yaml ($spec_paths paths / $spec_ops operations)"
exit 0