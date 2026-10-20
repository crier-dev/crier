#!/usr/bin/env bash
# DF-CRIER-421: read-only proof that THIS repository's gate is armed.
# DF-CRIER-430: EVERY entrypoint's board arm is checked by MARKER, not only by bytes.
#
# Run from anywhere inside the checkout/worktree, not relative to this script.
# Exit 1 names the failing condition; exit 0 prints exactly one line per hook.
# Repair: make install-hooks. This verifies installation, not that Git cannot
# bypass hooks (--no-verify, command-line config and later writes still can).
#
# WHY A MARKER CHECK EXISTS (DF-CRIER-430, measured at 596825f0)
# --------------------------------------------------------------
# Installed hooks are compared byte-for-byte against their tracked sources, and
# that comparison cannot see a stale GENERATION: edit the tracked source down to
# `exit 0 # DISARMED` and reinstall it, and the installed hook is byte-identical
# to the source it was made from, so cmp passes. The doctor marker-checked
# pre-commit only, so the same disarm came out two different ways:
#
#   scripts/hooks/pre-merge-commit -> exit 0, `OK pre-merge-commit (executable, byte-identical …)`
#   scripts/hooks/pre-commit       -> exit 1, `installed pre-commit lacks board arm marker …`
#
# The merge and applypatch entrypoints exist to run the board gate, so their whole
# function IS the board arm. Each hook must therefore also carry the lines its
# tracked source exists to run, and a missing line refuses naming the hook and the
# line:
#
#   pre-commit        board arm marker:        bash "$BOARD_CHECKER" -q --staged-index
#   pre-merge-commit  board arm marker:        exec bash "$checker" --staged-index
#                     gate-scope disclosure:   the `gate scope: pre-merge-commit: …` line
#   pre-applypatch    board arm marker:        exec bash "$checker" --staged-index
#                     gate-scope disclosure:   the `gate scope: pre-applypatch: …` line
#
# The marker check runs BEFORE the byte comparison, so a disarmed hook is refused
# as a missing board arm rather than as byte drift.
#
# LIMITS: this reads LINES, not behaviour — a hook that keeps every marker and
# does something else still passes, and the doctor never runs the gate itself.
set -uo pipefail

refuse() { printf 'doctor-hooks: ERROR: %s; repair: make install-hooks\n' "$*" >&2; return 1; }

# require_marker <hook> <installed-path> <label> <line>
# A missing marker is refused by naming the hook AND the exact missing line; the
# match is a whole-line fixed-string comparison, so a commented-out or reworded
# line does not count as present.
require_marker() {
  local hook="$1" dst="$2" label="$3" marker="$4"
  grep -Fqx -- "$marker" "$dst" ||
    { refuse "installed $hook lacks $label ($marker) ($dst)"; return 1; }
}

doctor_hooks() {
  local root common hooks local_path effective_path resolved hook src dst
  root="$(git rev-parse --show-toplevel 2>/dev/null)" || { refuse "not inside a Git worktree"; return 1; }
  cd "$root" || return 1
  # Resolve relative --git-common-dir output from the repo root, and canonicalize
  # directory aliases before comparing. A linked worktree's .git is a FILE.
  common="$(git rev-parse --git-common-dir 2>/dev/null)" || { refuse "cannot resolve git-common-dir"; return 1; }
  common="$(cd "$common" && pwd -P)" || { refuse "git-common-dir is not a directory: $common"; return 1; }
  hooks="$common/hooks"
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    src="$root/scripts/hooks/$hook"
    [ -f "$src" ] || { refuse "source scripts/hooks/$hook is missing ($src)"; return 1; }
  done
  local_path="$(git config --local --path --get core.hooksPath 2>/dev/null)" || { refuse "local core.hooksPath is unset (global hooksPath is not sufficient)"; return 1; }
  [ -n "$local_path" ] || { refuse "local core.hooksPath is empty"; return 1; }
  resolved="$(cd "$local_path" 2>/dev/null && pwd -P)" || { refuse "local core.hooksPath does not resolve to a directory: $local_path (expected $hooks)"; return 1; }
  [ "$resolved" = "$hooks" ] || { refuse "local core.hooksPath resolves to $resolved, expected repository git-common-dir/hooks $hooks"; return 1; }
  # A --worktree config (or injected command config) can override --local.
  effective_path="$(git config --path --get core.hooksPath 2>/dev/null)" || { refuse "effective core.hooksPath is unset"; return 1; }
  resolved="$(cd "$effective_path" 2>/dev/null && pwd -P)" || { refuse "effective core.hooksPath does not resolve to a directory: $effective_path"; return 1; }
  [ "$resolved" = "$hooks" ] || { refuse "effective core.hooksPath resolves to $resolved, expected $hooks"; return 1; }
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    src="$root/scripts/hooks/$hook"
    dst="$hooks/$hook"
    [ -f "$dst" ] || { refuse "installed $hook is missing ($dst)"; return 1; }
    [ -x "$dst" ] || { refuse "installed $hook is not executable ($dst)"; return 1; }
    # DF-CRIER-430: the marker check runs before cmp — a stale generation is
    # byte-identical to its own disarmed source, so bytes cannot refuse it.
    case "$hook" in
      pre-commit)
        require_marker "$hook" "$dst" "board arm marker" 'bash "$BOARD_CHECKER" -q --staged-index' || return 1
        ;;
      pre-merge-commit)
        require_marker "$hook" "$dst" "board arm marker" 'exec bash "$checker" --staged-index' || return 1
        require_marker "$hook" "$dst" "gate-scope disclosure line" "printf '%s\n' 'gate scope: pre-merge-commit: this entrypoint validates the board only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and Makefile/Dockerfile checks are NOT RUN here).'" || return 1
        ;;
      pre-applypatch)
        require_marker "$hook" "$dst" "board arm marker" 'exec bash "$checker" --staged-index' || return 1
        require_marker "$hook" "$dst" "gate-scope disclosure line" "printf '%s\n' 'gate scope: pre-applypatch: this entrypoint validates the board only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and Makefile/Dockerfile checks are NOT RUN here).'" || return 1
        ;;
    esac
    cmp -s "$src" "$dst" || { refuse "installed $hook differs from scripts/hooks/$hook ($dst)"; return 1; }
  done
  # Print success only after ALL checks, never a partial green before a refusal.
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    printf 'doctor-hooks: OK %s (executable, byte-identical, board-arm markers present; repository hooks at %s)\n' "$hook" "$hooks"
  done
}

doctor_hooks "$@"
