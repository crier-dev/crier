#!/usr/bin/env bash
# Run the repository suite while the caller's network cut is active.
# The caller owns applying/removing the cut; this script owns truthful evidence.
set -uo pipefail

# Exempt loopback from the dead proxy so Python clients (which respect HTTP_PROXY
# for loopback unlike Go net/http) can still reach the loopback test server.
export no_proxy=127.0.0.1,localhost

log=${DISC_LOG:-disc.log}

# Accept the documented --validate [repo] [log] form, while preserving the
# original [repo] --validate [log] form for existing callers.
if [[ ${1:-} == "--validate" ]]; then
  repo=${2:-.}
  evidence=${3:-$log}
elif [[ ${2:-} == "--validate" ]]; then
  repo=${1:-.}
  evidence=${3:-$log}
else
  repo=${1:-.}
fi
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

if [[ ${1:-} == "--validate" || ${2:-} == "--validate" ]]; then
  validate_evidence "$evidence"
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

# Build the failure set from the failing result lines, never from the first
# lines of the transcript. In particular, a passing package often precedes the
# package that failed when Go runs packages in parallel.
failed_packages=$(awk '$1 == "FAIL" && NF >= 2 { print $2 }' "$log" | awk '!seen[$0]++' | paste -sd, -)
failed_tests=$(awk '$1 == "---" && $2 == "FAIL:" && NF >= 3 { print $3 }' "$log" | awk '!seen[$0]++' | paste -sd, -)
failed_packages=${failed_packages:-none}
failed_tests=${failed_tests:-none}
printf 'FAILURE_SET: packages=%s tests=%s\n' "$failed_packages" "$failed_tests" | tee -a "$log"

# A failure is tolerated only when every named failing test contains evidence
# of the injected dead proxy. A transport-looking line in an `ok` package must
# not turn an unrelated failure into UNVERIFIED, and a mixed failure set is not
# tolerated. The tab-separated records are test-name<TAB>network-match.
live_transport_pattern='proxyconnect|connection refused|dial tcp|no such host|TLS handshake timeout|i/o timeout'
failure_tests=$(awk -v pattern="$live_transport_pattern" '
function flush() {
  if (name != "") {
    printf "%s\t%d\n", name, network
    name = ""
    network = 0
  }
}
/^--- FAIL:/ {
  flush()
  name = $3
  network = ($0 ~ pattern)
  next
}
name != "" {
  if ($0 ~ pattern) network = 1
  if ($0 ~ /^FAIL[[:space:]]*$/) flush()
}
END { flush() }
' "$log")
network_only=1
saw_failure=0
if [[ -n "$failure_tests" ]]; then
  while IFS=$'\t' read -r _test_name is_network; do
    [[ -n "$_test_name" ]] || continue
    saw_failure=1
    if [[ "$is_network" != 1 ]]; then
      network_only=0
    fi
  done <<< "$failure_tests"
else
  # Compile/package failures have no --- FAIL test line. In that case, use
  # transport evidence only from non-passing lines; the package name remains in
  # FAILURE_SET above so the evidence is still actionable.
  saw_failure=$(awk '$1 == "FAIL" && NF >= 2 { found = 1 } END { print found + 0 }' "$log")
  if [[ "$saw_failure" == 1 ]] && ! awk -v pattern="$live_transport_pattern" '
    /^ok[[:space:]]/ || /^\?[[:space:]]/ { next }
    $0 ~ pattern { found = 1 }
    END { exit(found ? 0 : 1) }
  ' "$log"; then
    network_only=0
  fi
fi

network_cut_applied=0
if [[ -n "${NETWORK_CUT_APPLIED:-}" ]] || grep -q "NETWORK_CUT_APPLIED" "$log" 2>/dev/null; then
  network_cut_applied=1
fi
if [[ "$saw_failure" == 1 && "$network_cut_applied" == 1 && "$network_only" == 1 ]]; then
  printf 'VERDICT: UNVERIFIED: network-caused failure(s) under network cut; packages=%s tests=%s\n' \
    "$failed_packages" "$failed_tests" | tee -a "$log"
elif [[ "$saw_failure" == 1 ]]; then
  if [[ "$network_cut_applied" == 1 ]]; then
    reason='unrelated failure(s) under network cut'
  else
    reason='unrelated or unclassified failure(s)'
  fi
  printf 'VERDICT: FAIL: %s; packages=%s tests=%s\n' "$reason" "$failed_packages" "$failed_tests" | tee -a "$log"
fi

printf 'RESULT: %s (exit %d)\n' "$([[ $rc -eq 0 ]] && echo PASS || echo FAIL)" "$rc" | tee -a "$log"
if [[ ! -s "$log" ]]; then
  echo "FAIL: disconnect evidence unexpectedly empty: $log" >&2
  exit 1
fi
exit "$rc"
