#!/usr/bin/env bash
# Run the repository suite while the caller's network cut is active.
# The caller owns applying/removing the cut; this script owns truthful evidence.
set -uo pipefail

repo=${1:-.}
log=${DISC_LOG:-disc.log}
repo=$(cd "$repo" 2>/dev/null && pwd) || { echo "UNVERIFIED: repository path unavailable: $repo" >&2; exit 2; }

suite_available() {
  [[ -f "$repo/go.mod" ]] && command -v go >/dev/null 2>&1
}

validate_evidence() {
  local evidence=$1
  if suite_available; then
    if [[ ! -s "$evidence" ]]; then
      echo "FAIL: repository suite is available but disconnect evidence is empty: $evidence" >&2
      return 1
    fi
  else
    echo "UNVERIFIED: repository test path unavailable (go.mod or go executable missing)" >&2
    return 2
  fi
}

if [[ ${2:-} == "--validate" ]]; then
  validate_evidence "${3:-$log}"
  exit $?
fi

if ! suite_available; then
  printf '%s\n' "UNVERIFIED: repository test path unavailable (go.mod or go executable missing)" | tee "$log"
  exit 2
fi

mkdir -p "$(dirname "$log")"
: > "$log"
cmd='go test ./... -count=1 -timeout 60s -p 1'
printf 'COMMAND: (cd %q && %s)\n' "$repo" "$cmd" | tee -a "$log"
(
  cd "$repo" || exit 2
  set -o pipefail
  go test ./... -count=1 -timeout 60s -p 1 2>&1 | tee -a "$log"
)
rc=$?
printf 'RESULT: %s (exit %d)\n' "$([[ $rc -eq 0 ]] && echo PASS || echo FAIL)" "$rc" | tee -a "$log"
if [[ ! -s "$log" ]]; then
  echo "FAIL: disconnect evidence unexpectedly empty: $log" >&2
  exit 1
fi
exit "$rc"
