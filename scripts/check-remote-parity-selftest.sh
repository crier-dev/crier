#!/usr/bin/env bash
#
# scripts/check-remote-parity-selftest.sh — prove the dual-remote parity checker
# is not vacuous (REV5-CRIER-001).
#
# WHY THIS EXISTS
# ---------------
# scripts/check-remote-parity.sh is the gate that keeps `origin` (github, PRIMARY)
# and `gitlab` (the subordinate content mirror) at the SAME main — a mirror that is
# behind answers clone/fetch with a stale tree, and the repo has already paid for
# both failure shapes: two diverged lineages around ticks 143-149, and a tick that
# simply never pushed the mirror (around ticks 79 and 101). A checker that can only
# say PASS would be decoration, so this selftest drives the REAL checker against
# fixtures it builds under ${TMPDIR:-/tmp} — throwaway BARE repositories wired into
# a scratch clone, never the real remotes — and requires a rejection for every
# drift shape, plus the DF-CRIER-206/209/189 NEUTER proof that the rejection comes
# from the checker's verdict and not from anything else.
#
# WHAT IS PROVED
# --------------
#   a) PARITY           both bare remotes at the same commit            -> ACCEPT (0),
#                       with the real `(0 0)` counts in the PASS line
#   b) MIRROR BEHIND    origin one commit ahead of gitlab (the case the
#                       tick battery kept hitting: left=1 right=0)      -> REJECT (1),
#                       naming both counts and the `git push gitlab main` recipe,
#                       and NOT naming the origin recipe
#   c) PRIMARY BEHIND   gitlab one commit ahead of origin
#                       (left=0 right=1)                               -> REJECT (1),
#                       naming `git push origin main` and not the gitlab one
#   d) DUAL LINEAGE     both remotes carry a commit the other lacks     -> REJECT (1),
#                       with the ESCALATE message, both tips, both counts, and
#                       NO push recipe of either kind
#   e) MISSING REMOTE   a remote name that is not configured            -> exit 2,
#                       naming the missing remote and the ones present
#   f) NEUTER PROOF     a copy of the checker whose verdict call is forced to
#                       success must ACCEPT fixture (b) — the same fixture the real
#                       checker rejects — and the copy must DIFFER from the original,
#                       so the causality proof cannot go vacuous
#   g) MISSING BRANCH   a configured remote that answers the fetch but has no
#                       `main` (an empty bare repo)                     -> exit 2,
#                       naming the missing remote-tracking ref
#   h) FAILED FETCH     a configured remote whose URL does not exist    -> exit 2,
#                       naming the remote and its URL (a remote this host cannot
#                       read is NEVER reported as "no drift")
#
# (g) and (h) are the two fail-closed rules the tick battery is most likely to hit
# in the wild (a mirror that was never initialised, a mirror host that is down);
# they are proved alongside (a)-(f) so a green here covers the whole header.
#
# ISOLATION CONTRACT
# ------------------
# Every fixture is a pair of bare repositories plus one scratch clone, all created
# under this selftest's own mktemp -d scratch dir and removed on exit. The checker is
# invoked with PARITY_REMOTE_A/PARITY_REMOTE_B/PARITY_BRANCH passed EXPLICITLY and
# with its working directory INSIDE the fixture, so it can never read — let alone
# fetch or push — the real origin/gitlab of the checkout this selftest lives in.
# No network is used: the remotes are local paths.
#
# EXIT CODES
#   0  every proof behaved as required
#   1  at least one proof did not behave
#   2  this selftest cannot run (the checker is missing on disk, or git/mktemp are
#      unavailable) — never a silent skip
#
# DEPENDENCIES: bash, git, mktemp, grep, cmp, sed, rm.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-remote-parity-selftest"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
CHECKER="$SCRIPT_DIR/check-remote-parity.sh"

FAILS=0
CHECKS=0
SELFTEST_TMP=""
LAST_OUT=""
LAST_RC=0

# Fixture commits need an identity and no signing; the host's global config is not
# assumed to have either.
GIT_ID=(-c user.name=parity-selftest -c user.email=parity-selftest@example.invalid -c commit.gpgsign=false)

_cleanup() { # EXIT trap for the selftest scratch dir (global var only)
  if [ -n "${SELFTEST_TMP:-}" ]; then
    rm -rf "$SELFTEST_TMP"
    SELFTEST_TMP=""
  fi
  return 0
}

_ok() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS: %s\n' "$*"
}

_bad() {
  CHECKS=$((CHECKS + 1))
  FAILS=$((FAILS + 1))
  printf '%s selftest: FAIL: %s\n' "$PROG" "$*" >&2
}

# ── fixture builders ──────────────────────────────────────────────────────────

# _commit <workdir> <message> — write file.txt and commit it.
_commit() {
  printf '%s\n' "$2" >"$1/file.txt" || return 1
  git -C "$1" add file.txt || return 1
  git -C "$1" "${GIT_ID[@]}" commit -q -m "$2" || return 1
  return 0
}

# make_fixture <base-dir> — origin.git + gitlab.git + work/, with work/main's single
# base commit pushed to BOTH remotes (the parity starting state).
make_fixture() {
  local b="$1"
  mkdir -p "$b" || return 1
  git init -q --bare "$b/origin.git" || return 1
  git init -q --bare "$b/gitlab.git" || return 1
  git init -q -b main "$b/work" || return 1
  _commit "$b/work" "base" || return 1
  git -C "$b/work" remote add origin "$b/origin.git" || return 1
  git -C "$b/work" remote add gitlab "$b/gitlab.git" || return 1
  git -C "$b/work" push -q origin main || return 1
  git -C "$b/work" push -q gitlab main || return 1
  return 0
}

# _run_checker <workdir> [ENV=VAL...] — runs the REAL checker inside <workdir> with
# an explicit remote/branch configuration; sets LAST_OUT (combined output) and
# LAST_RC (its exit status).
_run_checker() {
  local wd="$1"
  shift
  LAST_OUT="$(cd "$wd" || exit 2
    env "$@" bash "$CHECKER" 2>&1)"
  LAST_RC=$?
  return 0
}

# _has <needle> — does LAST_OUT contain <needle>?
_has() { printf '%s' "$LAST_OUT" | grep -q -- "$1"; }

# ── the proofs ────────────────────────────────────────────────────────────────

proofs() {
  local tmp="$SELFTEST_TMP"

  # ── a) PARITY is ACCEPTED, with the real counts ─────────────────────────────
  if ! make_fixture "$tmp/a"; then
    _bad "could not build the parity fixture (git init/commit/push failed under $tmp/a)"
  else
    _run_checker "$tmp/a/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=gitlab PARITY_BRANCH=main
    if [ "$LAST_RC" -ne 0 ]; then
      _bad "a) PARITY: two remotes at the same commit were REJECTED (rc=$LAST_RC)
  output: $LAST_OUT"
    elif ! _has 'PASS  parity    origin/main == gitlab/main (0 0)'; then
      _bad "a) PARITY: accepted, but the PASS line did not carry the real counts
  output: $LAST_OUT"
    else
      _ok "a) PARITY: two remotes at the same commit are accepted (rc=$LAST_RC) with the real (0 0) counts"
    fi
  fi

  # ── b) MIRROR BEHIND (left=1 right=0) -> REJECT with the gitlab recipe ────────
  if ! make_fixture "$tmp/b" ||
    ! _commit "$tmp/b/work" "origin-only" ||
    ! git -C "$tmp/b/work" push -q origin main; then
    _bad "could not build the mirror-behind fixture (origin one commit ahead of gitlab)"
  else
    _run_checker "$tmp/b/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=gitlab PARITY_BRANCH=main
    if [ "$LAST_RC" -eq 0 ]; then
      _bad "b) MIRROR BEHIND: origin one commit ahead of gitlab was ACCEPTED (rc=0)
  output: $LAST_OUT"
    elif ! _has 'git push gitlab main'; then
      _bad "b) MIRROR BEHIND: rejected (rc=$LAST_RC) without the 'git push gitlab main' recipe
  output: $LAST_OUT"
    elif _has 'git push origin main'; then
      _bad "b) MIRROR BEHIND: rejected while ALSO advising a push to origin (only the behind side may be advised)
  output: $LAST_OUT"
    elif ! _has 'origin-only=1 gitlab-only=0'; then
      _bad "b) MIRROR BEHIND: rejected without naming both counts (left=1 right=0)
  output: $LAST_OUT"
    elif ! _has 'FAIL  parity'; then
      _bad "b) MIRROR BEHIND: rejected without a FAIL line
  output: $LAST_OUT"
    else
      _ok "b) MIRROR BEHIND: origin ahead of gitlab is rejected (rc=$LAST_RC) with both counts named and the 'git push gitlab main' recipe"
    fi
  fi

  # ── c) PRIMARY BEHIND (left=0 right=1) -> REJECT with the origin recipe ───────
  if ! make_fixture "$tmp/c" ||
    ! _commit "$tmp/c/work" "gitlab-only" ||
    ! git -C "$tmp/c/work" push -q gitlab main; then
    _bad "could not build the primary-behind fixture (gitlab one commit ahead of origin)"
  else
    _run_checker "$tmp/c/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=gitlab PARITY_BRANCH=main
    if [ "$LAST_RC" -eq 0 ]; then
      _bad "c) PRIMARY BEHIND: gitlab one commit ahead of origin was ACCEPTED (rc=0)
  output: $LAST_OUT"
    elif ! _has 'git push origin main'; then
      _bad "c) PRIMARY BEHIND: rejected (rc=$LAST_RC) without the 'git push origin main' recipe
  output: $LAST_OUT"
    elif _has 'git push gitlab main'; then
      _bad "c) PRIMARY BEHIND: rejected while ALSO advising a push to gitlab (only the behind side may be advised)
  output: $LAST_OUT"
    elif ! _has 'origin-only=0 gitlab-only=1'; then
      _bad "c) PRIMARY BEHIND: rejected without naming both counts (left=0 right=1)
  output: $LAST_OUT"
    else
      _ok "c) PRIMARY BEHIND: gitlab ahead of origin is rejected (rc=$LAST_RC) with both counts named and the 'git push origin main' recipe"
    fi
  fi

  # ── d) DUAL LINEAGE -> REJECT with the escalation and NO push advice ──────────
  if ! make_fixture "$tmp/d" ||
    ! _commit "$tmp/d/work" "origin-side" ||
    ! git -C "$tmp/d/work" push -q origin main ||
    ! git -C "$tmp/d/work" reset -q --hard HEAD~1 ||
    ! _commit "$tmp/d/work" "gitlab-side" ||
    ! git -C "$tmp/d/work" push -q gitlab main; then
    _bad "could not build the dual-lineage fixture (both remotes carrying a unique commit)"
  else
    _run_checker "$tmp/d/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=gitlab PARITY_BRANCH=main
    if [ "$LAST_RC" -eq 0 ]; then
      _bad "d) DUAL LINEAGE: two diverged lineages were ACCEPTED (rc=0)
  output: $LAST_OUT"
    elif ! _has 'DUAL LINEAGE'; then
      _bad "d) DUAL LINEAGE: rejected (rc=$LAST_RC) without naming the dual-lineage shape
  output: $LAST_OUT"
    elif ! _has 'ESCALATE'; then
      _bad "d) DUAL LINEAGE: rejected without the escalation message
  output: $LAST_OUT"
    elif ! _has 'origin-only=1 gitlab-only=1'; then
      _bad "d) DUAL LINEAGE: rejected without naming both counts
  output: $LAST_OUT"
    elif _has 'git push origin main' || _has 'git push gitlab main'; then
      _bad "d) DUAL LINEAGE: a push recipe was printed — dual lineage must escalate and advise NO automatic push
  output: $LAST_OUT"
    else
      _ok "d) DUAL LINEAGE: two diverged lineages are rejected (rc=$LAST_RC) with the ESCALATE message and NO push recipe"
    fi
  fi

  # ── e) MISSING REMOTE -> exit 2 naming it, and no green ──────────────────────
  if ! make_fixture "$tmp/e"; then
    _bad "could not build the fixture for the missing-remote proof"
  else
    _run_checker "$tmp/e/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=no-such-remote PARITY_BRANCH=main
    if [ "$LAST_RC" -ne 2 ]; then
      _bad "e) MISSING REMOTE: exited $LAST_RC, not 2 (fail-closed)
  output: $LAST_OUT"
    elif ! _has "remote B 'no-such-remote' does not exist"; then
      _bad "e) MISSING REMOTE: exit 2 without naming the missing remote
  output: $LAST_OUT"
    elif _has 'PASS  parity'; then
      _bad "e) MISSING REMOTE: exit 2 but a PASS line was printed anyway
  output: $LAST_OUT"
    else
      _ok "e) MISSING REMOTE: an unconfigured remote exits 2 naming it and prints no green"
    fi
  fi

  # ── f) NEUTER PROOF: the verdict call forced to success must ACCEPT (b) ───────
  local neutered="$tmp/neutered-parity-verdict.sh"
  if [ ! -d "$tmp/b/work" ]; then
    _bad "f) NEUTER PROOF: the mirror-behind fixture (b) is missing, so the causality proof cannot run"
  elif ! cp "$CHECKER" "$neutered"; then
    _bad "f) NEUTER PROOF: could not copy the checker to $neutered"
  else
    sed -i.tmp -e 's|^\([[:space:]]*\)if ! _parity_verdict "\$left" "\$right"; then$|\1if false; then|' "$neutered"
    rm -f "$neutered.tmp"
    if cmp -s "$CHECKER" "$neutered"; then
      _bad "f) NEUTER PROOF: the neuter sed no longer matches the parity verdict line (NEUTER-MARK[parity-verdict]) — the causality proof would be vacuous"
    else
      # The SAME fixture (b) the real checker rejects, driven through the neutered
      # copy of the same script.
      LAST_OUT="$(cd "$tmp/b/work" || exit 2
        env PARITY_REMOTE_A=origin PARITY_REMOTE_B=gitlab PARITY_BRANCH=main bash "$neutered" 2>&1)"
      LAST_RC=$?
      if [ "$LAST_RC" -ne 0 ]; then
        _bad "f) NEUTER PROOF: with the verdict neutered the same mirror-behind fixture is STILL rejected (rc=$LAST_RC) — the rejection does not come from that verdict
  output: $LAST_OUT"
      else
        _ok "f) NEUTER PROOF: the neutered copy differs from the original and ACCEPTS the same mirror-behind fixture (rc=0) — the rejection is caused by the parity verdict"
      fi
    fi
  fi

  # ── g) a remote that answers but has no branch -> exit 2 ─────────────────────
  if ! make_fixture "$tmp/g" ||
    ! git init -q --bare "$tmp/g/empty.git" ||
    ! git -C "$tmp/g/work" remote add empty "$tmp/g/empty.git"; then
    _bad "could not build the missing-branch fixture (an empty bare remote)"
  else
    _run_checker "$tmp/g/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=empty PARITY_BRANCH=main
    if [ "$LAST_RC" -ne 2 ]; then
      _bad "g) MISSING BRANCH: exited $LAST_RC, not 2 (fail-closed)
  output: $LAST_OUT"
    elif ! _has 'remote-tracking ref empty/main is missing'; then
      _bad "g) MISSING BRANCH: exit 2 without naming the missing remote-tracking ref
  output: $LAST_OUT"
    elif _has 'PASS  parity'; then
      _bad "g) MISSING BRANCH: exit 2 but a PASS line was printed anyway
  output: $LAST_OUT"
    else
      _ok "g) MISSING BRANCH: a configured remote with no 'main' exits 2 naming the missing ref and prints no green"
    fi
  fi

  # ── h) a configured remote whose URL does not exist -> exit 2 ────────────────
  if ! make_fixture "$tmp/h" ||
    ! git -C "$tmp/h/work" remote add ghost "$tmp/h/ghost-missing.git"; then
    _bad "could not build the failed-fetch fixture (a remote pointing at a nonexistent path)"
  else
    _run_checker "$tmp/h/work" PARITY_REMOTE_A=origin PARITY_REMOTE_B=ghost PARITY_BRANCH=main
    if [ "$LAST_RC" -ne 2 ]; then
      _bad "h) FAILED FETCH: exited $LAST_RC, not 2 (fail-closed)
  output: $LAST_OUT"
    elif ! _has 'git fetch ghost failed'; then
      _bad "h) FAILED FETCH: exit 2 without naming the remote whose fetch failed
  output: $LAST_OUT"
    elif _has 'PASS  parity'; then
      _bad "h) FAILED FETCH: exit 2 but a PASS line was printed anyway
  output: $LAST_OUT"
    else
      _ok "h) FAILED FETCH: an unreachable remote exits 2 naming it and prints no green"
    fi
  fi

  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-remote-parity-selftest.sh — prove the dual-remote parity checker is not
vacuous (REV5-CRIER-001)

Usage:
  bash scripts/check-remote-parity-selftest.sh

Builds throwaway bare repositories under ${TMPDIR:-/tmp} and drives the real
checker against them: parity (accept), mirror behind, primary behind, dual
lineage (escalate, no push advice), a missing remote, a remote with no branch, an
unreachable remote, and a NEUTER proof that the rejection comes from the checker's
verdict. Never touches the real origin/gitlab.

Exit codes: 0 every proof behaved, 1 at least one did not, 2 this selftest cannot
run (checker missing on disk, or git/mktemp unavailable).
EOF
}

main() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -h | --help | help)
        usage
        return 0
        ;;
      *)
        printf "%s: ERROR: unknown argument '%s' (try --help)\n" "$PROG" "$1" >&2
        return 2
        ;;
    esac
  done

  command -v git >/dev/null 2>&1 || {
    printf '%s: ERROR: git is not on PATH — this selftest cannot build its fixtures.\n' "$PROG" >&2
    return 2
  }
  command -v mktemp >/dev/null 2>&1 || {
    printf '%s: ERROR: mktemp is not on PATH — this selftest cannot create its scratch dir.\n' "$PROG" >&2
    return 2
  }
  if [ ! -f "$CHECKER" ]; then
    printf '%s: ERROR: the checker under test is missing: %s\n' "$PROG" "$CHECKER" >&2
    printf '%s: ERROR: run this selftest from the checkout that holds scripts/check-remote-parity.sh.\n' "$PROG" >&2
    return 2
  fi

  # The scratch dir is held in a GLOBAL so the EXIT trap can read it after this
  # function returns — a trap quoting a function-local under `set -u` dies with
  # "unbound variable" and leaks the directory.
  SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-remote-parity-selftest.XXXXXX")" || {
    printf '%s: ERROR: mktemp -d failed\n' "$PROG" >&2
    return 2
  }
  trap '_cleanup' EXIT

  printf '%s: fixtures under %s (bare repos only — the real origin/gitlab are never read)\n' "$PROG" "$SELFTEST_TMP"
  proofs

  if [ "$FAILS" -ne 0 ]; then
    printf '%s selftest: %d/%d proofs behaved — FAIL\n' "$PROG" "$((CHECKS - FAILS))" "$CHECKS" >&2
    return 1
  fi
  printf '%s selftest: %d/%d proofs behaved\n' "$PROG" "$CHECKS" "$CHECKS"
  return 0
}

main "$@"
exit $?
