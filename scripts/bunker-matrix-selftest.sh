#!/usr/bin/env bash
#
# scripts/bunker-matrix-selftest.sh — prove the bunker-matrix argument surface,
# its --local safety and its remote-mode refusal — with NO bunker host, NO
# docker, NO real crier server and NO network (DF-CRIER-81).
#
# WHY THIS EXISTS
# ---------------
# scripts/bunker-matrix.sh probes a live crier relay and (remote mode) deploys
# every cell through bunker-deploy.sh + the bunker CLI. That made its argument
# surface untestable: a parsing refactor could silently break CI's invocation
# (.github/workflows/bunker-e2e.yml passes --host/--port/--agent/--server/--sink)
# or START a real deployment from a typo. This selftest drives a SANDBOX COPY of
# the matrix (byte-identity pinned) with a poisoned bunker-deploy.sh (exit 42 +
# a canary file), a poisoned bunker CLI (calls logged, mode-switchable) and a
# local fake crier (a python3 http.server whose hit counters are read back), so
# every claim is measured, never eyeballed:
#
#   1.  --help and -h exit 0 and list --local, --host, --port, --agent,
#       --server, --sink, --skip-build plus the remote prerequisites
#   2.  an unknown option exits nonzero, names the argument, and leaves no
#       shell unbound-variable traceback
#   3.  an option without a value (and a value that is itself another option)
#       exits nonzero naming the option — no unbound-variable crash
#   4.  a non-numeric / out-of-range --port exits nonzero
#   5.  remote mode with NEITHER --agent/--server NOR BUNKER_AGENT/
#       BUNKER_SERVER refuses BEFORE any deployment (poison deploy unhit,
#       canary absent) and the refusal names --local and --agent/--server
#   6.  BUNKER_AGENT/BUNKER_SERVER satisfy that gate (CI's shape) and the run
#       really proceeds into remote operations (bunker preflight passes a
#       pass-mode poison bunker, the guard-on deploy leg reaches the poisoned
#       bunker-deploy.sh and dies with ITS exit code 42 + canary)
#   7.  --local NEVER calls bunker-deploy.sh (canary absent) and NEVER calls
#       the bunker CLI (poison log absent) — deployment-free by proof
#   8.  --local routes probes to the SUPPLIED --host/--port: the fake crier's
#       /status counter advances and it receives real register+inbox POSTs
#   9.  --local honestly SKIPS the cells the server posture cannot answer
#       (guard-on without a key, fail-closed with the guard on, blocking
#       without --sink), reports them as SKIP lines, and still exits 0 because
#       zero PROBES failed
#   10. skip evidence rows carry the documented shape (cell/ts/http=0/want=0,
#       body "skipped: <reason>")
#   11. --local without a running server fails with an actionable message
#
# Exit codes: 0 all assertions behaved, 1 at least one failed, 2 a dependency
# is missing (python3, curl) or the sandbox could not be built.
set -uo pipefail

SELFTEST_DIR="$(cd "$(dirname "$0")" && pwd)"
MATRIX_SRC="$SELFTEST_DIR/bunker-matrix.sh"
DEPLOY_SRC="$SELFTEST_DIR/bunker-deploy.sh"
TRANSPORT_LIB="$SELFTEST_DIR/lib/transport-retry.sh"

PASSED=0; FAILED=0
ok() { PASSED=$((PASSED + 1)); echo "PASS: $*"; }
bad() { FAILED=$((FAILED + 1)); echo "FAIL: $*"; }

usage_selftest() {
  cat <<'EOF'
scripts/bunker-matrix-selftest.sh — self-contained battery for
scripts/bunker-matrix.sh (DF-CRIER-81). Drives a sandbox copy of the matrix
with a poisoned bunker-deploy.sh, a poisoned bunker CLI and a local fake crier
(python3 stdlib http.server). No docker, no bunker host, no network.

Usage:
  bash scripts/bunker-matrix-selftest.sh            # run every arm
  bash scripts/bunker-matrix-selftest.sh --help     # this text

Exit codes: 0 all arms behaved, 1 at least one failed, 2 a dependency is missing.
EOF
}

TMP=""
FAKE_PID=""
cleanup() {
  if [[ -n "$FAKE_PID" ]] && kill -0 "$FAKE_PID" 2>/dev/null; then
    kill "$FAKE_PID" 2>/dev/null
    wait "$FAKE_PID" 2>/dev/null
  fi
  [[ -n "$TMP" ]] && rm -rf "$TMP"
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# run_matrix <log-prefix> [EXTRA_ENV k=v ...] -- <matrix args...>
#   Runs the SANDBOX copy with a hermetic environment: HOME points into the
#   sandbox (the real ~/.hermes/.env DEEPSEEK fallback can never leak in),
#   DEEPSEEK_API_KEY is empty, BUNKER_BIN points at the poison bunker, and
#   BUNKER_AGENT/BUNKER_SERVER are stripped unless the caller passes them.
run_matrix() {
  local log="$1"; shift
  local -a extra=()
  while [[ "${1:-}" != "--" ]]; do
    extra+=("$1"); shift
  done
  shift
  env -u BUNKER_AGENT -u BUNKER_SERVER \
    HOME="$SB_HOME" DEEPSEEK_API_KEY="" \
    PATH="$SB_BIN:$PATH" BUNKER_BIN="$SB_BIN/bunker" \
    "${extra[@]}" \
    bash "$SB/scripts/bunker-matrix.sh" "$@" >"$log.out" 2>"$log.err"
  echo $?
}

comb() { # <log-prefix> — combined output for greps
  cat "$1.out" "$1.err" 2>/dev/null
}

arm_help() { # <flag>
  local flag="$1" rc out
  out="$TMP/help$flag"
  rc="$(run_matrix "$TMP/help$flag" -- "$flag")"
  local missing=""
  for opt in --local --host --port --agent --server --sink --skip-build; do
    grep -q -- "$opt" "$out.out" || missing="$missing $opt"
  done
  if [[ "$rc" -ne 0 ]]; then
    bad "HELP$flag: exits $rc, want 0"
  elif [[ -n "$missing" ]]; then
    bad "HELP$flag: usage is missing:$missing"
  elif ! grep -q "SSH key" "$out.out"; then
    bad "HELP$flag: usage does not name the remote prerequisites (SSH key)"
  elif [[ -s "$out.err" ]]; then
    bad "HELP$flag: printed to stderr what should be usage: $(head -c 200 "$out.err")"
  else
    ok "HELP$flag: exits 0, lists every option, the remote prerequisites and the bunker tooling"
  fi
}

main() {
  case "${1:-}" in
    -h | --help | help) usage_selftest; return 0 ;;
    "") ;;
    *)
      echo "bunker-matrix-selftest: unknown argument '${1}' (try --help)" >&2
      return 2
      ;;
  esac

  for tool in python3 curl; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "bunker-matrix-selftest: '$tool' is not on PATH — needed to drive the sandbox" >&2
      return 2
    fi
  done
  for f in "$MATRIX_SRC" "$DEPLOY_SRC" "$TRANSPORT_LIB"; do
    if [[ ! -f "$f" ]]; then
      echo "bunker-matrix-selftest: cannot find $f next to this script" >&2
      return 2
    fi
  done

  TMP="$(mktemp -d "${TMPDIR:-/tmp}/bunker-matrix-selftest.XXXXXX")" || {
    echo "bunker-matrix-selftest: mktemp -d failed" >&2
    return 2
  }

  # ── sandbox: scripts/ copy + poison deploy + poison bunker ─────────────────
  SB="$TMP/sandbox"
  SB_HOME="$TMP/home"
  SB_BIN="$TMP/bin"
  # The matrix computes REPO="$(dirname "$0")/.." and sources
  # "$REPO/scripts/lib/transport-retry.sh", so the sandbox must mirror the
  # repo layout: scripts/lib under the sandbox root.
  mkdir -p "$SB/scripts/lib" "$SB_HOME" "$SB_BIN" || return 2
  cp "$TRANSPORT_LIB" "$SB/scripts/lib/transport-retry.sh"
  cp "$MATRIX_SRC" "$SB/scripts/bunker-matrix.sh"
  chmod +x "$SB/scripts/bunker-matrix.sh"
  # POISON bunker-deploy.sh: any deployment attempt is loud (POISON-DEPLOY on
  # stderr), countable (canary file) and fatal (exit 42, the leg's own code).
  {
    echo '#!/usr/bin/env bash'
    echo 'echo "POISON-DEPLOY: bunker-deploy.sh must never run in local mode" >&2'
    echo "touch \"$TMP/canary-deploy\""
    echo 'exit 42'
  } > "$SB/scripts/bunker-deploy.sh"
  chmod +x "$SB/scripts/bunker-deploy.sh"
  # POISON bunker CLI: every call is logged (canary-bunker); default mode fails
  # loudly (exit 43), pass-mode (POISON_BUNKER_MODE=pass) exits 0 so the remote
  # preflight can be driven PAST and the deploy leg reached (arm 6).
  {
    echo '#!/usr/bin/env bash'
    echo 'echo "$@" >> "${POISON_BUNKER_LOG:?}"'
    echo 'if [ "${POISON_BUNKER_MODE:-fail}" = "pass" ]; then exit 0; fi'
    echo 'echo "POISON-BUNKER: the bunker CLI must never run in local mode" >&2'
    echo 'exit 43'
  } > "$SB_BIN/bunker"
  chmod +x "$SB_BIN/bunker"

  # Premise: the sandbox runs a byte-identical copy of the tracked matrix.
  if cmp -s "$MATRIX_SRC" "$SB/scripts/bunker-matrix.sh"; then
    ok "PREMISE: the sandbox runs a byte-identical copy of scripts/bunker-matrix.sh"
  else
    bad "PREMISE: the sandbox copy of bunker-matrix.sh drifted from the tracked file — the arms would test a stranger"
  fi

  # ── 1: --help / -h ──────────────────────────────────────────────────────────
  arm_help "--help"
  arm_help "-h"

  # ── 2: unknown option ───────────────────────────────────────────────────────
  rc="$(run_matrix "$TMP/unknown" -- --frobnicate)"
  if [[ "$rc" -ne 0 ]] \
    && grep -q -- "--frobnicate" "$TMP/unknown.err" \
    && ! grep -q "unbound variable" "$TMP/unknown.err"; then
    ok "UNKNOWN-ARG: exits nonzero, names the argument, no unbound-variable traceback"
  else
    bad "UNKNOWN-ARG: rc=$rc; stderr: $(head -c 200 "$TMP/unknown.err")"
  fi

  # ── 3: option without a value / option-as-value ─────────────────────────────
  rc="$(run_matrix "$TMP/novalue" -- --host)"
  if [[ "$rc" -ne 0 ]] \
    && grep -q -- "--host" "$TMP/novalue.err" \
    && ! grep -q "unbound variable" "$TMP/novalue.err"; then
    ok "MISSING-VALUE: an option without a value exits nonzero naming the option"
  else
    bad "MISSING-VALUE: rc=$rc; stderr: $(head -c 200 "$TMP/novalue.err")"
  fi

  rc="$(run_matrix "$TMP/optasval" -- --port --local)"
  if [[ "$rc" -ne 0 ]] \
    && grep -q -- "--port" "$TMP/optasval.err" \
    && ! grep -q "unbound variable" "$TMP/optasval.err"; then
    ok "OPTION-AS-VALUE: an option given another option as its value exits nonzero naming the option"
  else
    bad "OPTION-AS-VALUE: rc=$rc; stderr: $(head -c 200 "$TMP/optasval.err")"
  fi

  # ── 4: port validation ──────────────────────────────────────────────────────
  for badport in abc 0 70000; do
    rc="$(run_matrix "$TMP/port-$badport" -- --local --port "$badport")"
    if [[ "$rc" -ne 0 ]] \
      && grep -qi "port" "$TMP/port-$badport.err" \
      && ! grep -q "unbound variable" "$TMP/port-$badport.err"; then
      ok "PORT-INVALID-$badport: refused with a diagnostic naming the port"
    else
      bad "PORT-INVALID-$badport: rc=$rc; stderr: $(head -c 200 "$TMP/port-$badport.err")"
    fi
  done

  # ── 5: remote mode without identity refuses BEFORE any deploy ───────────────
  rc="$(run_matrix "$TMP/remote-refuse" -- --host 127.0.0.1 --port 29999)"
  if [[ "$rc" -ne 0 ]] \
    && grep -q -- "--local" "$TMP/remote-refuse.err" \
    && grep -q -- "--agent" "$TMP/remote-refuse.err" \
    && [[ ! -e "$TMP/canary-deploy" ]]; then
    ok "REMOTE-REFUSES: refuses before any deployment and names --local and --agent/--server as the alternatives"
  else
    bad "REMOTE-REFUSES: rc=$rc; names --local: $(grep -c -- '--local' "$TMP/remote-refuse.err" || true); canary: $([[ -e "$TMP/canary-deploy" ]] && echo present || echo absent)"
  fi

  # ── 6: BUNKER_AGENT/BUNKER_SERVER satisfy the gate; the deploy leg is then
  #      genuinely reachable (poison bunker passes the preflight, the poisoned
  #      bunker-deploy.sh dies with its own exit 42) ────────────────────────────
  : > "$TMP/canary-bunker"
  # NOTE: the matrix does not propagate the deploy leg's own exit code —
  # deploy_leg turns a failed leg into `fatal …` (exit 1). The PROOF that the
  # gate opened and the leg genuinely reached bunker-deploy.sh is therefore the
  # canary + the POISON-DEPLOY line, with a nonzero matrix exit.
  rc="$(run_matrix "$TMP/remote-env" \
    POISON_BUNKER_MODE=pass POISON_BUNKER_LOG="$TMP/canary-bunker" \
    BUNKER_AGENT=ci-agent BUNKER_SERVER=ci-server \
    -- --host 127.0.0.1 --port 29999)"
  if [[ "$rc" -ne 0 ]] \
    && [[ -e "$TMP/canary-deploy" ]] \
    && grep -q "POISON-DEPLOY" "$TMP/remote-env.err"; then
    ok "REMOTE-ENV-GATE: BUNKER_AGENT/BUNKER_SERVER satisfy the gate and the deploy leg really reaches bunker-deploy.sh (canary + POISON-DEPLOY, matrix exits $rc)"
  else
    bad "REMOTE-ENV-GATE: rc=$rc (want nonzero); canary: $([[ -e "$TMP/canary-deploy" ]] && echo present || echo absent); stderr: $(head -c 200 "$TMP/remote-env.err")"
  fi

  # ── the local battery: start the fake crier, drive --local at it ────────────
  cat > "$TMP/fake_crier.py" <<'PY'
import json, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

COUNTERS = os.environ["FAKE_COUNTERS"]
counts = {"status": 0, "agents": 0, "inbox": 0, "patch": 0, "other": 0}

def flush():
    with open(COUNTERS, "w") as f:
        json.dump(counts, f)

class H(BaseHTTPRequestHandler):
    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path.startswith("/status"):
            counts["status"] += 1
            self._json({"require_agent_signature": False, "guard_enabled": True})
        else:
            counts["other"] += 1
            self._json({"error": "unexpected GET"}, 404)
        flush()

    def do_POST(self):
        p = self.path
        if "/inbox" in p:
            counts["inbox"] += 1
            # Minimal guard emulation so the matrix's PASS path can be proven:
            # a per-agent dead-provider policy (registered by the fail-closed
            # cell) must answer 403 GUARD_BLOCKED — the real crier's behavior
            # with fail_closed=true and an unreachable provider.
            if "fail-closed" in p:
                self._json({"error": "GUARD_BLOCKED"}, 403)
            else:
                self._json({"ok": True}, 201)
        elif p.rstrip("/").endswith("/agents"):
            counts["agents"] += 1
            self._json({"ok": True}, 201)
        else:
            counts["other"] += 1
            self._json({"ok": True}, 201)
        flush()

    def do_PATCH(self):
        counts["patch"] += 1
        self._json({"ok": True})
        flush()

    def log_message(self, *a):
        pass

srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
with open(os.environ["FAKE_PORT_FILE"], "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
PY
  : > "$TMP/counters.json"
  FAKE_COUNTERS="$TMP/counters.json" FAKE_PORT_FILE="$TMP/fake.port" \
    python3 "$TMP/fake_crier.py" &
  FAKE_PID=$!
  for _ in $(seq 1 50); do
    [[ -s "$TMP/fake.port" ]] && break
    sleep 0.1
  done
  if [[ ! -s "$TMP/fake.port" ]]; then
    bad "FAKE-SERVER: the fake crier never became ready — the local arms cannot run"
    if [[ "$FAILED" -gt 0 ]]; then return 1; fi
    return 0
  fi
  FAKE_PORT="$(cat "$TMP/fake.port")"
  counters() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], 0))' "$TMP/counters.json" "$1"; }

  # ── 7+8+9: --local never deploys, routes to the supplied host/port, skips
  #      honestly and exits 0 because zero probes failed ──────────────────────
  EV="$TMP/local-evidence.jsonl"
  rm -f "$TMP/canary-deploy" "$TMP/canary-bunker"
  rc="$(run_matrix "$TMP/local" EVIDENCE="$EV" \
    POISON_BUNKER_MODE=fail POISON_BUNKER_LOG="$TMP/canary-bunker" \
    -- --local --host 127.0.0.1 --port "$FAKE_PORT")"
  if [[ "$rc" -eq 0 ]] && [[ ! -e "$TMP/canary-deploy" ]] \
    && ! grep -q "POISON-DEPLOY" "$TMP/local.out" "$TMP/local.err"; then
    ok "LOCAL-NO-DEPLOY: --local exits 0 and bunker-deploy.sh is NEVER called (poison canary absent)"
  else
    bad "LOCAL-NO-DEPLOY: rc=$rc; canary: $([[ -e "$TMP/canary-deploy" ]] && echo present || echo absent)"
  fi

  if [[ ! -e "$TMP/canary-bunker" ]] \
    && ! grep -q "POISON-BUNKER" "$TMP/local.out" "$TMP/local.err"; then
    ok "LOCAL-NO-BUNKER: the bunker CLI is NEVER called in local mode (poison log absent)"
  else
    bad "LOCAL-NO-BUNKER: poison bunker log present or POISON-BUNKER in output"
  fi

  if [[ "$(counters status)" -ge 1 && "$(counters agents)" -ge 1 && "$(counters inbox)" -ge 2 ]]; then
    ok "LOCAL-ROUTES-PROBES: probes hit the SUPPLIED --host/--port (status=$(counters status) agents=$(counters agents) inbox=$(counters inbox) on the fake crier)"
  else
    bad "LOCAL-ROUTES-PROBES: the fake crier was not really probed (status=$(counters status) agents=$(counters agents) inbox=$(counters inbox))"
  fi

  if grep -q "^SKIP  guard-on" "$TMP/local.out" \
    && grep -q "^SKIP  guard-off" "$TMP/local.out" \
    && grep -q "^SKIP  blocking" "$TMP/local.out" \
    && grep -q '^PASS  fail-closed clean blocked (HTTP 403)' "$TMP/local.out" \
    && grep -q '^PASS  fail-closed injection blocked (HTTP 403)' "$TMP/local.out" \
    && grep -qE "^== matrix done: 2 pass / 0 fail / 3 skipped" "$TMP/local.out"; then
    ok "LOCAL-SKIPS-HONEST: guard-on/guard-off/blocking skipped with named reasons, the fail-closed probes really ran and passed against the supplied host/port, summary reports 2 pass / 0 fail / 3 skipped"
  else
    bad "LOCAL-SKIPS-HONEST: local output did not show the expected skips + pass path: $(tail -c 400 "$TMP/local.out")"
  fi

  # ── 10: skip evidence rows carry the documented shape ──────────────────────
  if python3 - "$EV" <<'PY'
import json, sys
rows = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
skips = [r for r in rows if str(r.get("body", "")).startswith("skipped:")]
assert skips, "no skip rows in evidence"
for r in skips:
    assert r.get("http") == 0 and r.get("want") == 0, r
    assert "cell" in r and "ts" in r, r
print("ok")
PY
  then
    ok "LOCAL-EVIDENCE-SHAPE: skip rows carry cell/ts/http=0/want=0 and a 'skipped: <reason>' body"
  else
    bad "LOCAL-EVIDENCE-SHAPE: evidence at $EV does not carry the documented skip shape"
  fi

  # ── 11: --local without a running server fails actionably ──────────────────
  rc="$(run_matrix "$TMP/local-noserver" -- --local --host 127.0.0.1 --port 29998)"
  if [[ "$rc" -ne 0 ]] \
    && grep -q "start one first" "$TMP/local-noserver.err" \
    && grep -q "make run" "$TMP/local-noserver.err"; then
    ok "LOCAL-NO-SERVER: fails with an actionable message (start a server with make run first)"
  else
    bad "LOCAL-NO-SERVER: rc=$rc; stderr: $(head -c 200 "$TMP/local-noserver.err")"
  fi

  if [[ "$FAILED" -gt 0 ]]; then
    echo "bunker-matrix-selftest: $PASSED/$((PASSED + FAILED)) checks behaved — FAIL" >&2
    return 1
  fi
  echo "bunker-matrix-selftest: $PASSED/$((PASSED + FAILED)) checks behaved (help, argument validation, remote refusal + env gate, local no-deploy/no-bunker, probe routing, honest skips, evidence shape, no-server refusal)"
  return 0
}

main "$@"
exit $?
