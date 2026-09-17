#!/usr/bin/env bash
#
# scripts/check-shell-yaml.sh — shell-script + GitHub-workflow gate (DF-CRIER-206).
#
# WHY THIS EXISTS
# ---------------
# The commit gate is the gitreins pre-commit hook and its Tier-1 battery is
# secrets / go_build / go_lint / go_tests. NONE of those checks reads a shell
# script, a Makefile recipe or a .github/workflows/*.yml, so for a diff made
# only of such files the gate printed
#
#     Tier 1 Guards: PASS  (test mode: full)
#
# having verified nothing about that diff. Measured on this tree before this
# script existed: a workflow with an unclosed flow sequence and a .sh with an
# unterminated `if` both PASSed the gate. This checker is the missing half of
# the gate: it takes an explicit file list (default: every tracked file in the
# repo) and fails closed on the shell and workflow files inside it.
#
# USAGE
# -----
#   bash scripts/check-shell-yaml.sh                  # default: all tracked files
#   bash scripts/check-shell-yaml.sh FILE [FILE...]   # exactly the files you name
#   bash scripts/check-shell-yaml.sh --yaml-mode pyyaml FILE...
#   bash scripts/check-shell-yaml.sh --selftest
#
# WHAT IS CHECKED
# ---------------
#   shell     every file ending in .sh, plus every file whose FIRST LINE is a
#             shell shebang (bash/sh/dash/ksh/zsh/ash/mksh) whatever its
#             extension -> `bash -n <file>`
#   workflow  every .github/workflows/*.yml|*.yaml -> `actionlint <file>` when
#             actionlint is on PATH, otherwise python3 + PyYAML parse-only.
#             The mode AND the tool version are printed; if neither validator is
#             available the checker exits 2 — a missing tool is never a silent
#             skip.
#
# YAML MODES (--yaml-mode, or CHECK_SHELL_YAML_MODE)
#   auto        (default) actionlint if present, else PyYAML, else exit 2
#   actionlint  require actionlint (exit 2 when absent) — the strict linter
#   pyyaml      force the parse-only fallback (what CI uses when it cannot build
#               actionlint); exercised by --selftest in both arms
#
# actionlint is a full linter, not only a parser, so it needs to know this
# repo's custom self-hosted runner labels — declared in .github/actionlint.yaml
# (actionlint's own config mechanism; an UNDECLARED label is still rejected, so
# the config is a declaration, not a silencer). The config path in use is
# printed with the mode.
#
# EXIT CODES
#   0  every file in scope passed
#   1  at least one file was rejected (also: a NAMED file is missing/unreadable)
#   2  misuse (bad option / bad --yaml-mode) or a missing dependency
#
# A tracked file that is ABSENT from the worktree is named in a note and left out
# of the counts (no content means no syntax to verify) — but a file NAMED on the
# command line that does not exist is a hard error: the caller claimed there was
# something to check.
#
# OUTPUT CONTRACT (grep-able)
#   `PASS  shell     <file>` / `PASS  workflow  <file>` per checked file,
#   `FAIL  ...` plus the tool's own message on rejection, then
#   `scope: N shell file(s), M workflow file(s) verified`  (all passed)
#   `scope: N shell file(s), M workflow file(s) in scope — K rejected`
#   The counts are real counts of files actually checked in this run.
#
# SELFTEST — used by `make shell-yaml-selftest`; it builds its fixtures in a
# scratch dir under ${TMPDIR:-/tmp} and asserts, by running THIS script on them:
#   1. a syntactically broken .sh is REJECTED (nonzero + bash's message)
#   2. a malformed workflow YAML is REJECTED in auto mode, and the mode is named
#   3. the same malformed YAML is REJECTED with --yaml-mode pyyaml (fallback arm)
#   4. a clean shell+workflow pair is ACCEPTED with a `scope:` line carrying the
#      real counts (1 shell file, 1 workflow file) — the anti-vacuous check
#   5. a broken extensionless file with a shell shebang is REJECTED (proves the
#      shebang clause, not just the *.sh suffix)
#   `make shell-yaml-selftest` exits 0 only when all of them behaved.
#
# DEPENDENCIES: bash, git (only for the default file list), grep/sed/mktemp;
# plus one YAML validator (actionlint or python3+PyYAML) when a workflow file is
# in scope.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-shell-yaml"
YAML_MODE_REQUESTED="${CHECK_SHELL_YAML_MODE:-auto}"
YAML_RESOLVED=""
ACTIONLINT_BIN=""
YAML_TOOL_DESC=""

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

# ── file classification ───────────────────────────────────────────────────────

# Workflows: .github/workflows/*.yml|*.yaml, repo-relative or absolute.
_is_workflow() {
  case "$1" in
    .github/workflows/*.yml | .github/workflows/*.yaml) return 0 ;;
    */.github/workflows/*.yml | */.github/workflows/*.yaml) return 0 ;;
  esac
  return 1
}

# Shell: *.sh, or any extensionless/other file whose first line is a shell
# shebang. A .sh that is not a shell shebang is still checked (the suffix is
# the repo's own convention for "this is a shell script").
_is_shell() {
  local f="$1" first=""
  case "$f" in *.sh) return 0 ;; esac
  IFS= read -r first <"$f" 2>/dev/null || return 1
  case "$first" in '#!'*) ;; *) return 1 ;; esac
  printf '%s\n' "$first" |
    grep -qE '^#!.*([/ ])(bash|sh|dash|ksh|zsh|ash|mksh)([[:space:]]|$)' || return 1
  return 0
}

# ── the two checks ────────────────────────────────────────────────────────────

# CHECK_DIAG carries the rejected file's tool output up to the reporter, so the
# report reads `FAIL <file>` first and the tool's own message underneath it.
CHECK_DIAG=""

# _check_shell <file> — bash -n; returns nonzero when bash rejects the file.
_check_shell() {
  local f="$1" out="" rc=0
  out="$(bash -n "$f" 2>&1)"
  rc=$?
  [ "$rc" -eq 0 ] && return 0
  CHECK_DIAG="$out"
  return 1
}

# _check_workflow <file> — actionlint or the PyYAML parse fallback.
_check_workflow() {
  local f="$1" out="" rc=0
  case "$YAML_RESOLVED" in
    actionlint)
      out="$("$ACTIONLINT_BIN" "$f" 2>&1)"
      rc=$?
      ;;
    pyyaml)
      out="$(python3 -c 'import sys, yaml
try:
    with open(sys.argv[1], "rb") as fh:
        list(yaml.safe_load_all(fh))
except yaml.YAMLError as exc:
    print("PyYAML: %s" % exc, file=sys.stderr)
    sys.exit(1)' "$f" 2>&1)"
      rc=$?
      ;;
    *)
      _err "internal: no YAML mode resolved"
      return 2
      ;;
  esac
  [ "$rc" -eq 0 ] && return 0
  CHECK_DIAG="$out"
  return 1
}

# ── YAML mode resolution + version attribution ────────────────────────────────

_have_actionlint() { command -v actionlint >/dev/null 2>&1; }
_have_pyyaml() { command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; }

# Actionlint's config is per-project (it reads <project>/.github/actionlint.yaml).
# Report the config that actionlint will actually use for <file>, resolved from
# the file's own directory — never from this script's cwd, which could attribute
# a config actionlint never read.
_actionlint_config_for() {
  local f="$1" dir root cfg
  dir="$(cd "$(dirname "$f")" 2>/dev/null && pwd)" || return 0
  root="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null)" || return 0
  [ -n "$root" ] || return 0
  cfg="$root/.github/actionlint.yaml"
  [ -f "$cfg" ] && printf '%s' "$cfg"
  return 0
}

resolve_yaml_mode() { # <mode> -> sets YAML_RESOLVED / ACTIONLINT_BIN / YAML_TOOL_DESC
  local mode="$1" ver="" cfg=""
  case "$mode" in
    auto)
      if _have_actionlint; then
        mode=actionlint
      elif _have_pyyaml; then
        mode=pyyaml
      else
        _err "no YAML validator available: actionlint is not on PATH and python3+yaml is not importable."
        _err "  refusing to skip the workflow check — install actionlint (or PyYAML) and re-run."
        return 2
      fi
      ;;
    actionlint)
      if ! _have_actionlint; then
        _err "--yaml-mode actionlint was requested but actionlint is not on PATH."
        return 2
      fi
      ;;
    pyyaml)
      if ! _have_pyyaml; then
        _err "--yaml-mode pyyaml was requested but python3 cannot import yaml (PyYAML)."
        return 2
      fi
      ;;
    *)
      _err "unknown yaml mode '$mode' (expected auto|actionlint|pyyaml)"
      return 2
      ;;
  esac

  YAML_RESOLVED="$mode"
  case "$mode" in
    actionlint)
      ACTIONLINT_BIN="$(command -v actionlint)"
      ver="$(actionlint --version 2>/dev/null | head -n 1)"
      [ -n "$ver" ] || ver="unknown version"
      cfg="$(_actionlint_config_for "${YAML_PROBE_FILE:-$PWD}")"
      if [ -n "$cfg" ]; then
        YAML_TOOL_DESC="actionlint $ver (config: $cfg)"
      else
        YAML_TOOL_DESC="actionlint $ver (no .github/actionlint.yaml found — actionlint defaults)"
      fi
      ;;
    pyyaml)
      ver="$(python3 -c 'import yaml; print(yaml.__version__)' 2>/dev/null)"
      [ -n "$ver" ] || ver="unknown version"
      YAML_TOOL_DESC="PyYAML $ver parse-only fallback (actionlint not used)"
      ;;
  esac
  return 0
}

# ── main check ────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-shell-yaml.sh — shell + workflow gate (DF-CRIER-206)

Usage:
  bash scripts/check-shell-yaml.sh [--yaml-mode auto|actionlint|pyyaml] [FILE...]
  bash scripts/check-shell-yaml.sh --selftest

With no FILE arguments every tracked file is considered (git ls-files); only the
shell scripts and .github/workflows/*.yml|*.yaml inside that set are checked,
and the real counts are printed:

  scope: 9 shell file(s), 3 workflow file(s) verified

Exit codes: 0 all passed, 1 at least one rejected, 2 misuse/missing dependency.
EOF
}

run_check() { # <files...>
  local -a files=("$@")
  local -a shell_files=() yaml_files=() other_files=()
  local from_default=0 repo_root="" tracked="" n_tracked=0
  local f rc=0 fails=0 checked=0 missing=0

  command -v bash >/dev/null 2>&1 || {
    _err "bash is required."
    return 2
  }

  repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || repo_root=""

  if [ "${#files[@]}" -eq 0 ]; then
    from_default=1
    command -v git >/dev/null 2>&1 || {
      _err "git is required to derive the default (tracked) file list."
      return 2
    }
    [ -n "$repo_root" ] || {
      _err "not inside a git checkout — pass an explicit file list instead."
      return 2
    }
    tracked="$(git -C "$repo_root" ls-files)" || {
      _err "git ls-files failed in $repo_root"
      return 2
    }
    [ -n "$tracked" ] || {
      _err "git ls-files returned no tracked files — refusing to report a vacuous PASS."
      return 2
    }
    while IFS= read -r f; do
      [ -n "$f" ] || continue
      files+=("$repo_root/$f")
      n_tracked=$((n_tracked + 1))
    done <<<"$tracked"
  fi

  # Classify. Nothing is skipped silently:
  #   - a NAMED file that is missing or unreadable is a FAILURE (the caller
  #     asserted it should be checked),
  #   - in the default (whole-repo) mode a TRACKED file that is missing from the
  #     worktree has no content to check — it is named in a note, excluded from
  #     the scope counts, and does not fail the run (a file that does not exist
  #     cannot carry a syntax error; the deletion itself is the git diff's
  #     business),
  #   - an UNREADABLE file is a failure in both modes: content exists and this
  #     checker cannot verify it.
  local -a missing_tracked_list=()
  for f in "${files[@]}"; do
    if [ ! -e "$f" ]; then
      if [ "$from_default" -eq 1 ]; then
        missing_tracked_list+=("$f")
      else
        _err "named file does not exist: $f (refusing to skip it silently)"
        missing=$((missing + 1))
      fi
      continue
    fi
    if [ ! -r "$f" ]; then
      _err "file is not readable: $f (refusing to skip it silently)"
      missing=$((missing + 1))
      continue
    fi
    if _is_workflow "$f"; then
      yaml_files+=("$f")
    elif _is_shell "$f"; then
      shell_files+=("$f")
    else
      other_files+=("$f")
    fi
  done

  if [ "$from_default" -eq 1 ]; then
    _info "scope source: git ls-files ($n_tracked tracked file(s)) from $repo_root"
    if [ "${#missing_tracked_list[@]}" -gt 0 ]; then
      local shown=0
      printf '%s: note: %d tracked file(s) are absent from the worktree and were not checked (no content = no syntax to verify): %s' \
        "$PROG" "${#missing_tracked_list[@]}" "$repo_root/" >&2
      for f in "${missing_tracked_list[@]}"; do
        shown=$((shown + 1))
        [ "$shown" -le 10 ] && printf '%s ' "${f#"$repo_root/"}" >&2
      done
      [ "${#missing_tracked_list[@]}" -gt 10 ] && printf '(+%d more)' "$((${#missing_tracked_list[@]} - 10))" >&2
      printf '\n' >&2
    fi
  fi

  # Resolve + announce the YAML mode BEFORE checking, so the evidence is
  # attributable even if a later check aborts.
  if [ "${#yaml_files[@]}" -gt 0 ]; then
    YAML_PROBE_FILE="${yaml_files[0]}"
    resolve_yaml_mode "$YAML_MODE_REQUESTED" || return 2
    _info "yaml mode: $YAML_TOOL_DESC"
  elif [ "$YAML_MODE_REQUESTED" = "actionlint" ]; then
    # Explicitly requested: honour the request even with nothing to check, so a
    # missing tool is still an error rather than a quiet no-op.
    resolve_yaml_mode actionlint || return 2
    _info "yaml mode: $YAML_TOOL_DESC (no workflow file in scope)"
  fi

  for f in "${shell_files[@]}"; do
    checked=$((checked + 1))
    if _check_shell "$f"; then
      printf 'PASS  shell     %s\n' "$f"
    else
      printf 'FAIL  shell     %s\n' "$f"
      [ -n "$CHECK_DIAG" ] && printf '%s\n' "$CHECK_DIAG" | sed 's/^/      /'
      _err "shell syntax check failed: $f"
      fails=$((fails + 1))
    fi
  done

  for f in "${yaml_files[@]}"; do
    checked=$((checked + 1))
    if _check_workflow "$f"; then
      printf 'PASS  workflow  %s  [%s]\n' "$f" "$YAML_RESOLVED"
    else
      printf 'FAIL  workflow  %s  [%s]\n' "$f" "$YAML_RESOLVED"
      [ -n "$CHECK_DIAG" ] && printf '%s\n' "$CHECK_DIAG" | sed 's/^/      /'
      _err "workflow check failed: $f"
      fails=$((fails + 1))
    fi
  done

  # Anything handed to the checker that is neither: say so out loud (a silent
  # skip is the defect class this script exists to close).
  if [ "${#other_files[@]}" -gt 0 ]; then
    if [ "$from_default" -eq 1 ]; then
      _info "note: $((n_tracked - checked - ${#missing_tracked_list[@]})) tracked file(s) are neither shell scripts nor workflow YAML and are out of scope"
    else
      local shown=0
      printf 'note: %d supplied file(s) are neither shell scripts nor workflow YAML (not checked):' "${#other_files[@]}" >&2
      for f in "${other_files[@]}"; do
        shown=$((shown + 1))
        if [ "$shown" -le 10 ]; then
          printf ' %s' "$f" >&2
        fi
      done
      if [ "${#other_files[@]}" -gt 10 ]; then
        printf ' (+%d more)' "$((${#other_files[@]} - 10))" >&2
      fi
      printf '\n' >&2
    fi
  fi

  if [ "$fails" -eq 0 ] && [ "$missing" -eq 0 ]; then
    _info "scope: ${#shell_files[@]} shell file(s), ${#yaml_files[@]} workflow file(s) verified"
    _info "PASS — $checked file(s) checked, 0 rejected"
    return 0
  fi

  _info "scope: ${#shell_files[@]} shell file(s), ${#yaml_files[@]} workflow file(s) in scope — $((fails + missing)) rejected"
  _err "FAIL — $checked file(s) checked, $((fails + missing)) rejected"
  return 1
}

# ── selftest ──────────────────────────────────────────────────────────────────

_selftest_cleanup() { # EXIT trap for the selftest scratch dir (global var only)
  if [ -n "${_SELFTEST_TMP:-}" ]; then
    rm -rf "$_SELFTEST_TMP"
    _SELFTEST_TMP=""
  fi
  return 0
}

_selftest() {
  local fails=0 checks=0
  # The scratch dir is held in a GLOBAL (not a local) so the EXIT trap can still
  # read it after this function returns — a trap that quotes a function-local
  # under `set -u` dies with "unbound variable" and leaks the directory.
  _SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-shell-yaml-selftest.XXXXXX")" || {
    _err "selftest: mktemp -d failed"
    return 1
  }
  local tmp="$_SELFTEST_TMP"
  trap '_selftest_cleanup' EXIT

  mkdir -p "$tmp/.github/workflows"

  # clean pair (must be ACCEPTED by both arms)
  printf '#!/usr/bin/env bash\nset -euo pipefail\necho ok\n' >"$tmp/clean.sh"
  cat >"$tmp/.github/workflows/clean.yml" <<'YAML'
name: selftest
on: push
jobs:
  ok:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
YAML

  # broken fixtures (must be REJECTED)
  printf '#!/usr/bin/env bash\nif [ 1 -eq 1 ]; then\necho missing fi\n' >"$tmp/broken.sh"
  printf '#!/usr/bin/env bash\nif [ 1 -eq 1 ]; then\necho no fi\n' >"$tmp/broken-tool"
  chmod +x "$tmp/broken-tool"
  printf 'name: broken\non: [push\n' >"$tmp/.github/workflows/broken.yml"

  local out="" rc=0

  # 1. broken .sh rejected, bash's own message surfaced
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/broken.sh" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a syntactically broken .sh was ACCEPTED (rc=0)\n' "$PROG" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "FAIL  shell     $tmp/broken.sh"; then
    printf '%s selftest: FAIL: broken .sh was rejected but the report did not name it\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'syntax error'; then
    printf '%s selftest: FAIL: broken .sh was rejected without bash'"'"'s message\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a broken .sh is rejected (rc=%d) and bash'"'"'s message is printed\n' "$rc"
  fi

  # 2. malformed workflow rejected in auto mode, mode named
  checks=$((checks + 1))
  out="$(bash "$SELF" --yaml-mode auto "$tmp/.github/workflows/broken.yml" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a malformed workflow YAML was ACCEPTED in auto mode (rc=0)\n' "$PROG" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "FAIL  workflow  $tmp/.github/workflows/broken.yml"; then
    printf '%s selftest: FAIL: malformed workflow rejected but the report did not name it\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -qE 'yaml mode: (actionlint|PyYAML)'; then
    printf '%s selftest: FAIL: malformed workflow rejected without naming the mode that ran\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a malformed workflow is rejected in auto mode and the mode is named (%s)\n' "$(printf '%s' "$out" | grep -m1 'yaml mode:')"
  fi

  # 3. the parse-only fallback arm rejects it too (what CI uses)
  checks=$((checks + 1))
  out="$(bash "$SELF" --yaml-mode pyyaml "$tmp/.github/workflows/broken.yml" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: the PyYAML fallback ACCEPTED a malformed workflow (rc=0)\n' "$PROG" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'yaml mode: PyYAML'; then
    printf '%s selftest: FAIL: the fallback arm ran without naming PyYAML\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: the PyYAML fallback arm rejects a malformed workflow and names itself\n'
  fi

  # 4. clean pair ACCEPTED, with real counts in the scope line (anti-vacuous)
  checks=$((checks + 1))
  out="$(bash "$SELF" --yaml-mode auto "$tmp/clean.sh" "$tmp/.github/workflows/clean.yml" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the clean shell+workflow pair was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'scope: 1 shell file(s), 1 workflow file(s) verified'; then
    printf '%s selftest: FAIL: the clean run did not report real counts (scope line)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a clean shell+workflow pair is accepted and the scope line counts both files\n'
  fi

  # 5. extensionless file with a shell shebang is in scope (shebang clause)
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/broken-tool" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a broken extensionless shell script (shebang only) was ACCEPTED\n' "$PROG" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "FAIL  shell     $tmp/broken-tool"; then
    printf '%s selftest: FAIL: the shebang-detected file was not reported as a shell failure\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a broken extensionless script with a shell shebang is detected and rejected\n'
  fi

  if [ "$fails" -ne 0 ]; then
    printf '%s selftest: %d/%d checks behaved — FAIL\n' "$PROG" "$((checks - fails))" "$checks" >&2
    return 1
  fi
  printf '%s selftest: %d/%d checks behaved\n' "$PROG" "$checks" "$checks"
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

main() {
  local -a files=()
  local selftest=0

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --selftest)
        selftest=1
        shift
        ;;
      --yaml-mode)
        [ "$#" -ge 2 ] || {
          _err "--yaml-mode needs a value (auto|actionlint|pyyaml)"
          return 2
        }
        YAML_MODE_REQUESTED="$2"
        shift 2
        ;;
      --yaml-mode=*)
        YAML_MODE_REQUESTED="${1#*=}"
        shift
        ;;
      -h | --help | help)
        usage
        return 0
        ;;
      --)
        shift
        while [ "$#" -gt 0 ]; do
          files+=("$1")
          shift
        done
        ;;
      -*)
        _err "unknown option '$1' (try --help)"
        return 2
        ;;
      *)
        files+=("$1")
        shift
        ;;
    esac
  done

  if [ "$selftest" -eq 1 ]; then
    _selftest
    return $?
  fi

  case "$YAML_MODE_REQUESTED" in
    auto | actionlint | pyyaml) ;;
    *)
      _err "unknown yaml mode '$YAML_MODE_REQUESTED' (expected auto|actionlint|pyyaml)"
      return 2
      ;;
  esac

  run_check "${files[@]}"
}

main "$@"
exit $?
