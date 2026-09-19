#!/usr/bin/env bash
# bunker-matrix-selftest.sh — argument and local-safety contract for DF-CRIER-81.
#
# Runs a private copy of bunker-matrix.sh with adjacent bunker/deploy and PATH
# stubs. No real bunker, Docker daemon, provider key, server, or network is used.
set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPTS_DIR="$(cd "$(dirname "$SELF")" && pwd)"
TARGET="$SCRIPTS_DIR/bunker-matrix.sh"
RETRY_LIB="$SCRIPTS_DIR/lib/transport-retry.sh"
TMP=""
CHECKS=0
FAILS=0

cleanup() {
  [ -n "$TMP" ] && rm -rf "$TMP"
}
trap cleanup EXIT

pass() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS  %s\n' "$1"
}

fail() {
  CHECKS=$((CHECKS + 1))
  FAILS=$((FAILS + 1))
  printf 'FAIL  %s\n' "$1" >&2
  [ -z "${2:-}" ] || printf '      %s\n' "$2" >&2
}

run_capture() { # output-file command...
  local output="$1"
  shift
  set +e
  "$@" >"$output" 2>&1
  RUN_RC=$?
  set -e
}

expect_rc() { # description expected actual output-file
  local desc="$1" expected="$2" actual="$3" output="$4"
  if [ "$actual" -eq "$expected" ]; then
    pass "$desc (rc=$actual)"
  else
    fail "$desc (want rc=$expected, got rc=$actual)" "$(cat "$output")"
  fi
}

expect_contains() { # description file literal
  local desc="$1" file="$2" literal="$3"
  if grep -Fq -- "$literal" "$file"; then
    pass "$desc"
  else
    fail "$desc" "missing '$literal' in: $(cat "$file")"
  fi
}

expect_absent() { # description file literal
  local desc="$1" file="$2" literal="$3"
  if grep -Fq -- "$literal" "$file"; then
    fail "$desc" "unexpected '$literal' in: $(cat "$file")"
  else
    pass "$desc"
  fi
}

[ -r "$TARGET" ] || { printf 'bunker-matrix-selftest: missing %s\n' "$TARGET" >&2; exit 2; }
[ -r "$RETRY_LIB" ] || { printf 'bunker-matrix-selftest: missing %s\n' "$RETRY_LIB" >&2; exit 2; }
command -v mktemp >/dev/null 2>&1 || { printf 'bunker-matrix-selftest: mktemp is required\n' >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { printf 'bunker-matrix-selftest: python3 is required\n' >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/bunker-matrix-selftest.XXXXXX")" || exit 2
mkdir -p "$TMP/repo/scripts/lib" "$TMP/bin"
cp "$TARGET" "$TMP/repo/scripts/bunker-matrix.sh"
cp "$RETRY_LIB" "$TMP/repo/scripts/lib/transport-retry.sh"

# Any remote deployment or bunker invocation is a hard test failure. Keeping the
# stubs adjacent to the copied matrix also makes a regression safe: it cannot
# fall through to the repository's real deploy script.
cat >"$TMP/repo/scripts/bunker-deploy.sh" <<'STUB'
#!/usr/bin/env bash
printf 'DEPLOY_CALLED env=%s %s\n' "${CR_ENV_FILE:-}" "$*" >>"$MATRIX_STUB_LOG"
[ "${MATRIX_ALLOW_REMOTE:-0}" = 1 ] && exit 0
exit 97
STUB
cat >"$TMP/bin/bunker" <<'STUB'
#!/usr/bin/env bash
printf 'BUNKER_CALLED %s\n' "$*" >>"$MATRIX_STUB_LOG"
[ "${MATRIX_ALLOW_REMOTE:-0}" = 1 ] && exit 0
exit 98
STUB
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf 'CURL' >>"$MATRIX_CURL_LOG"
printf ' <%s>' "$@" >>"$MATRIX_CURL_LOG"
printf '\n' >>"$MATRIX_CURL_LOG"
case " $* " in
  *" -w "*"/agents/fail-closed/inbox"*) printf '{"error":"GUARD_BLOCKED"}\n403' ;;
  *" -w "*"/agents/guard-on/inbox"*"Ignore all previous instructions"*) printf '{"error":"GUARD_BLOCKED"}\n403' ;;
  *" -w "*"/agents/blocking/inbox"*) printf 'ECHO fixture\n200' ;;
  *" -w "*) printf '{}\n201' ;;
  *) printf '{}' ;;
esac
exit 0
STUB
chmod +x "$TMP/repo/scripts/bunker-deploy.sh" "$TMP/bin/bunker" "$TMP/bin/curl"
: >"$TMP/stub.log"
: >"$TMP/curl.log"
export MATRIX_STUB_LOG="$TMP/stub.log"
export MATRIX_CURL_LOG="$TMP/curl.log"
export BUNKER_BIN="$TMP/bin/bunker"
export PATH="$TMP/bin:$PATH"
MATRIX="$TMP/repo/scripts/bunker-matrix.sh"

# A. Both help spellings are successful and self-contained.
for help_flag in --help -h; do
  out="$TMP/help-${help_flag#-}.out"
  run_capture "$out" bash "$MATRIX" "$help_flag"
  expect_rc "$help_flag exits successfully" 0 "$RUN_RC" "$out"
  for literal in 'Usage:' '--local' '--host' '--port' '--agent' '--server' '--sink' '--skip-build' 'Remote prerequisites:' 'already-running local'; do
    expect_contains "$help_flag documents $literal" "$out" "$literal"
  done
done

# B. Parser failures name the bad input and never expose an unbound-variable traceback.
for option in --cell --host --port --agent --server --sink; do
  label="${option#--}"
  run_capture "$TMP/missing-$label.out" bash "$MATRIX" "$option"
  if [ "$RUN_RC" -ne 0 ]; then pass "$option without a value exits nonzero"; else fail "$option without a value exits nonzero" "$(cat "$TMP/missing-$label.out")"; fi
  expect_contains "$option missing-value diagnostic names the option" "$TMP/missing-$label.out" "$option requires a value"
  expect_absent "$option missing-value diagnostic has no unbound-variable traceback" "$TMP/missing-$label.out" 'unbound variable'
done

run_capture "$TMP/unknown.out" bash "$MATRIX" --wat
if [ "$RUN_RC" -ne 0 ]; then pass 'unknown option exits nonzero'; else fail 'unknown option exits nonzero' "$(cat "$TMP/unknown.out")"; fi
expect_contains 'unknown option is named' "$TMP/unknown.out" 'unknown option: --wat'

run_capture "$TMP/port.out" bash "$MATRIX" --local --port not-a-port
if [ "$RUN_RC" -ne 0 ]; then pass 'invalid port exits nonzero'; else fail 'invalid port exits nonzero' "$(cat "$TMP/port.out")"; fi
expect_contains 'invalid port diagnostic is actionable' "$TMP/port.out" 'port must be an integer between 1 and 65535'

# C. Remote is the default, but it must fail before deploy when identity is absent.
: >"$TMP/stub.log"
run_capture "$TMP/remote.out" bash "$MATRIX" --host 192.0.2.10 --port 30011
if [ "$RUN_RC" -ne 0 ]; then pass 'remote mode without identity exits nonzero'; else fail 'remote mode without identity exits nonzero' "$(cat "$TMP/remote.out")"; fi
expect_contains 'remote prerequisite error mentions --agent/--server' "$TMP/remote.out" '--agent and --server'
expect_contains 'remote prerequisite error points to --local' "$TMP/remote.out" '--local'
if [ ! -s "$TMP/stub.log" ]; then pass 'remote prerequisite failure occurs before deploy/bunker'; else fail 'remote prerequisite failure occurs before deploy/bunker' "$(cat "$TMP/stub.log")"; fi

for partial in agent server; do
  : >"$TMP/stub.log"
  if [ "$partial" = agent ]; then
    partial_args=(--agent crier-lab)
  else
    partial_args=(--server bunker-server)
  fi
  run_capture "$TMP/remote-$partial-only.out" bash "$MATRIX" "${partial_args[@]}"
  if [ "$RUN_RC" -ne 0 ]; then pass "remote mode with only --$partial exits nonzero"; else fail "remote mode with only --$partial exits nonzero" "$(cat "$TMP/remote-$partial-only.out")"; fi
  expect_contains "remote mode with only --$partial names both requirements" "$TMP/remote-$partial-only.out" '--agent and --server'
  if [ ! -s "$TMP/stub.log" ]; then pass "remote mode with only --$partial fails before remote calls"; else fail "remote mode with only --$partial fails before remote calls" "$(cat "$TMP/stub.log")"; fi
done

# A complete existing CI-shaped identity reaches the bunker preflight unchanged;
# the bunker stub intentionally stops it before any deployment.
: >"$TMP/stub.log"
run_capture "$TMP/remote-complete.out" bash "$MATRIX" --host 192.0.2.10 --port 30011 \
  --agent crier-lab --server bunker-server --sink http://127.0.0.1:19012 --skip-build
if [ "$RUN_RC" -ne 0 ]; then pass 'complete remote invocation reaches the expected preflight'; else fail 'complete remote invocation reaches the expected preflight' "$(cat "$TMP/remote-complete.out")"; fi
expect_contains 'complete remote invocation routes agent/server to bunker' "$TMP/stub.log" 'BUNKER_CALLED info crier-lab --server bunker-server'
expect_absent 'complete remote preflight failure does not deploy' "$TMP/stub.log" 'DEPLOY_CALLED'

# Let all stubs succeed and prove the existing CI-shaped invocation still runs
# every remote deploy cell and every advertised live probe.
: >"$TMP/stub.log"
: >"$TMP/curl.log"
run_capture "$TMP/remote-full.out" env MATRIX_ALLOW_REMOTE=1 DEEPSEEK_API_KEY=fixture-key \
  EVIDENCE="$TMP/remote-evidence.jsonl" bash "$MATRIX" \
  --host 192.0.2.10 --port 30011 --agent crier-lab --server bunker-server \
  --sink http://127.0.0.1:19012 --skip-build
expect_rc 'existing CI-shaped remote invocation remains compatible' 0 "$RUN_RC" "$TMP/remote-full.out"
for cell in guard-on guard-off fail-closed blocking; do
  expect_contains "remote mode deploys $cell" "$TMP/stub.log" "DEPLOY_CALLED env=/tmp/mx-$cell.env"
  expect_contains "remote mode reports $cell cell" "$TMP/remote-full.out" "--- cell $cell"
done
for probe_name in 'guard-on clean' 'guard-on injection' 'guard-off clean' \
  'guard-off injection delivered' 'fail-closed clean blocked' \
  'fail-closed injection blocked' 'blocking round-trip reply'; do
  expect_contains "remote mode retains $probe_name probe" "$TMP/remote-full.out" "PASS  $probe_name"
done
if [ "$(grep -c '^DEPLOY_CALLED ' "$TMP/stub.log")" -eq 4 ]; then
  pass 'remote mode performs exactly four cell deployments'
else
  fail 'remote mode performs exactly four cell deployments' "$(cat "$TMP/stub.log")"
fi

# D. Local guard-off targets the supplied base URL and invokes neither remote path.
: >"$TMP/stub.log"
: >"$TMP/curl.log"
run_capture "$TMP/local.out" env EVIDENCE="$TMP/evidence.jsonl" bash "$MATRIX" \
  --local --host 127.0.0.9 --port 45678
expect_rc 'local default guard-off fixture succeeds' 0 "$RUN_RC" "$TMP/local.out"
expect_contains 'local mode defaults to guard-off' "$TMP/local.out" "probing cell 'guard-off'"
expect_contains 'local output names its non-deploy mode' "$TMP/local.out" 'LOCAL mode: no deploy, restart, or reconfiguration'
if [ ! -s "$TMP/stub.log" ]; then pass 'local mode never invokes bunker-deploy.sh or bunker'; else fail 'local mode never invokes bunker-deploy.sh or bunker' "$(cat "$TMP/stub.log")"; fi
expect_contains 'local calls target the supplied host and port' "$TMP/curl.log" 'http://127.0.0.9:45678'
if grep -Fv 'http://127.0.0.9:45678' "$TMP/curl.log" | grep -q '^CURL'; then
  fail 'every local HTTP call targets the supplied base URL' "$(cat "$TMP/curl.log")"
else
  pass 'every local HTTP call targets the supplied base URL'
fi
expect_contains 'local mode retains guard-off clean probe' "$TMP/local.out" 'PASS  guard-off clean'
expect_contains 'local mode retains guard-off injection probe' "$TMP/local.out" 'PASS  guard-off injection delivered'
for unselected in guard-on fail-closed blocking; do
  expect_absent "local mode does not pretend to run unconfigured $unselected" "$TMP/local.out" "--- cell $unselected"
done

printf 'bunker-matrix-selftest: %d checks, %d failures\n' "$CHECKS" "$FAILS"
[ "$FAILS" -eq 0 ]
