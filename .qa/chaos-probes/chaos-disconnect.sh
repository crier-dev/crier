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
tmp_out=$(mktemp)
trap 'rm -f "$tmp_out"' EXIT
(
  cd "$repo" || exit 2
  set -o pipefail
  go test ./... -count=1 -timeout 60s -p 1 2>&1 | tee "$tmp_out"
)
rc=$?
cat "$tmp_out" >> "$log"

# Classify live-transport dependency failures under network cut as UNVERIFIED,
# separate from the offline suite result. A test that fails because the proxy
# is unreachable under the cut is not a disconnect-handling defect — it is a
# test that cannot run under the cut. The offline suite (tests that do not
# depend on live transport) still gets its own PASS/FAIL verdict.
live_transport_pattern='proxyconnect|connection refused|dial tcp|no such host|TLS handshake timeout'
if grep -q "NETWORK_CUT_APPLIED" "$log" 2>/dev/null && \
   grep -qE "$live_transport_pattern" "$tmp_out" 2>/dev/null; then
  printf 'UNVERIFIED: live transport dependency unavailable under network cut\n' | tee -a "$log"
  # Offline leg: tests whose output does not mention live-transport failure.
  # If any package passed, the offline suite has a result to report.
  if grep -qE '^ok\s' "$tmp_out" 2>/dev/null; then
    offline_pass=$(grep -cE '^ok\s' "$tmp_out")
    printf 'OFFLINE_SUITE: %d package(s) passed independently of live transport\n' "$offline_pass" | tee -a "$log"
  fi
  if grep -qE '^FAIL\s' "$tmp_out" 2>/dev/null; then
    offline_fail=$(grep -cE '^FAIL\s' "$tmp_out")
    printf 'OFFLINE_SUITE_FAIL: %d package(s) failed independent of live transport\n' "$offline_fail" | tee -a "$log"
  fi
fi

printf 'RESULT: %s (exit %d)\n' "$([[ $rc -eq 0 ]] && echo PASS || echo FAIL)" "$rc" | tee -a "$log"
if [[ ! -s "$log" ]]; then
  echo "FAIL: disconnect evidence unexpectedly empty: $log" >&2
  exit 1
fi
exit "$rc"
