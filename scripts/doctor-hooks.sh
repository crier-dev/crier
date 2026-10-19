#!/usr/bin/env bash
# DF-CRIER-421: read-only proof that THIS repository's gate is armed.
# Run from anywhere inside the checkout/worktree, not relative to this script.
# Exit 1 names the failing condition; exit 0 prints exactly one line per hook.
# Repair: make install-hooks. This verifies installation, not that Git cannot
# bypass hooks (--no-verify, command-line config and later writes still can).
set -uo pipefail

refuse() { printf 'doctor-hooks: ERROR: %s; repair: make install-hooks\n' "$*" >&2; return 1; }

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
    if [ "$hook" = pre-commit ]; then
      grep -Fqx 'bash "$BOARD_CHECKER" -q --staged-index' "$dst" || { refuse "installed pre-commit lacks board arm marker (bash board checker --staged-index) ($dst)"; return 1; }
    fi
    cmp -s "$src" "$dst" || { refuse "installed $hook differs from scripts/hooks/$hook ($dst)"; return 1; }
  done
  # Print success only after ALL checks, never a partial green before a refusal.
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    printf 'doctor-hooks: OK %s (executable, byte-identical; repository hooks at %s)\n' "$hook" "$hooks"
  done
}

doctor_hooks "$@"
