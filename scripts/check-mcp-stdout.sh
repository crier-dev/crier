#!/usr/bin/env bash
#
# scripts/check-mcp-stdout.sh — MCP launcher stdout contract gate (DF-CRIER-137).
#
# WHY THIS EXISTS
# ---------------
# The documented way to launch the MCP bridge is the compound command
#
#     make build-mcp && ./bin/crier-mcp          (README.md, docs/integration-guide.md)
#
# The bridge itself is stdio-clean: fed one MCP `initialize` frame it answers
# with exactly one JSON-RPC line on stdout and keeps every log line on stderr.
# The launcher around it was NOT clean, because the `build-mcp` recipe was not
# prefixed with `@` and make echoes each recipe line to STDOUT before running it:
#
#     $ make build-mcp > /tmp/pre.txt 2>/dev/null; cat /tmp/pre.txt
#     go build -ldflags "-X github.com/crier-dev/crier/internal/buildinfo.Version=…" -o bin/crier-mcp ./cmd/crier-mcp
#
# A strict MCP stdio client that launches that documented line reads a non-JSON
# line before any JSON-RPC frame and fails the handshake. Measured on this tree; the
# same finding was filed three times (DF-CRIER-66, DF-CRIER-91, DF-CRIER-137), so
# this script is the non-vacuous gate that makes a fourth re-find impossible: it
# runs the documented launcher(s) for real and fails closed on any contaminant.
#
# USAGE
# -----
#   bash scripts/check-mcp-stdout.sh                     # both documented launchers
#   bash scripts/check-mcp-stdout.sh 'CMD' ['CMD' ...]   # exactly the launchers you name
#   bash scripts/check-mcp-stdout.sh --help
#
# Default mode checks, in this order:
#   (1) make mcp
#   (2) make build-mcp && ./bin/crier-mcp
#
# WHAT IS CHECKED (per launcher)
# ------------------------------
#   1. the launcher is run via `bash -c` from the repo root with ONE MCP
#      `initialize` frame on stdin, and stdout/stderr captured to SEPARATE files
#      under ${TMPDIR:-/tmp}. The FIRST stdout line must be a JSON object that
#      carries `"jsonrpc":"2.0"` and a `result` member (the initialize response) —
#      i.e. no line precedes it. Anything else is reported VERBATIM (truncated to
#      ${CHECK_MCP_STDOUT_LINE_LIMIT:-200} chars) together with the launcher.
#   2. the child is read within a bounded timeout (${CHECK_MCP_STDOUT_TIMEOUT:-30}s,
#      the build included). A timeout is a FAILURE (exit 1) naming the launcher and
#      the budget — never a skip.
#   3. the child does not outlive the run: the launcher is started under
#      `timeout` WITHOUT `--foreground`, which is exactly what puts it in its own
#      process group, so on expiry the signal (TERM, escalating to KILL after 5s)
#      goes to the whole group — a hanging `make`/`go`/bridge tree is killed as a
#      group and reaped, never orphaned. `make mcp-stdout-selftest` pins this: its
#      hang fixture writes its own pid and the selftest asserts that pid is gone.
#
# ENVIRONMENT THE LAUNCHER RUNS WITHOUT
#   CRIER_HTTP_URL, CRIER_AGENT_ID and CRIER_AGENT_PRIVATE_KEY_FILE are UNSET for
#   every run (`env -u`), so the bridge takes its in-process path: it needs no
#   server and binds no port (verified — this launcher needs no listener).
#
#   The make-nesting variables (MAKEFLAGS, MFLAGS, MAKELEVEL) are unset as well,
#   and the strip is REPORTED on every run that had them. This gate is normally run
#   BY make, and a parent make exports MAKELEVEL=1 — at MAKELEVEL > 0 GNU make turns
#   --print-directory on automatically and prints
#   `make[1]: Entering directory '<dir>'` on STDOUT before doing anything
#   (measured: MAKELEVEL=1 + `make mcp` puts that line ahead of the JSON-RPC frame).
#   That line belongs to make's own nesting, not to the launcher an MCP client
#   launches from its own config, so the gate must not count it as contamination —
#   and it must say that it stripped it, which it does.
#
# EXIT CODES
#   0  every launcher kept its stdout contract (one PASS line each)
#   1  a launcher contaminated its stdout, or timed out
#   2  FAIL-CLOSED — a checker that verified nothing never prints a green:
#      `make` (or `go`) is not on PATH, `timeout` is not on PATH, the repo root or
#      cmd/crier-mcp is missing, ./bin/crier-mcp exists but is not executable, or
#      a launcher produced NO stdout line at all
#
# MISUSE
#   --help prints usage and exits 0; an unknown option is exit 2.
#
# OUTPUT CONTRACT (grep-able)
#   `check-mcp-stdout: checking launcher='<cmd>' …`               per launcher
#   `PASS — launcher=<cmd> first_stdout_line=<frame>`             one per launcher
#   `check-mcp-stdout: ERROR: …`                                  on stderr, on failure
#   `check-mcp-stdout: PASS — N launcher(s) checked, 0 contaminated`
#
# DEPENDENCIES: bash, timeout (the bounding/reaping mechanism — its absence is
# exit 2, because running a launcher unbounded is not an option), mktemp, wc,
# head, cut, env, plus `make` and the Go toolchain for the default launchers.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-mcp-stdout"
TIMEOUT="${CHECK_MCP_STDOUT_TIMEOUT:-30}"
LINE_LIMIT="${CHECK_MCP_STDOUT_LINE_LIMIT:-200}"

# The one frame every launcher is fed. It is the smallest legal MCP handshake: a
# client that speaks 2024-11-05 and declares no capabilities.
INIT_FRAME='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"stdio-check","version":"1.0"}}}'

# The documented launchers, in documentation order (README.md:408,
# docs/integration-guide.md:417/443). Explicit arguments replace this list.
DOCUMENTED_LAUNCHERS=(
  'make mcp'
  'make build-mcp && ./bin/crier-mcp'
)

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

# _truncate <string> — bound a line so a verbose contaminant cannot bury the log.
_truncate() {
  printf '%s' "$1" | cut -c "1-$LINE_LIMIT"
}

# _is_initialize_response <line> — true when the line is a single-line JSON
# object carrying both `"jsonrpc":"2.0"` and a `result` member. That is the
# documented shape of the initialize response (cmd/crier-mcp answers with one
# line and keeps logging on stderr); anything else on that first line is a
# contaminant — build output, a warning, a partial frame.
_is_initialize_response() {
  local line="$1"
  # Must be a JSON object on ONE line: `{ … }`.
  case "$line" in
    \{*\}) ;;
    *) return 1 ;;
  esac
  printf '%s' "$line" | grep -Eq '"jsonrpc"[[:space:]]*:[[:space:]]*"2\.0"' || return 1
  printf '%s' "$line" | grep -Eq '"result"[[:space:]]*:' || return 1
  return 0
}

# ── prerequisites ─────────────────────────────────────────────────────────────

# The repo root the launchers must run from (they use the relative path
# ./bin/crier-mcp). git is preferred so the checker works from any subdirectory;
# the script's own location is the fallback.
resolve_repo_root() {
  local root="" git_root=""
  if command -v git >/dev/null 2>&1; then
    git_root="$(git rev-parse --show-toplevel 2>/dev/null)"
  fi
  if [ -n "$git_root" ] && [ -f "$git_root/Makefile" ]; then
    root="$git_root"
  else
    root="$(cd "$(dirname "$SELF")/.." && pwd)"
  fi
  [ -f "$root/Makefile" ] || {
    _err "no Makefile at the resolved repo root $root — this checker verifies the"
    _err "  documented launcher of THIS repo and cannot verify anything without it."
    return 2
  }
  printf '%s' "$root"
  return 0
}

# Fail-closed tool resolution for the DEFAULT launchers. `timeout` is the
# bounding/reaping mechanism: without it a launcher that hangs would hang this
# checker, so its absence is exit 2 rather than an unbounded run. make and go are
# what the documented launchers build with; a missing `./bin/crier-mcp` is NOT a
# missing tool here — the launchers build it, and a launcher that then produces no
# stdout is the exit-2 path below.
resolve_tools() {
  local mode="$1" bin="" tool=""
  for tool in timeout make go; do
    command -v "$tool" >/dev/null 2>&1 || {
      _err "'$tool' is not on PATH — refusing to skip the MCP stdout check."
      _err "  mode: $mode"
      _err "  install it and re-run; a checker that verified nothing never prints a green."
      return 2
    }
  done
  TIMEOUT_BIN="$(command -v timeout)"
  [ -d "$REPO_ROOT/cmd/crier-mcp" ] || {
    _err "cmd/crier-mcp is not present under $REPO_ROOT — the documented launcher"
    _err "  cannot build anything there, so there is nothing to verify (mode: $mode)."
    return 2
  }
  bin="$REPO_ROOT/bin/crier-mcp"
  if [ -e "$bin" ] && [ ! -x "$bin" ]; then
    _err "$bin exists but is not executable — the documented launcher would fail"
    _err "  with a permission error, not with a JSON-RPC frame (mode: $mode)."
    return 2
  fi
  _info "timeout: $TIMEOUT_BIN (budget ${TIMEOUT}s, TERM then KILL after 5s, whole process group)"
  return 0
}

# ── the check ─────────────────────────────────────────────────────────────────

# check_launcher <launcher-command> — 0 contract kept, 1 contaminated/timed out,
# 2 nothing was verified.
check_launcher() {
  local cmd="$1"
  local tmp="" frame="" out="" err="" rc=0 first="" out_bytes=0 err_bytes=0 head5=""
  local -a env_args=()
  local v="" stripped=""

  tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-mcp-stdout.XXXXXX")" || {
    _err "mktemp -d failed — cannot capture the launcher's stdout and stderr."
    return 2
  }
  frame="$tmp/frame.jsonl"
  out="$tmp/stdout"
  err="$tmp/stderr"
  if ! printf '%s\n' "$INIT_FRAME" >"$frame"; then
    _err "cannot write the initialize frame to $frame."
    rm -rf "$tmp"
    return 2
  fi

  _info "checking launcher='$cmd' (one initialize frame on stdin; CRIER_HTTP_URL, CRIER_AGENT_ID, CRIER_AGENT_PRIVATE_KEY_FILE unset)"

  # `make` signals a NESTED invocation through its own environment: a parent make
  # exports MAKELEVEL=1 (measured on this tree), and GNU make turns
  # --print-directory ON automatically at MAKELEVEL > 0, printing
  #
  #     make[1]: Entering directory '/home/kara/crier'
  #
  # on STDOUT before it does anything else. This checker is normally run BY make
  # (`make mcp-stdout-check`), so without stripping those variables the gate would
  # blame make's own nesting chatter on the documented launcher — which no client
  # produces when it launches the command from its own config. They are stripped
  # here and the strip is REPORTED, never silent: `make mcp-stdout-selftest` pins
  # it with a fixture that contaminates its stdout exactly when MAKELEVEL is set.
  for v in MAKEFLAGS MFLAGS MAKELEVEL; do
    if [ -n "${!v:-}" ]; then
      env_args+=(-u "$v")
      stripped="$stripped $v=${!v}"
    fi
  done

  [ -n "$stripped" ] && _info "stripped make-nesting env (a parent make exports MAKELEVEL; GNU make then prints 'Entering directory' on stdout, which belongs to make, not to the launcher):$stripped"

  # `timeout` without --foreground puts the launcher in its own process GROUP and
  # signals that whole group on expiry, so a build or bridge that hangs is killed
  # as a group (measured) instead of being orphaned; -k 5 escalates to KILL.
  env "${env_args[@]}" \
    -u CRIER_HTTP_URL -u CRIER_AGENT_ID -u CRIER_AGENT_PRIVATE_KEY_FILE \
    "$TIMEOUT_BIN" -k 5 "$TIMEOUT" bash -c "$cmd" <"$frame" >"$out" 2>"$err"
  rc=$?

  out_bytes="$(wc -c <"$out" | tr -d ' ')"
  err_bytes="$(wc -c <"$err" | tr -d ' ')"

  # (2) BOUNDED READ — a timeout is a failure, never a skip.
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    _err "TIMEOUT: launcher='$cmd' did not finish within ${TIMEOUT}s (rc=$rc)."
    _err "  the whole launch, build included, is bounded by ${CHECK_MCP_STDOUT_TIMEOUT:-30}s; a"
    _err "  launcher that cannot answer one initialize frame in that budget is a FAILURE."
    head5="$(tail -n 5 "$err" 2>/dev/null)"
    [ -n "$head5" ] && _err "  launcher stderr (last 5 lines): $(_truncate "$head5")"
    rm -rf "$tmp"
    return 1
  fi

  # FAIL CLOSED — no stdout line at all means this run verified nothing.
  if [ "$out_bytes" -eq 0 ]; then
    _err "FAIL-CLOSED: launcher='$cmd' produced NO stdout line at all (rc=$rc, stderr ${err_bytes} byte(s))."
    _err "  nothing was verified about this launcher's stdout, so no green is reported."
    head5="$(tail -n 5 "$err" 2>/dev/null)"
    [ -n "$head5" ] && _err "  launcher stderr (last 5 lines): $(_truncate "$head5")"
    rm -rf "$tmp"
    return 2
  fi

  first="$(head -n 1 "$out")"

  # NEUTER-MARK[mcp-stdout-verdict]: the launcher's stdout verdict. The selftest
  # seds exactly this line (and asserts the copy changed) so a rejected fixture is
  # rejected BY THIS VERDICT and not by accident.
  if ! _is_initialize_response "$first"; then
    _err "CONTAMINATED STDOUT: launcher='$cmd'"
    _err "  the FIRST stdout line is not the JSON-RPC initialize response — a strict"
    _err "  stdio MCP client reads it before any frame and fails the handshake."
    _err "  first stdout line (truncated to ${LINE_LIMIT} chars): $(_truncate "$first")"
    _err "  launcher stderr: ${err_bytes} byte(s) — logging belongs there; this contaminant arrived on stdout."
    rm -rf "$tmp"
    return 1
  fi

  printf 'PASS — launcher=%s first_stdout_line=%s\n' "$cmd" "$first"
  _info "launcher='$cmd' rc=$rc stdout=${out_bytes} byte(s)/$(wc -l <"$out" | tr -d ' ') line(s) stderr=${err_bytes} byte(s)"
  rm -rf "$tmp"
  return 0
}

# ── main check ────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-mcp-stdout.sh — prove the documented MCP launcher's stdout carries only
JSON-RPC frames (DF-CRIER-137)

Usage:
  bash scripts/check-mcp-stdout.sh                     # both documented launchers
  bash scripts/check-mcp-stdout.sh 'CMD' ['CMD' ...]   # exactly the launchers you name

Default mode checks, in documentation order:
  make mcp
  make build-mcp && ./bin/crier-mcp

Each launcher is run with ONE MCP `initialize` frame on stdin and stdout/stderr
captured separately. The FIRST stdout line must be the JSON-RPC initialize
response; a timeout is a failure (exit 1), a launcher that prints nothing on
stdout is exit 2 with no green, and a missing validate-by-tool (make, go,
timeout) is exit 2 too — never a silent skip.

Exit codes: 0 all launchers kept the contract, 1 contaminated or timed out,
2 fail-closed (nothing verified).
EOF
}

run_check() {
  local -a cmds=("$@")
  local from_default=0 rc=0 checked=0 fails=0 nothing_verified=0 c=""

  REPO_ROOT="$(resolve_repo_root)" || return 2

  if [ "${#cmds[@]}" -eq 0 ]; then
    from_default=1
    cmds=("${DOCUMENTED_LAUNCHERS[@]}")
  fi

  if [ "$from_default" -eq 1 ]; then
    resolve_tools "default (the documented launchers)" || return 2
  else
    command -v timeout >/dev/null 2>&1 || {
      _err "'timeout' is not on PATH — refusing to run a launcher unbounded."
      _err "  mode: explicit launcher list"
      return 2
    }
    TIMEOUT_BIN="$(command -v timeout)"
  fi

  _info "repo root: $REPO_ROOT"
  _info "scope: ${#cmds[@]} launcher(s) checked"

  cd "$REPO_ROOT" || {
    _err "cannot cd to the repo root $REPO_ROOT"
    return 2
  }

  for c in "${cmds[@]}"; do
    checked=$((checked + 1))
    # NOT `if ! check_launcher …`: the message must distinguish "contaminated"
    # (exit 1) from "nothing was verified" (exit 2), so the status is captured
    # directly — `$?` after a negated command is the negation's status, not the
    # launcher's.
    check_launcher "$c"
    rc=$?
    case "$rc" in
      0) ;;
      1) fails=$((fails + 1)) ;;
      *)
        fails=$((fails + 1))
        nothing_verified=$((nothing_verified + 1))
        ;;
    esac
  done

  # The DF-CRIER-208 class: a checker that verified nothing never prints a green.
  if [ "$nothing_verified" -gt 0 ]; then
    _err "FAIL-CLOSED — $nothing_verified launcher(s) produced no stdout line at all, so this run verified nothing about them (exit 2, never a green)."
    _err "FAIL — $checked launcher(s) checked, $fails rejected"
    return 2
  fi

  if [ "$fails" -eq 0 ]; then
    _info "PASS — $checked launcher(s) checked, 0 contaminated"
    return 0
  fi

  _err "FAIL — $checked launcher(s) checked, $fails rejected"
  return 1
}

main() {
  local -a cmds=()

  while [ "$#" -gt 0 ]; do
    case "$1" in
      -h | --help | help)
        usage
        return 0
        ;;
      --)
        shift
        while [ "$#" -gt 0 ]; do
          cmds+=("$1")
          shift
        done
        ;;
      -*)
        _err "unknown option '$1' (try --help)"
        return 2
        ;;
      *)
        cmds+=("$1")
        shift
        ;;
    esac
  done

  case "$TIMEOUT" in
    '' | *[!0-9]*)
      _err "CHECK_MCP_STDOUT_TIMEOUT must be a positive integer of seconds (got '$TIMEOUT')"
      return 2
      ;;
  esac
  case "$LINE_LIMIT" in
    '' | *[!0-9]*)
      _err "CHECK_MCP_STDOUT_LINE_LIMIT must be a positive integer of characters (got '$LINE_LIMIT')"
      return 2
      ;;
  esac

  REPO_ROOT=""
  TIMEOUT_BIN=""
  run_check "${cmds[@]}"
}

main "$@"
exit $?
