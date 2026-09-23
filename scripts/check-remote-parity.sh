#!/usr/bin/env bash
#
# scripts/check-remote-parity.sh — dual-remote parity gate (REV5-CRIER-001).
#
# WHY THIS EXISTS
# ---------------
# This repo has two remotes: `origin` (github.com/crier-dev/crier) is the PRIMARY,
# and `gitlab` (gitlab.readydedis.com:totalwindup/crier) is a subordinate CONTENT
# MIRROR that is supposed to carry exactly the same main. Nothing in the repo said
# so, and nothing checked it — the tick battery compared the two by hand, once per
# tick, with a `git rev-list --left-right --count origin/main...gitlab/main` typed
# into a shell. Measured history of the failures that hand-check was catching:
#
#   - ticks 143-149 briefly carried TWO diverged lineages: commits landed on one
#     remote that the other never saw, and a fast-forward push could not repair it;
#   - twice a tick never pushed the mirror at all (measured around ticks 79 and
#     101), so `gitlab` silently sat N commits behind `origin` — a mirror that is
#     behind is worse than an absent one, because it answers `clone` and `fetch`
#     with a stale tree.
#
# A hand-check is not a gate: it is run by whoever remembers, on whatever the local
# refs happened to be, and its result lives in a chat log rather than in the repo.
# This script is the repo-owned version of that check. It is the fourth arm of the
# same pattern as scripts/check-gofmt.sh (DF-CRIER-189),
# scripts/check-shell-yaml.sh (DF-CRIER-206) and scripts/check-make-docker.sh
# (DF-CRIER-209): a small, attributable, fail-closed checker plus a selftest that
# proves it still rejects what it claims to reject.
#
# USAGE
# -----
#   bash scripts/check-remote-parity.sh
#   PARITY_REMOTE_A=origin PARITY_REMOTE_B=mirror PARITY_BRANCH=main \
#     bash scripts/check-remote-parity.sh
#
#   PARITY_REMOTE_A   name of the primary remote                (default: origin)
#   PARITY_REMOTE_B   name of the mirror remote                 (default: gitlab)
#   PARITY_BRANCH     branch name to compare on both remotes    (default: main)
#
# The three overrides exist so the selftest can point this checker at throwaway
# bare repositories under ${TMPDIR:-/tmp} without touching the real remotes.
#
# WHAT IS CHECKED
# ---------------
#   1. both named remotes exist in this checkout (git remote get-url);
#   2. `git fetch <remote>` succeeds for BOTH of them — remotes are expected to be
#      reachable from the host that runs this (the tick battery / dev host); a
#      failed or unusable remote is NOT tolerated as "unknown" (see FAIL CLOSED);
#   3. the remote-tracking refs <A>/<branch> and <B>/<branch> exist after the
#      fetch — a remote that answered the fetch but has no such branch is a
#      refusal, not a pass;
#   4. `git rev-list --left-right --count <A>/<branch>...<B>/<branch>` — exactly
#      0<TAB>0 is parity. Anything else is drift, reported loudly with the fix.
#
# WHAT THE TWO COUNTS MEAN, AND WHICH SIDE THEY NAME
# --------------------------------------------------
# `git rev-list --left-right --count A/branch...B/branch` prints `<left><TAB><right>`:
#
#   left  = commits reachable from A/branch but NOT from B/branch  -> A-ONLY
#   right = commits reachable from B/branch but NOT from A/branch  -> B-ONLY
#
# so the two readings a log reader must never mix up are:
#
#   left == 0, right == N > 0   B is AHEAD of A  =>  A is BEHIND  =>  push A
#   right == 0, left == N > 0   A is AHEAD of B  =>  B is BEHIND  =>  push B
#
# i.e. the UNIQUE side is the side that is ahead, and the ZERO side is the side
# that is behind and needs the push. The mirror-behind case the tick battery kept
# hitting (origin has commits gitlab never got) is therefore
# `left = N, right = 0`, and its fix is `git push gitlab main`.
#
# DUAL LINEAGE (both counts nonzero) is the shape no push can reconcile: it needs a
# human decision about which lineage is canonical and an explicit force-push. This
# checker NEVER force-pushes and never prints a copy-pasteable force-push command —
# it prints the escalation, both tips and both counts, and stops.
#
# FAIL CLOSED (exit 2 — never a vacuous PASS)
# -------------------------------------------
# Every one of these is exit 2 with the missing thing NAMED, because each of them
# would otherwise be reported as parity by a checker that only looked at numbers it
# could not compute:
#   - git is not on PATH, or not inside a git checkout;
#   - PARITY_BRANCH is empty, or PARITY_REMOTE_A == PARITY_REMOTE_B;
#   - remote A (or B) is not configured in this checkout;
#   - `git fetch <remote>` failed for A or B;
#   - <A>/<branch> (or <B>/<branch>) does not exist after the fetch;
#   - `git rev-list --left-right --count` failed or did not print two integers.
# A missing remote, a missing remote-tracking branch or a failed fetch can never
# come back as "0 0, in parity" — that is the blank green this arm exists to remove.
#
# EXIT CODES
#   0  exact parity (0 0) — the two remotes carry the same <branch>
#   1  drift: one side behind (one push recipe printed) or dual lineage (escalated)
#   2  misuse, or a fail-closed condition (missing remote / missing remote-tracking
#      branch / failed fetch / missing git / not a git checkout)
#
# OUTPUT CONTRACT (grep-able)
#   `check-remote-parity: remotes: A=<name> (<url>) B=<name> (<url>)`
#   `check-remote-parity: branch: <branch>`
#   `check-remote-parity: fetched: <A>, <B>`
#   `check-remote-parity: counts: left(<A>-only)=<n> right(<B>-only)=<m>`
#   `PASS  parity    <A>/<branch> == <B>/<branch> (<left> <right>)`
#   `FAIL  parity    <A>/<branch> vs <B>/<branch> (<A>-only=<left> <B>-only=<right>)`
#   `check-remote-parity: PASS — ...` / `check-remote-parity: FAIL — ...`
#   The counts printed are the counts this run actually computed.
#
# DEPENDENCIES: bash, git (with network/ssh access to both remotes), grep/sed/cmp
# only in the selftest. There is no fallback fetch: a remote this host cannot reach
# is exit 2 naming it, never a skip.
#
# SELFTEST: scripts/check-remote-parity-selftest.sh (make parity-selftest) builds
# throwaway bare repositories under ${TMPDIR:-/tmp} and proves the parity acceptance,
# both one-sided drifts with their own recipe, the dual-lineage escalation, the
# missing-remote and missing-branch and failed-fetch refusals, and — NEUTER PROOF —
# that a copy of this script whose verdict call is forced to success ACCEPTS the
# fixture this script rejects.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-remote-parity"

REMOTE_A="${PARITY_REMOTE_A:-origin}"
REMOTE_B="${PARITY_REMOTE_B:-gitlab}"
BRANCH="${PARITY_BRANCH:-main}"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

# ── remote helpers ────────────────────────────────────────────────────────────

# _remotes_present — space-separated list of configured remote names, for the
# "what WAS missing" part of a fail-closed message.
_remotes_present() {
  git remote 2>/dev/null | tr '\n' ' '
}

# ── the verdict ───────────────────────────────────────────────────────────────

# _parity_verdict <left> <right> — returns 0 on exact parity, 1 on drift after
# printing the loud diagnosis + fix recipe. The counts are already printed by the
# caller, so this function owns only the VERDICT and the remedy.
_parity_verdict() {
  local left="$1" right="$2"

  if [ "$left" -eq 0 ] && [ "$right" -eq 0 ]; then
    return 0
  fi

  printf '%s\n' "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
  printf '%s: FAIL — REMOTE PARITY DRIFT: %s/%s and %s/%s are NOT in parity.\n' \
    "$PROG" "$REMOTE_A" "$BRANCH" "$REMOTE_B" "$BRANCH" >&2
  printf '  left  (%s-only commits) = %s\n' "$REMOTE_A" "$left" >&2
  printf '  right (%s-only commits) = %s\n' "$REMOTE_B" "$right" >&2
  printf '  tips: %s/%s=%s  %s/%s=%s\n' \
    "$REMOTE_A" "$BRANCH" "$TIP_A_SHORT" "$REMOTE_B" "$BRANCH" "$TIP_B_SHORT" >&2

  if [ "$left" -eq 0 ] && [ "$right" -gt 0 ]; then
    # B-only commits exist, so A is the side that is behind.
    printf '  DIAGNOSIS: the %s remote is BEHIND %s by %s commit(s) — %s carries commits %s does not.\n' \
      "$REMOTE_A" "$REMOTE_B" "$right" "$REMOTE_B" "$REMOTE_A" >&2
    printf '  FIX: git push %s %s\n' "$REMOTE_A" "$BRANCH" >&2
    printf '       (run it from a checkout whose %s is at %s/%s, then re-run this checker)\n' \
      "$BRANCH" "$REMOTE_B" "$BRANCH" >&2
  elif [ "$right" -eq 0 ] && [ "$left" -gt 0 ]; then
    # A-only commits exist, so B is the side that is behind.
    printf '  DIAGNOSIS: the %s remote is BEHIND %s by %s commit(s) — %s carries commits %s does not.\n' \
      "$REMOTE_B" "$REMOTE_A" "$left" "$REMOTE_A" "$REMOTE_B" >&2
    printf '  FIX: git push %s %s\n' "$REMOTE_B" "$BRANCH" >&2
    printf '       (run it from a checkout whose %s is at %s/%s, then re-run this checker)\n' \
      "$BRANCH" "$REMOTE_A" "$BRANCH" >&2
  else
    printf '  DIAGNOSIS: DUAL LINEAGE — BOTH remotes carry commits the other lacks (%s-only=%s, %s-only=%s). No push reconciles them.\n' \
      "$REMOTE_A" "$left" "$REMOTE_B" "$right" >&2
    printf '  FIX: ESCALATE — this is a force-push decision and it is NOT taken automatically.\n' >&2
    printf '       Compare the two tips above, decide which lineage is canonical, publish that one by hand, then re-run this checker.\n' >&2
    printf '       This checker never rewrites history and never prints a copy-pasteable force command.\n' >&2
  fi
  printf '%s\n' "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!" >&2
  return 1
}

# ── main check ────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-remote-parity.sh — dual-remote parity gate (REV5-CRIER-001)

Usage:
  bash scripts/check-remote-parity.sh

Env:
  PARITY_REMOTE_A   primary remote name            (default: origin)
  PARITY_REMOTE_B   mirror remote name             (default: gitlab)
  PARITY_BRANCH     branch compared on both        (default: main)

Fetches both remotes, then requires
`git rev-list --left-right --count A/<branch>...B/<branch>` to be exactly 0 0.

  left  = A-only commits (A ahead)  -> a nonzero left means B is BEHIND
  right = B-only commits (B ahead)  -> a nonzero right means A is BEHIND

One side behind:  exit 1 and the recipe `git push <the-behind-remote> <branch>`.
Dual lineage:     exit 1 and an ESCALATION — this checker never force-pushes.

Exit codes: 0 parity, 1 drift (behind or dual lineage), 2 misuse or fail-closed
(git missing / not a checkout / empty branch / A == B / missing remote / failed
fetch / missing remote-tracking branch) — always naming what was missing.
EOF
}

run_check() {
  local repo_root="" url_a="" url_b="" ref_a="" ref_b="" tip_a="" tip_b=""
  local counts="" left="" right="" present=""

  command -v git >/dev/null 2>&1 || {
    _err "git is not on PATH — cannot compare remote parity."
    return 2
  }

  case "$BRANCH" in
    '')
      _err "PARITY_BRANCH is empty — refusing to guess a branch."
      return 2
      ;;
  esac

  if [ "$REMOTE_A" = "$REMOTE_B" ]; then
    _err "PARITY_REMOTE_A and PARITY_REMOTE_B are both '$REMOTE_A' — comparing a remote with itself is a vacuous parity."
    return 2
  fi

  repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || repo_root=""
  [ -n "$repo_root" ] || {
    _err "not inside a git checkout — refusing to report parity for a tree with no remotes."
    return 2
  }

  present="$(_remotes_present)"

  url_a="$(git -C "$repo_root" remote get-url "$REMOTE_A" 2>/dev/null)" || url_a=""
  if [ -z "$url_a" ]; then
    _err "remote A '$REMOTE_A' does not exist in $repo_root (configured remotes: ${present:-none})."
    return 2
  fi
  url_b="$(git -C "$repo_root" remote get-url "$REMOTE_B" 2>/dev/null)" || url_b=""
  if [ -z "$url_b" ]; then
    _err "remote B '$REMOTE_B' does not exist in $repo_root (configured remotes: ${present:-none})."
    return 2
  fi

  _info "remotes: A=$REMOTE_A ($url_a) B=$REMOTE_B ($url_b)"
  _info "branch: $BRANCH"
  _info "checkout: $repo_root"

  # Fetch BOTH remotes. A remote this host cannot reach is exit 2 naming it: the
  # alternative — treating an unfetched remote as "no drift" — is exactly the
  # vacuous green the fail-closed rules exist to remove.
  if ! git -C "$repo_root" fetch --quiet "$REMOTE_A" >&2; then
    _err "git fetch $REMOTE_A failed (url: $url_a) — refusing to report parity against a remote this host could not read."
    return 2
  fi
  if ! git -C "$repo_root" fetch --quiet "$REMOTE_B" >&2; then
    _err "git fetch $REMOTE_B failed (url: $url_b) — refusing to report parity against a remote this host could not read."
    return 2
  fi
  _info "fetched: $REMOTE_A, $REMOTE_B"

  ref_a="refs/remotes/$REMOTE_A/$BRANCH"
  ref_b="refs/remotes/$REMOTE_B/$BRANCH"
  tip_a="$(git -C "$repo_root" rev-parse --verify --quiet "$ref_a")" || tip_a=""
  if [ -z "$tip_a" ]; then
    _err "remote-tracking ref $REMOTE_A/$BRANCH is missing after fetching $REMOTE_A — the remote answered but has no '$BRANCH' branch (refusing to compare against nothing)."
    return 2
  fi
  tip_b="$(git -C "$repo_root" rev-parse --verify --quiet "$ref_b")" || tip_b=""
  if [ -z "$tip_b" ]; then
    _err "remote-tracking ref $REMOTE_B/$BRANCH is missing after fetching $REMOTE_B — the remote answered but has no '$BRANCH' branch (refusing to compare against nothing)."
    return 2
  fi

  # The verdict function reports the tips in its loud block, so they travel as
  # globals — set before the verdict is called.
  TIP_A_SHORT="$(git -C "$repo_root" rev-parse --short=12 "$tip_a" 2>/dev/null)" || TIP_A_SHORT="$tip_a"
  TIP_B_SHORT="$(git -C "$repo_root" rev-parse --short=12 "$tip_b" 2>/dev/null)" || TIP_B_SHORT="$tip_b"
  _info "tips: $REMOTE_A/$BRANCH=$TIP_A_SHORT $REMOTE_B/$BRANCH=$TIP_B_SHORT"

  counts="$(git -C "$repo_root" rev-list --left-right --count "$REMOTE_A/$BRANCH...$REMOTE_B/$BRANCH" 2>/dev/null)" || counts=""
  read -r left right <<<"$counts" || true
  case "${left:-}" in *[!0-9]* | '') left="" ;; esac
  case "${right:-}" in *[!0-9]* | '') right="" ;; esac
  if [ -z "$left" ] || [ -z "$right" ]; then
    _err "git rev-list --left-right --count $REMOTE_A/$BRANCH...$REMOTE_B/$BRANCH did not print two counts (got '${counts:-<empty>}') — refusing to report a parity verdict on counts this run could not compute."
    return 2
  fi
  _info "counts: left($REMOTE_A-only)=$left right($REMOTE_B-only)=$right"

  # NEUTER-MARK[parity-verdict]: the parity verdict call. The selftest seds
  # exactly this line (in a copy, and asserts the copy changed) to prove that a
  # rejected fixture is rejected BY THIS VERDICT and not by accident.
  if ! _parity_verdict "$left" "$right"; then
    printf 'FAIL  parity    %s/%s vs %s/%s (%s-only=%s %s-only=%s)\n' \
      "$REMOTE_A" "$BRANCH" "$REMOTE_B" "$BRANCH" "$REMOTE_A" "$left" "$REMOTE_B" "$right"
    _err "FAIL — $REMOTE_A/$BRANCH and $REMOTE_B/$BRANCH are NOT in parity ($left $right); see the diagnosis and fix above."
    return 1
  fi

  printf 'PASS  parity    %s/%s == %s/%s (%s %s)\n' \
    "$REMOTE_A" "$BRANCH" "$REMOTE_B" "$BRANCH" "$left" "$right"
  _info "PASS — $REMOTE_A/$BRANCH and $REMOTE_B/$BRANCH are in exact parity ($left $right) at $TIP_A_SHORT."
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

main() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -h | --help | help)
        usage
        return 0
        ;;
      -*)
        _err "unknown option '$1' (try --help)"
        return 2
        ;;
      *)
        _err "this checker takes no arguments (got '$1') — configure it with PARITY_REMOTE_A / PARITY_REMOTE_B / PARITY_BRANCH."
        return 2
        ;;
    esac
  done

  run_check
}

main "$@"
exit $?
