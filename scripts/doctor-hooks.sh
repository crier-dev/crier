#!/usr/bin/env bash
# DF-CRIER-421: read-only proof that THIS repository's gate is armed.
# DF-CRIER-430: EVERY entrypoint's board arm is checked by MARKER, not only by bytes.
# DF-CRIER-433: … and the arm must EXECUTE — a marker that cannot be reached is refused.
# DF-CRIER-438: … and it must execute the way GIT RUNS IT — the probe drives a real
#               `git commit` / `git merge --no-ff` / `git am`, never the hook directly.
# DF-CRIER-439: `timeout(1)` is a REQUIRED tool — without it the doctor exits 2.
# DF-CRIER-441: the bound KILLS a hook that traps SIGTERM (`--kill-after`), and the
#               whole probe process group is reaped when the bound fires.
#
# Run from anywhere inside the checkout/worktree, not relative to this script.
# Exit 1 names the failing condition; exit 2 names a required tool that is missing
# from PATH; exit 0 prints exactly one line per hook.
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
# WHY PRESENCE IS STILL NOT ENOUGH — THE EXECUTION PROBE (DF-CRIER-433)
# ---------------------------------------------------------------------
# A marker can be present and UNREACHABLE. Coordinator-measured on the merged
# tree minutes after 430 landed: took scripts/hooks/pre-merge-commit, PREPENDED
# `exit 0 # DISARMED` keeping the original body and every marker line below it,
# reinstalled with `make install-hooks`, and `make doctor-hooks` printed three
# `OK … board-arm markers present` lines and exited 0 — while the gate could not
# run at all. A reviewer reproduced it on pre-commit with a staged 5→1 board
# shrink that COMMITTED, exit 0, the gate never running. A marker that is present
# but unreachable is exactly the stale generation this doctor claims to detect.
#
# So the doctor no longer trusts text alone. Two arms are added, in this order:
#
#   (1) STATIC PRECONDITION — refuse when an UNCONDITIONAL success exit
#       (`exit`, `exit 0`, `return`, `return 0`) appears in the hook BEFORE its
#       board arm marker line. Cheap, exact and noisy-good: it names the line, the
#       statement, and the marker line the arm would have run on. It is a heuristic
#       over shell text — it tracks block depth (if/for/while/until/case/select/
#       subshell/function bodies) so a CONDITIONAL exit is not flagged, and it can
#       only UNDER-flag (a guard such as `[ -n "$X" ] || exit 0` is not a statement
#       starting with the keyword). Under-flagging is safe because (2) is
#       authoritative and refuses whatever (1) misses.
#   (2) EXECUTION PROBE — the hook is RUN, BY GIT. A scratch Git repository is
#       built under ${TMPDIR:-/tmp}: `.coding-hermes/board/{tasks,events}.jsonl`
#       seeded with PROBE_ROWS rows each, the tracked `scripts/check-board.sh`
#       copied in, a committed baseline, the INSTALLED hook copied into the
#       fixture's hooks directory (which the fixture's LOCAL core.hooksPath
#       selects), and then the ONE Git entrypoint that invokes that hook is run —
#       `git commit` for pre-commit, `git merge --no-ff` for pre-merge-commit,
#       `git am` for pre-applypatch:
#         * healthy run — a non-board change (or a merge/`am` of one): the
#           entrypoint must exit 0 AND the board must still be intact in HEAD;
#         * shrink run  — the same entrypoint carrying a 5→1 shrink of
#           tasks.jsonl: it must exit NON-ZERO, its stderr must name the shrink,
#           and HEAD must still hold the un-shrunk board.
#       A hook that accepts the shrink is refused as a board arm that cannot run,
#       whatever its text says. This is reachability end to end: the lines above
#       the marker, `set -e`/`set -u` interactions, quoting, early returns.
#
# WHY THE PROBE DRIVES GIT ITSELF (DF-CRIER-438, measured on the merged 84c6caa)
# -----------------------------------------------------------------------------
# The probe used to run the INSTALLED hook as a DIRECT child of the doctor, under
# an environment the doctor synthesized. Every other way the probe presented
# itself was therefore a TELL a hook could gate on, and the false green was still
# reachable — coordinator-measured on a fresh clone of `main`, with the tracked
# hook disarmed by a fixture test and the marker left intact:
#
#   case "${PWD}" in
#     */doctor-hooks-probe*) : ;;   # the probe's STABLE scratch prefix
#     *) exit 0 ;;
#   esac
#
#   doctor-hooks: OK pre-commit (… arm REACHES: a staged board shrink is refused …)
#   real commit of tasks.jsonl 703 -> 1: COMMIT_RC=0, rows in HEAD: 1
#
# The tells were structural: the stable `doctor-hooks-probe.*` prefix of the
# scratch directory, the fixture's `bin/` on PATH, and a stand-in `boardctl` that
# could be stat'ed. Running the hook directly is what made them load-bearing — Git
# never invokes a hook that way, so a hook keyed on any of them is armed in the
# probe and disarmed in every real commit, which is a green that means nothing.
#
# The probe therefore no longer executes the hook at all. It runs the ENTRYPOINT
# Git runs, in the fixture, and lets GIT invoke the hook:
#
#   pre-commit        -> git commit            (the shrink run stages the 5→1 shrink)
#   pre-merge-commit  -> git merge --no-ff     (a real merge, MERGE_HEAD in progress,
#                                               whose result is the 5→1 shrink)
#   pre-applypatch    -> git am <patch>        (a real mailbox patch carrying it)
#
# Consequences, each measured on git 2.53.0 while this arm was written:
#   * the hook's environment, cwd, argv and stdin are GIT's, not the probe's — the
#     synthesized environment of DF-CRIER-437 is gone because it is no longer needed
#     (Git exports GIT_INDEX_FILE=.git/index and GIT_PREFIX= to pre-commit and
#     pre-merge-commit, nothing to pre-applypatch, and never GIT_DIR unless the
#     caller had one);
#   * a real merge in progress and a real `git am` patch are exercised — the states
#     this header used to list as NOT simulated;
#   * hook output arrives on the caller's STDERR (git runs these hooks with their
#     stdout redirected onto stderr — measured), so the refusal text the probe
#     asserts on is read where a user would see it;
#   * the fixture's directory name is RANDOM (`mktemp -d` on a random slug), so no
#     stable probe prefix exists to key on, and the stand-in tools directory lives
#     OUTSIDE the fixture root, so no path under the repository the hook runs in is
#     on PATH;
#   * nothing else in the fixture NAMES the probe either: its commit identity is
#     THIS repository's own identity, its branches are `main`/`side`/`patch`, its
#     commit messages, file names and board rows are ordinary text, and its stand-in
#     `boardctl` is named `boardctl` and answers `version` with the HOST's own
#     boardctl version line — the probe's own evidence line is what says it is a
#     stand-in. A hook gated on any probe-identifying string therefore arms in a
#     real commit and exits 0 in the fixture, which is a REFUSAL, not a green.
# There is NO direct-execution path left: a hook that Git cannot invoke in this
# harness is refused, never probed by hand and reported armed. An "arm REACHES"
# line is therefore always produced by a hook Git actually invoked.
#
# WHAT THE PROBE DOES NOT COVER (stated, not implied)
# ---------------------------------------------------
#   * It drives the THREE entrypoints this repository installs, and only those:
#     pre-push, post-commit, prepare-commit-msg, commit-msg and the rest are not
#     installed here and are not probed. `git cherry-pick`/`rebase` (which invoke
#     pre-commit) and a hand-run `git commit` completing a merge are not probed.
#   * It always runs from the repository ROOT, so GIT_PREFIX is always empty: a
#     hook keyed on a SUBDIRECTORY prefix is not exercised.
#   * It uses the fixture's own index. A hook keyed on GIT_INDEX_FILE pointing
#     somewhere else (a partial index, `GIT_INDEX_FILE=/tmp/x git commit`) is not
#     exercised.
#   * The fixture's `boardctl` is a STAND-IN on PATH (the probe evidence names the
#     path it resolved, so a fixture that did not use it is caught). Boardctl's own
#     validation is not what this probe measures: the stand-in accepts, so the only
#     rejection the fixture can produce is the board arm's own.
#   * The fixture IS a scratch repository under ${TMPDIR:-/tmp} whose validator is
#     that stand-in. A hook that inspects PATH's shape, its parent's command line,
#     or file metadata instead of the diff Git stages can still tell a probe from a
#     real commit; that residue is inherent to any fixture-based probe and is
#     stated here rather than claimed to be solved.
#   * `BOARD_ALLOW_SHRINK` is REMOVED from the probe's environment. It is the
#     documented declaration for an intended deletion, and inheriting it would let
#     a shrink pass and turn this probe into a false refusal.
#   * Each hook invocation is bounded (`DOCTOR_PROBE_TIMEOUT`, default 20s) and the
#     bound is printed; a run that hits it is refused, never counted as a pass
#     (DF-CRIER-441: `timeout --kill-after=1s`, and exit 124/137 both count as the
#     bound firing).
#   * The bound needs `timeout(1)`: it is a REQUIRED tool, and a host without it is
#     exit 2 naming the tool (DF-CRIER-439), never an unbounded run.
#
# LIMITS: the static arm reads LINES, not behaviour; the execution arm proves the
# board arm is reachable through a real entrypoint and refuses a real shrink in a
# fixture, not that a hook does nothing ELSE on the way, and not that a bypass
# (--no-verify, later writes, command-line config) is impossible.
set -uo pipefail

# The probe's per-invocation bound (seconds). A bound is ALWAYS in effect: it is
# reported in the probe evidence line, and a hook that exhausts it is refused
# (DF-CRIER-441: the bound signal is followed by `--kill-after`, so a hook that
# traps SIGTERM is killed rather than waited for, and its whole process group is
# reaped so a hostile hook cannot outlive the run).
PROBE_TIMEOUT="${DOCTOR_PROBE_TIMEOUT:-20}"
# Rows seeded per canonical board file; the shrink run stages exactly one row, so
# the fixture's refusal must report PROBE_ROWS-1 lost rows.
PROBE_ROWS=5
PROBE_DIR=""
# The stand-in tools directory the fixture's board arm resolves through PATH.
# DF-CRIER-438: it lives OUTSIDE every fixture root, so no path under the
# repository the hook runs in appears on PATH.
PROBE_TOOLS=""
# The fixture repository's identity, set in its LOCAL config rather than passed
# with `-c`: a `-c` override is visible to the hook as GIT_CONFIG_PARAMETERS, which
# a real commit of this repository does not carry. Nothing on the probe's own
# command line may be something a hook could key on.
PROBE_IDENT_NAME=board
PROBE_IDENT_EMAIL=board@example.invalid

# ── the probe's environment ───────────────────────────────────────────────────
# `git rev-parse --local-env-vars` is Git's own definition of the environment
# variables that point at a repository (measured set, git 2.53.0). Kept literal
# here rather than read from a subprocess, so the strip list is reviewable in the
# diff. Every one of them is REMOVED from the environment the probe's OWN Git
# operations run in — an inherited GIT_DIR/GIT_INDEX_FILE points at the caller's
# repository and would make the probe build, stage into and read the WRONG one.
# DF-CRIER-438: this list no longer describes the environment the HOOK sees. The
# hook sees whatever Git exports to it, because Git is what invokes it.
GIT_LOCAL_ENV_VARS=(
  GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_PREFIX GIT_IMPLICIT_WORK_TREE
  GIT_GRAFT_FILE GIT_SHALLOW_FILE GIT_REPLACE_REF_BASE GIT_NO_REPLACE_OBJECTS
  GIT_CONFIG GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS
)
PROBE_UNSET_ARGS=()
for _probe_var in "${GIT_LOCAL_ENV_VARS[@]}" BOARD_ALLOW_SHRINK; do
  PROBE_UNSET_ARGS+=(-u "$_probe_var")
done

cleanup_probe() {
  local rc=$?
  if [ -n "${PROBE_DIR:-}" ]; then
    rm -rf "$PROBE_DIR"
    PROBE_DIR=""
  fi
  return "$rc"
}
trap cleanup_probe EXIT

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

# first_marker_line <installed-path> <marker> -> line number, empty when absent
# Exact whole-line match, the same comparison require_marker makes: the arm's
# boundary must be the line the marker contract points at, not a comment that
# happens to quote it.
first_marker_line() {
  grep -nxF -- "$2" "$1" 2>/dev/null | head -1 | cut -d: -f1
}

# success_exit_before <installed-path> <marker-line> -> "<line>|<text>", empty if none
#
# DF-CRIER-433 static arm: an unconditional success exit above the board arm
# marker means the arm can never run. "Unconditional" is decided by BLOCK DEPTH,
# tracked with a deliberately conservative first-token rule: a statement whose
# first token is if/for/while/until/case/select, a `(`, a function definition or a
# lone `{` opens a block; fi/done/esac/`}`/`)` closes one. Items that open or close
# on one line (`f() { … }`, `} else {`) net to zero. The rule can only MISS (it is
# why `[ -n "$X" ] || exit 0` is invisible here); the execution probe backstops
# every miss, so a miss costs a worse error message, never a false green.
success_exit_before() {
  awk -v limit="$2" '
    BEGIN { depth = 0 }
    NR >= limit { exit }
    {
      line = $0
      sub(/\r$/, "", line)
      trimmed = line
      sub(/^[ \t]+/, "", trimmed)
      if (trimmed == "" || substr(trimmed, 1, 1) == "#") next
      # the success-exit test runs at the depth this line started at
      if (depth == 0 &&
          trimmed ~ /^(exit|return)[ \t]*0?[ \t]*;?[ \t]*(#.*)?$/) {
        print NR "|" trimmed
        exit
      }
      first = trimmed
      sub(/[ \t].*$/, "", first)
      opened = 0
      closed = 0
      if (first ~ /^(if|for|while|until|case|select|\()$/) opened = 1
      else if (trimmed ~ /^[A-Za-z_][A-Za-z0-9_]*[ \t]*\(\)/) opened = 1
      else if (trimmed ~ /\{[ \t]*$/) opened = 1
      if (first ~ /^(fi|done|esac|\}|\))$/) closed = 1
      else if (trimmed ~ /\}[ \t]*$/) closed = 1
      depth = depth + opened - closed
      if (depth < 0) depth = 0
    }' "$1"
}

# ── the execution probe (DF-CRIER-433, driven by Git since DF-CRIER-438) ──────

# probe_seed_board <fixture> — the board the shrink is measured against.
probe_seed_board() {
  local fx="$1" i
  : >"$fx/.coding-hermes/board/tasks.jsonl" || return 1
  : >"$fx/.coding-hermes/board/events.jsonl" || return 1
  i=1
  while [ "$i" -le "$PROBE_ROWS" ]; do
    printf '{"id":"T-%s","title":"board row %s","status":"pending","priority":"P3"}\n' \
      "$i" "$i" >>"$fx/.coding-hermes/board/tasks.jsonl" || return 1
    printf '{"id":%s,"event_type":"board_row","task_id":"T-%s","actor":"board","detail":"board row %s"}\n' \
      "$i" "$i" "$i" >>"$fx/.coding-hermes/board/events.jsonl" || return 1
    i=$((i + 1))
  done
  return 0
}

# probe_git <fixture> <git-args…> — a Git call the probe makes ABOUT its own
# fixture. The caller's repository-pointing environment is removed first: with an
# inherited GIT_DIR/GIT_INDEX_FILE these calls would reset, stage and read the
# CALLER's repository instead of the fixture. These calls are the fixture's
# SETUP; the entrypoint the hook observes is run by probe_run, through Git itself.
probe_git() {
  local fx="$1"
  shift
  env "${PROBE_UNSET_ARGS[@]}" git -C "$fx" "$@"
}

# probe_tools <dir> — the stand-in tools directory (DF-CRIER-438).
#
# The stand-in exists so the fixture is deterministic on a host where boardctl is
# absent (the arm would otherwise fail for that reason alone). It is deliberately
# indistinguishable from the real thing: it is named `boardctl`, and its `version`
# output is the HOST boardctl's own version line when one is installed. What it
# does NOT do is validate rows — board VALIDATION is not what this probe measures,
# so it accepts, and the only rejection the fixture can produce is the board arm's
# own. The `gitreins` stub is there for the same reason: an unconditional caller
# must not reach (or hang on) the host's co-harness.
probe_tools() {
  local tools="$1" version_line
  mkdir -p "$tools" || return 1
  version_line="$(timeout --kill-after=1s 5 boardctl version 2>/dev/null | head -1)"
  [ -n "$version_line" ] || version_line='boardctl version dev'
  version_line="$(printf '%s' "$version_line" | tr -d "'")"
  cat >"$tools/boardctl" <<PROBE_BOARDCTL
#!/bin/sh
# boardctl — resolved by the board arm through PATH.
case "\${1:-}" in
  version) printf '%s\\n' '$version_line' ;;
esac
exit 0
PROBE_BOARDCTL
  chmod +x "$tools/boardctl" || return 1
  printf '#!/bin/sh\nexit 0\n' >"$tools/gitreins" || return 1
  chmod +x "$tools/gitreins" || return 1
  return 0
}

# probe_build <root> <fx> — seed, wire and COMMIT one fixture repository.
#
# Everything the fixture's own history needs (identity, no gpg signing, no hooks)
# lives in the fixture's LOCAL config, never in `-c` on a command the hook can
# observe. `core.hooksPath` starts at /dev/null — so the fixture's setup commits
# cannot run a host hook the caller happens to have configured, and an inherited
# GLOBAL hooksPath can never be consulted — and probe_install_hook points it at the
# fixture's own hooks directory once the hook under test is in place.
probe_build() {
  local root="$1" fx="$2"
  mkdir -p "$fx/.coding-hermes/board" "$fx/scripts" || return 1
  probe_seed_board "$fx" || return 1
  cp "$root/scripts/check-board.sh" "$fx/scripts/check-board.sh" || return 1
  printf '%s\n' 'content' >"$fx/notes.txt" || return 1
  env "${PROBE_UNSET_ARGS[@]}" git -c init.templateDir= -c init.defaultBranch=main init -q "$fx" || return 1
  mkdir -p "$fx/.git/hooks" || return 1
  probe_git "$fx" config user.name "$PROBE_IDENT_NAME" >/dev/null || return 1
  probe_git "$fx" config user.email "$PROBE_IDENT_EMAIL" >/dev/null || return 1
  probe_git "$fx" config commit.gpgsign false || return 1
  probe_git "$fx" config core.hooksPath /dev/null || return 1
  probe_git "$fx" add .coding-hermes/board scripts notes.txt >/dev/null || return 1
  probe_git "$fx" commit -q -m 'board baseline' >/dev/null || return 1
  return 0
}

# probe_install_hook <fx> <hook> <installed-path> — the INSTALLED hook, byte for
# byte, in the fixture's own hooks directory, which the fixture's LOCAL
# core.hooksPath selects.
probe_install_hook() {
  local fx="$1" hook="$2" dst="$3"
  mkdir -p "$fx/.git/hooks" || { refuse "probe fixture could not create $fx/.git/hooks"; return 1; }
  cp "$dst" "$fx/.git/hooks/$hook" ||
    { refuse "probe could not stage the installed $hook into the fixture ($dst)"; return 1; }
  chmod +x "$fx/.git/hooks/$hook" ||
    { refuse "probe could not make the fixture's $hook executable"; return 1; }
  if ! cmp -s "$dst" "$fx/.git/hooks/$hook"; then
    refuse "probe fixture's $hook is not byte-identical to the installed hook ($dst) — the run would not measure what is installed"
    return 1
  fi
  probe_git "$fx" config core.hooksPath "$fx/.git/hooks" ||
    { refuse "probe fixture could not select its own hooks directory under $fx"; return 1; }
  return 0
}

# probe_shrink_stage <fx> — cut the fixture's tasks.jsonl to ONE row and stage it:
# the shrink the board arm owes a refusal for.
probe_shrink_stage() {
  local fx="$1"
  head -n 1 "$fx/.coding-hermes/board/tasks.jsonl" >"$fx/.coding-hermes/board/tasks.jsonl.shrink" || return 1
  mv "$fx/.coding-hermes/board/tasks.jsonl.shrink" "$fx/.coding-hermes/board/tasks.jsonl" || return 1
  probe_git "$fx" add .coding-hermes/board/tasks.jsonl >/dev/null || return 1
  return 0
}

# probe_entry_prepare <fx> <hook> <shrink> — stage the entrypoint's input.
#
# The commits here are SETUP: they construct the hostile input (the side branch,
# the divergence, the patch) and therefore run with hooks disabled. The one commit
# that must run the hook under test is the entrypoint itself, in probe_run.
probe_entry_prepare() {
  local fx="$1" hook="$2" shrink="$3" branch main_branch
  case "$hook" in
    pre-commit)
      if [ "$shrink" -eq 1 ]; then
        probe_shrink_stage "$fx" || return 1
      else
        printf '%s\n' 'changed' >>"$fx/notes.txt" || return 1
        probe_git "$fx" add notes.txt >/dev/null || return 1
      fi
      ;;
    pre-merge-commit)
      branch="$(probe_git "$fx" rev-parse --abbrev-ref HEAD)" || return 1
      probe_git "$fx" checkout -q -b side >/dev/null || return 1
      if [ "$shrink" -eq 1 ]; then
        probe_shrink_stage "$fx" || return 1
      else
        printf '%s\n' 'side' >"$fx/side.txt" || return 1
        probe_git "$fx" add side.txt >/dev/null || return 1
      fi
      probe_git "$fx" -c core.hooksPath=/dev/null commit -q -m 'side change' >/dev/null || return 1
      probe_git "$fx" checkout -q "$branch" >/dev/null || return 1
      printf '%s\n' 'diverged' >>"$fx/notes.txt" || return 1
      probe_git "$fx" add notes.txt >/dev/null || return 1
      probe_git "$fx" -c core.hooksPath=/dev/null commit -q -m 'unrelated change' >/dev/null || return 1
      ;;
    pre-applypatch)
      main_branch="$(probe_git "$fx" rev-parse --abbrev-ref HEAD)" || return 1
      branch="$(probe_git "$fx" rev-parse HEAD)" || return 1
      probe_git "$fx" checkout -q -b patch >/dev/null || return 1
      if [ "$shrink" -eq 1 ]; then
        probe_shrink_stage "$fx" || return 1
      else
        printf '%s\n' 'patched' >>"$fx/notes.txt" || return 1
        probe_git "$fx" add notes.txt >/dev/null || return 1
      fi
      probe_git "$fx" -c core.hooksPath=/dev/null commit -q -m 'patched change' >/dev/null || return 1
      probe_git "$fx" format-patch -1 --stdout >"$fx/patch.mbox" || return 1
      # Back to the branch the patch applies onto — a branch, as `git am` is really
      # run, never a detached HEAD a hook could key on.
      probe_git "$fx" checkout -q -B "$main_branch" "$branch" >/dev/null || return 1
      ;;
    *)
      refuse "no probe entrypoint is defined for $hook — refusing rather than falling back to running the hook directly"
      return 1
      ;;
  esac
  return 0
}

# probe_entry_command <fx> <hook> -> PROBE_CMD: the ONE Git command Git itself
# would run for this entrypoint.
PROBE_CMD=()
probe_entry_command() {
  case "$2" in
    pre-commit) PROBE_CMD=(git commit -q -m 'board update') ;;
    pre-merge-commit) PROBE_CMD=(git merge --no-ff --no-edit side) ;;
    pre-applypatch) PROBE_CMD=(git am "$1/patch.mbox") ;;
  esac
}

# probe_entry_label <hook> -> how the entrypoint is named in evidence.
probe_entry_label() {
  case "$1" in
    pre-commit) printf '%s' 'git commit' ;;
    pre-merge-commit) printf '%s' 'git merge --no-ff' ;;
    pre-applypatch) printf '%s' 'git am' ;;
  esac
}

# probe_first_line <file> -> its first line, empty when unreadable.
probe_first_line() { head -1 "$1" 2>/dev/null | tr -d '\r'; }

# probe_reason <file> -> the most specific line in a captured hook stderr: the
# first line that names a refusal, else the first line at all.
probe_reason() {
  local line
  line="$(grep -m1 -E 'refused|FAILED|UNAVAILABLE|missing|not in vocabulary|shrank|cannot' "$1" 2>/dev/null)"
  [ -n "$line" ] || line="$(probe_first_line "$1")"
  printf '%s' "$line"
}

# probe_run <fx> <out> <err> <command…> -> the command's exit status.
#
# One REAL Git entrypoint, run the way this repository's own commits run it: cwd =
# the fixture root, stdin /dev/null, the caller's repository-pointing environment
# removed (an inherited GIT_DIR/GIT_INDEX_FILE would aim the operation — and the
# hook Git invokes — at the CALLER's repository), the fixture's stand-in tools
# directory on PATH so the board arm resolves its validator. Both streams are
# captured to FILES: git runs these hooks with their stdout redirected onto stderr
# (measured), and a captured file — never a pipe — means a hook that leaks a child
# cannot hold the doctor open.
#
# DF-CRIER-441: the bound is `timeout --kill-after=1s`, and the command runs in its
# OWN process group (job control) so that when the bound fires the whole group —
# the Git process AND the hook it invoked, even one that traps SIGTERM and waits —
# is killed and reaped. Without the group kill a SIGTERM-trapping hook is merely
# abandoned by the Git process that dies, and outlives the doctor.
probe_run() {
  local fx="$1" out="$2" err="$3"
  shift 3
  local -a env_args=("${PROBE_UNSET_ARGS[@]}" "PATH=$PROBE_TOOLS:$PATH")
  (
    set -m
    cd "$fx" || exit 1
    env "${env_args[@]}" timeout --kill-after=1s "$PROBE_TIMEOUT" "$@" </dev/null &
    local child=$! rc=0
    wait "$child" || rc=$?
    # The bound fired (124) or the KILL did (137): nothing of the probe may
    # outlive the run. Best-effort, and harmless when the group is already gone.
    kill -KILL -- "-$child" 2>/dev/null || true
    if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
      wait "$child" 2>/dev/null || true
    fi
    exit "$rc"
  ) >"$out" 2>"$err"
}

# probe_hook <root> <hook> <installed-path> -> probe evidence line on stdout;
# refuses (stderr, non-zero) when the installed hook cannot be proven to run its
# board arm under the entrypoint Git itself runs.
probe_hook() {
  local root="$1" hook="$2" dst="$3"
  local entry bound fx out err rc rows line
  entry="$(probe_entry_label "$hook")"
  bound="bound ${PROBE_TIMEOUT}s/hook (timeout --kill-after=1s)"

  # ── the healthy run: the entrypoint must SUCCEED with the board intact ───────
  fx="$PROBE_DIR/$hook-healthy"
  probe_build "$root" "$fx" ||
    { refuse "the execution probe could not build its $hook fixture under $fx (requires git, python3 and a writable ${TMPDIR:-/tmp}); the hooks were NOT reported as armed on presence alone"; return 1; }
  probe_install_hook "$fx" "$hook" "$dst" || return 1
  probe_entry_prepare "$fx" "$hook" 0 ||
    { refuse "the execution probe could not stage its healthy $hook fixture under $fx"; return 1; }
  out="$PROBE_DIR/$hook.healthy.out"
  err="$PROBE_DIR/$hook.healthy.err"
  probe_entry_command "$fx" "$hook"
  rc=0
  probe_run "$fx" "$out" "$err" "${PROBE_CMD[@]}" || rc=$?
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    refuse "installed $hook did not answer within the probe bound ($bound) on a HEALTHY fixture (a real \`$entry\` in $fx) — refusing rather than reading a timeout as a pass"
    return 1
  fi
  if [ "$rc" -ne 0 ]; then
    refuse "installed $hook REJECTED a healthy probe fixture — a real \`$entry\` exited $rc in $fx, so the arm is not merely unreachable, it rejects a valid board [$bound] (stderr: $(probe_reason "$err"))"
    return 1
  fi
  rows="$(probe_git "$fx" show HEAD:.coding-hermes/board/tasks.jsonl 2>/dev/null | grep -c .)"
  if [ "$rows" != "$PROBE_ROWS" ]; then
    refuse "the healthy \`$entry\` probe fixture committed $rows row(s) of .coding-hermes/board/tasks.jsonl, expected $PROBE_ROWS — the fixture is not the shape this probe assumes ($fx)"
    return 1
  fi
  # What the healthy run answered with, kept for the hermeticity check below: it is
  # only meaningful once the hook is known to REACH its arm, because a hook that
  # exits before the arm never invokes a validator at all.
  local healthy_log
  healthy_log="$(cat "$out" "$err" 2>/dev/null)"

  # ── the shrink run: a real PROBE_ROWS->1 board shrink must be REFUSED ────────
  fx="$PROBE_DIR/$hook-shrink"
  probe_build "$root" "$fx" ||
    { refuse "the execution probe could not build its $hook shrink fixture under $fx"; return 1; }
  probe_install_hook "$fx" "$hook" "$dst" || return 1
  probe_entry_prepare "$fx" "$hook" 1 ||
    { refuse "the execution probe could not stage its $hook shrink fixture under $fx"; return 1; }
  out="$PROBE_DIR/$hook.shrink.out"
  err="$PROBE_DIR/$hook.shrink.err"
  probe_entry_command "$fx" "$hook"
  rc=0
  probe_run "$fx" "$out" "$err" "${PROBE_CMD[@]}" || rc=$?
  # The reachability verdict comes FIRST: a hook that accepted the shrink never
  # reached its board arm, whatever else the fixture would say about itself.
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    refuse "installed $hook did not answer within the probe bound ($bound) on a staged board shrink (a real \`$entry\` in $fx) — refusing rather than reading a timeout as a pass"
    return 1
  fi
  if [ "$rc" -eq 0 ]; then
    refuse "installed $hook does not reach its board arm: a real \`$entry\` of a staged ${PROBE_ROWS}->1 shrink of .coding-hermes/board/tasks.jsonl was ACCEPTED (exit 0) in $fx — the marker is present, the arm never ran [$bound]. The probe ran the entrypoint GIT itself runs, so a hook that is armed only outside one (a probe-only tell: a scratch path prefix, a fixture bin on PATH, a stand-in on PATH) is refused ($dst)"
    return 1
  fi
  line="$(grep -F -- '.coding-hermes/board/tasks.jsonl shrank:' "$err" 2>/dev/null | head -1)"
  if [ -z "$line" ]; then
    refuse "installed $hook exited $rc on a staged board shrink WITHOUT naming the shrink (a real \`$entry\`) — the arm is not doing its job [$bound] (stderr: $(probe_reason "$err"))"
    return 1
  fi
  # The refusal must have STOPPED the entrypoint: the committed board is still the
  # un-shrunk one. A hook that refuses and lets the commit through anyway would be
  # the same false green in a different shape.
  rows="$(probe_git "$fx" show HEAD:.coding-hermes/board/tasks.jsonl 2>/dev/null | grep -c .)"
  if [ "$rows" != "$PROBE_ROWS" ]; then
    refuse "installed $hook named the shrink but the \`$entry\` still landed it: .coding-hermes/board/tasks.jsonl holds $rows row(s) in HEAD of $fx, expected $PROBE_ROWS"
    return 1
  fi
  # Only now is the healthy run's answer worth reading: the hook is known to REACH
  # its arm, so a run that did not resolve the fixture's own validator measured
  # something else (a host boardctl, a different PATH) and proves nothing.
  case "$healthy_log" in
    *"$PROBE_TOOLS/boardctl"*) ;;
    *)
      refuse "probe fixture used a validator other than its own stand-in ($PROBE_TOOLS/boardctl) — the fixture is not hermetic, so the run proves nothing"
      return 1
      ;;
  esac
  printf 'doctor-hooks: probe %s: EXECUTED — a real `%s` in %s: healthy board committed exit 0 with %s row(s) intact; staged %s->1 board shrink REFUSED exit %s: "%s"; %s; fixture validator %s/boardctl (stand-in); %s\n' \
    "$hook" "$entry" "$fx" "$PROBE_ROWS" "$PROBE_ROWS" "$rc" "$line" \
    'the un-shrunk board is still what HEAD carries' "$PROBE_TOOLS" "$bound"
  return 0
}

# require_probe <root> <hook> <installed-path>
# The EXECUTION arm's verdict. It is a separate function so the neuter control in
# scripts/doctor-hooks-selftest.py has exactly one site to force to success.
require_probe() { probe_hook "$1" "$2" "$3" || return 1; }

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
    # The FIRST marker is the board arm invocation itself; everything the static
    # arm reads is measured against ITS line, not against cmp.
    local arm_marker arm_marker_label arm_line
    case "$hook" in
      pre-commit)
        arm_marker='bash "$BOARD_CHECKER" -q --staged-index'
        arm_marker_label="board arm marker"
        require_marker "$hook" "$dst" "board arm marker" "$arm_marker" || return 1
        ;;
      pre-merge-commit)
        arm_marker='exec bash "$checker" --staged-index'
        arm_marker_label="board arm marker"
        require_marker "$hook" "$dst" "board arm marker" "$arm_marker" || return 1
        require_marker "$hook" "$dst" "gate-scope disclosure line" "printf '%s\\n' 'gate scope: pre-merge-commit: this entrypoint validates the board only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and Makefile/Dockerfile checks are NOT RUN here).'" || return 1
        ;;
      pre-applypatch)
        arm_marker='exec bash "$checker" --staged-index'
        arm_marker_label="board arm marker"
        require_marker "$hook" "$dst" "board arm marker" "$arm_marker" || return 1
        require_marker "$hook" "$dst" "gate-scope disclosure line" "printf '%s\\n' 'gate scope: pre-applypatch: this entrypoint validates the board only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and Makefile/Dockerfile checks are NOT RUN here).'" || return 1
        ;;
    esac
    # DF-CRIER-433 static arm: DONE HERE means the first marker is above every
    # statement this hook actually wants to run — a success exit above it is a
    # one-line disarm no marker check can see.
    arm_line="$(first_marker_line "$dst" "$arm_marker")"
    local offence
    offence="$(success_exit_before "$dst" "$arm_line")"
    if [ -n "$offence" ]; then
      refuse "installed $hook exits before its board arm: line ${offence%%|*} is an unconditional \`${offence#*|}\`, above the $arm_marker_label on line $arm_line ($arm_marker) — the arm cannot run"
      return 1
    fi
    cmp -s "$src" "$dst" || { refuse "installed $hook differs from scripts/hooks/$hook ($dst)"; return 1; }
  done

  # ── the execution probe: presence is not reachability (DF-CRIER-433) ─────────
  # Prerequisites are stated rather than assumed: the board arm itself cannot run
  # without them, so a missing one is a refusal, never a silent presence-only pass.
  # DF-CRIER-439: `timeout` is checked FIRST and is a hard exit 2 — a probe run
  # without a bound is not an arm this doctor is willing to report on, and the bound
  # is not a nicety: it is what stops a hung hook from holding the commit back.
  local tool
  for tool in timeout git python3 mktemp cp grep head; do
    command -v "$tool" >/dev/null 2>&1 && continue
    if [ "$tool" = timeout ]; then
      printf 'doctor-hooks: ERROR: the execution probe needs %s, which is not on PATH — every probe run must be BOUNDED, and an unbounded run is never reported as armed; install coreutils and re-run; repair: make install-hooks\n' "$tool" >&2
      exit 2
    fi
    refuse "the execution probe needs $tool, which is not on PATH — the installed board arm cannot run on this host either, so presence-only is not reported as armed"
    return 1
  done
  [ -f "$root/scripts/check-board.sh" ] ||
    { refuse "scripts/check-board.sh is missing ($root/scripts/check-board.sh) — every entrypoint's board arm execs it, so the installed hook cannot run; the probe will not fall back to presence-only"; return 1; }

  # DF-CRIER-438: nothing in the fixture may name the probe. Its directory name is
  # random, its commit identity is THIS repository's own (an identity a real commit
  # here would carry — `doctor-hooks-probe` used to be exactly the kind of string a
  # hook could match), and its branches, commit messages, file names and board rows
  # carry no probe-identifying word either.
  PROBE_IDENT_NAME="$(git config user.name 2>/dev/null)"
  PROBE_IDENT_EMAIL="$(git config user.email 2>/dev/null)"
  [ -n "$PROBE_IDENT_NAME" ] || PROBE_IDENT_NAME=board
  [ -n "$PROBE_IDENT_EMAIL" ] || PROBE_IDENT_EMAIL=board@example.invalid

  # The probe it replaced used
  # a stable `doctor-hooks-probe.*` prefix, which a hook could match to tell the
  # probe from a real commit — and then arm only there. There is no stable name here
  # to match; the random slug comes from python3 (already a required tool), falling
  # back to the shell's own bits only if python3 refuses to answer.
  local probe_slug
  probe_slug="$(python3 -c 'import secrets; print(secrets.token_hex(8))' 2>/dev/null)"
  [ -n "$probe_slug" ] || probe_slug="$$$RANDOM$RANDOM"
  PROBE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/$probe_slug.XXXXXX")" ||
    { refuse 'the execution probe could not create a scratch directory under ${TMPDIR:-/tmp}'; return 1; }
  PROBE_TOOLS="$PROBE_DIR/.tools"
  probe_tools "$PROBE_TOOLS" ||
    { refuse "the execution probe could not write its stand-in tools under $PROBE_TOOLS"; return 1; }

  # Phase order is load-bearing: the probe evidence is COLLECTED here and printed
  # only after every hook has passed, so a refusal can never follow a printed OK.
  local probe_lines=() evidence
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    dst="$hooks/$hook"
    evidence="$(require_probe "$root" "$hook" "$dst")" || return 1
    probe_lines+=("$evidence")
  done

  # Print success only after ALL checks, never a partial green before a refusal.
  printf 'doctor-hooks: probe scope: every hook was probed by the entrypoint GIT ITSELF runs for it (pre-commit -> `git commit`, pre-merge-commit -> `git merge --no-ff`, pre-applypatch -> `git am`) in its own scratch repository under %s, whose LOCAL core.hooksPath is the fixture hooks directory: a healthy board must commit exit 0 with the board intact, and a staged %s->1 tasks.jsonl shrink must exit non-zero naming the shrink with the un-shrunk board still in HEAD. NO hook is executed directly. The fixture directory name is random, no path under the fixture root is on PATH, and its commit identity, branch names, commit messages and board rows name no probe; the validator answering the board arm is still a STAND-IN (%s/boardctl, whose `version` output mirrors the host boardctl) and the fixture IS a scratch repository, so a hook that inspects PATH, the command line of its parent or file metadata instead of the diff Git stages can still tell it from a real commit. Not simulated: a subdirectory invocation (GIT_PREFIX is always empty), --no-verify, a partial index, cherry-pick/rebase, and a hand-run `git commit` completing a merge. Each hook invocation is bounded (%ss, DOCTOR_PROBE_TIMEOUT to change) by `timeout --kill-after=1s`, whose whole process group is killed and reaped when the bound fires — exit 124 and 137 are both REFUSALS, never a pass. An unprobeable hook is REFUSED, never reported on presence alone.\n' \
    "$PROBE_DIR" "$PROBE_ROWS" "$PROBE_TOOLS" "$PROBE_TIMEOUT"
  local i=0
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    printf 'doctor-hooks: OK %s (executable, byte-identical, board-arm markers present, arm REACHES: a real `%s` of a staged board shrink is refused; repository hooks at %s)\n' \
      "$hook" "$(probe_entry_label "$hook")" "$hooks"
    printf '%s\n' "${probe_lines[i]}"
    i=$((i + 1))
  done
}

doctor_hooks "$@"
