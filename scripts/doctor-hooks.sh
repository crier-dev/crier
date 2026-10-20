#!/usr/bin/env bash
# DF-CRIER-421: read-only proof that THIS repository's gate is armed.
# DF-CRIER-430: EVERY entrypoint's board arm is checked by MARKER, not only by bytes.
# DF-CRIER-433: … and the arm must EXECUTE — a marker that cannot be reached is refused.
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
#   (2) EXECUTION PROBE — the hook is RUN. A scratch Git repository is built under
#       ${TMPDIR:-/tmp}: `.coding-hermes/board/{tasks,events}.jsonl` seeded with
#       PROBE_ROWS rows each, the tracked `scripts/check-board.sh` copied in, a
#       committed baseline, and the INSTALLED hook copied to that repository's
#       `.git/hooks/<hook>`. Then, exactly as Git runs these three entrypoints
#       (cwd = repository root, no arguments, inherited environment):
#         * healthy run — nothing staged: the hook must exit 0;
#         * shrink run  — a 5→1 shrink of tasks.jsonl staged: the hook must exit
#           NON-ZERO and its stderr must name the shrink for that file.
#       A hook that accepts the shrink is refused as a board arm that cannot run,
#       whatever its text says. This is reachability end to end: the lines above
#       the marker, `set -e`/`set -u` interactions, quoting, early returns.
#
# WHAT THE PROBE DOES NOT COVER (stated, not implied)
# ---------------------------------------------------
#   * It runs the INSTALLED hook directly rather than through Git's own commit
#     machinery, so it proves the hook BODY reaches the board arm; that Git will
#     find that file at all is what the hooksPath/cmp checks above establish.
#   * It builds a STAGED-INDEX fixture for all three entrypoints — including
#     pre-applypatch, which does have a staged index when Git runs it (Git applies
#     the patch to the index before the hook). NOT simulated: a real merge in
#     progress (MERGE_HEAD) or a real `git am` patch. A hook that only misbehaves
#     in those states would not be caught, and a hook type that cannot be probed
#     this way is refused outright — never silently downgraded to presence-only.
#   * The fixture's `boardctl` is a STAND-IN on PATH (its `version` output is
#     printed in the probe evidence, so a fixture that did not use it is caught).
#     The probe measures whether the HOOK reaches its board arm; boardctl's own
#     validation is what the real gate runs, not what this proves.
#   * `BOARD_ALLOW_SHRINK` is REMOVED from the probe's environment. It is the
#     documented declaration for an intended deletion, and inheriting it would let
#     a shrink pass and turn this probe into a false refusal.
#   * Each hook invocation is bounded (`DOCTOR_PROBE_TIMEOUT`, default 20s) and the
#     bound is printed; a run that hits it is refused, never counted as a pass.
#
# LIMITS: the static arm reads LINES, not behaviour; the execution arm proves the
# board arm is reachable and refuses a real shrink in a fixture, not that a hook
# does nothing ELSE on the way, and not that a bypass (--no-verify, later writes,
# command-line config) is impossible.
set -uo pipefail

# The probe's per-invocation bound (seconds). A bound is ALWAYS in effect: it is
# reported in the probe evidence line, and a hook that exhausts it is refused.
PROBE_TIMEOUT="${DOCTOR_PROBE_TIMEOUT:-20}"
# Rows seeded per canonical board file; the shrink run stages exactly one row, so
# the fixture's refusal must report PROBE_ROWS-1 lost rows.
PROBE_ROWS=5
PROBE_DIR=""

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

# ── the execution probe (DF-CRIER-433) ────────────────────────────────────────
# probe_seed_board <fixture> — the board the shrink is measured against.
probe_seed_board() {
  local fx="$1" i
  : >"$fx/.coding-hermes/board/tasks.jsonl" || return 1
  : >"$fx/.coding-hermes/board/events.jsonl" || return 1
  i=1
  while [ "$i" -le "$PROBE_ROWS" ]; do
    printf '{"id":"PROBE-T-%s","title":"doctor-hooks execution probe row","status":"pending","priority":"P3"}\n' \
      "$i" >>"$fx/.coding-hermes/board/tasks.jsonl" || return 1
    printf '{"id":"PROBE-E-%s","type":"probe.fixture","detail":"doctor-hooks execution probe event"}\n' \
      "$i" >>"$fx/.coding-hermes/board/events.jsonl" || return 1
    i=$((i + 1))
  done
  return 0
}

# probe_build <root> <fixture> — seed, wire and COMMIT the fixture repository.
# Every Git call is pinned (`core.hooksPath=/dev/null` so no host hook can run,
# `commit.gpgsign=false`, explicit identity, no init template) so the fixture
# measures the installed hook and nothing else on the host.
probe_build() {
  local root="$1" fx="$2" git_id
  mkdir -p "$fx/.coding-hermes/board" "$fx/scripts" "$fx/bin" || return 1
  probe_seed_board "$fx" || return 1
  cp "$root/scripts/check-board.sh" "$fx/scripts/check-board.sh" || return 1
  # A stand-in validator keeps the fixture free of a real boardctl: what is under
  # test is the HOOK's reachability, and a stub makes the fixture deterministic on
  # a box where boardctl is absent (where the arm would otherwise fail for that
  # reason alone). check-board.sh prints the resolved validator path every run, so
  # the evidence line proves this stand-in — not some host binary — was used.
  cat >"$fx/bin/boardctl" <<'PROBE_BOARDCTL_STUB'
#!/bin/sh
# doctor-hooks (DF-CRIER-433) execution-probe stand-in. Board VALIDATION is not
# what the probe measures; this answers `version` and accepts, so the only
# rejection the fixture can produce is the board arm's own.
case "${1:-}" in
  version) printf 'boardctl probe-standin 0 (doctor-hooks execution probe fixture)\n' ;;
esac
exit 0
PROBE_BOARDCTL_STUB
  chmod +x "$fx/bin/boardctl" || return 1
  git_id=(-c user.name=doctor-hooks-probe -c user.email=doctor-hooks-probe@example.invalid
    -c core.hooksPath=/dev/null -c commit.gpgsign=false)
  git -c init.templateDir= -c init.defaultBranch=probe init -q "$fx" || return 1
  git -C "$fx" "${git_id[@]}" add .coding-hermes/board scripts bin || return 1
  git -C "$fx" "${git_id[@]}" commit -qm 'doctor-hooks execution probe fixture base' || return 1
  return 0
}

# probe_run <fixture> <hook-path> <stderr-file> -> hook stdout; exit status is the
# hook's. cwd is the fixture root, no arguments: how Git runs these entrypoints.
# GIT_DIR and friends are removed so the hook cannot be aimed at the doctor's own
# repository; BOARD_ALLOW_SHRINK is removed because honouring it would let the
# staged shrink through and turn this probe into a false refusal.
probe_run() {
  local fx="$1" hookpath="$2" errfile="$3"
  (
    cd "$fx" || exit 1
    if command -v timeout >/dev/null 2>&1; then
      env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_COMMON_DIR \
        -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES -u BOARD_ALLOW_SHRINK \
        PATH="$fx/bin:$PATH" timeout "$PROBE_TIMEOUT" "$hookpath"
    else
      env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_COMMON_DIR \
        -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES -u BOARD_ALLOW_SHRINK \
        PATH="$fx/bin:$PATH" "$hookpath"
    fi
  ) 2>"$errfile"
}

# probe_hook <root> <fixture> <hook> <installed-path> -> evidence line on stdout;
# refuses (stderr, non-zero) when the installed hook cannot be proven to run its
# board arm.
probe_hook() {
  local root="$1" fx="$2" hook="$3" dst="$4"
  local errfile="$PROBE_DIR/$hook.stderr" bound healthy_out healthy_rc
  local shrink_out shrink_rc first_line
  # The hook under test is the INSTALLED file, byte-for-byte: the fixture runs the
  # artifact this repository would actually execute.
  mkdir -p "$fx/.git/hooks" || { refuse "probe fixture could not create $fx/.git/hooks"; return 1; }
  cp "$dst" "$fx/.git/hooks/$hook" ||
    { refuse "probe could not stage the installed $hook into the fixture ($dst)"; return 1; }
  chmod +x "$fx/.git/hooks/$hook" ||
    { refuse "probe could not make the fixture's $hook executable"; return 1; }
  if ! cmp -s "$dst" "$fx/.git/hooks/$hook"; then
    refuse "probe fixture's $hook is not byte-identical to the installed hook ($dst) — the run would not measure what is installed"
    return 1
  fi
  if command -v timeout >/dev/null 2>&1; then
    bound="bound ${PROBE_TIMEOUT}s/hook"
  else
    bound="NO BOUND (timeout(1) is not on PATH)"
  fi
  # Reset the fixture to a CLEAN canonical state before the healthy run: a staged
  # or worktree change left by a previous hook's run would make this arm measure
  # something else, and the healthy run is only meaningful over an untouched board.
  probe_seed_board "$fx" || { refuse "probe fixture could not seed its board under $fx"; return 1; }
  git -C "$fx" reset -q || { refuse "probe fixture could not reset its index under $fx"; return 1; }
  if [ -n "$(git -C "$fx" status --porcelain)" ]; then
    refuse "probe fixture $fx is not clean before the healthy run — the fixture is not the shape this probe assumes"
    return 1
  fi
  healthy_out="$(probe_run "$fx" "$fx/.git/hooks/$hook" "$errfile")"
  healthy_rc=$?
  if [ "$healthy_rc" -eq 124 ]; then
    refuse "installed $hook did not answer within the probe bound ($bound) on a HEALTHY fixture — refusing rather than reading a timeout as a pass"
    return 1
  fi
  if [ "$healthy_rc" -ne 0 ]; then
    first_line="$(head -2 "$errfile" 2>/dev/null | tr '\n' ' ')"
    refuse "installed $hook REJECTED a healthy probe fixture (exit $healthy_rc) in $fx — the arm is not merely unreachable, it rejects a valid board [$bound] (stderr: ${first_line:-<none>})"
    return 1
  fi
  # The shrink: stage PROBE_ROWS-1 rows away. The board arm runs on EVERY commit,
  # so the hook owes a non-zero exit here whatever else is staged.
  probe_seed_board "$fx" || { refuse "probe fixture could not restore its board under $fx"; return 1; }
  head -n 1 "$fx/.coding-hermes/board/tasks.jsonl" >"$fx/.coding-hermes/board/tasks.jsonl.shrink" ||
    { refuse "probe fixture could not build the shrink under $fx"; return 1; }
  mv "$fx/.coding-hermes/board/tasks.jsonl.shrink" "$fx/.coding-hermes/board/tasks.jsonl" ||
    { refuse "probe fixture could not stage the shrink under $fx"; return 1; }
  git -C "$fx" add .coding-hermes/board/tasks.jsonl ||
    { refuse "probe fixture could not stage tasks.jsonl under $fx"; return 1; }
  shrink_out="$(probe_run "$fx" "$fx/.git/hooks/$hook" "$errfile")"
  shrink_rc=$?
  : "$shrink_out" # captured (not printed): the hook's own stdout must not leak into the doctor's
  # The reachability verdict comes FIRST: a hook that accepted the shrink never
  # reached its board arm, whatever else the fixture would say about itself.
  if [ "$shrink_rc" -eq 124 ]; then
    refuse "installed $hook did not answer within the probe bound ($bound) on a staged board shrink — refusing rather than reading a timeout as a pass"
    return 1
  fi
  if [ "$shrink_rc" -eq 0 ]; then
    refuse "installed $hook does not reach its board arm: a staged ${PROBE_ROWS}->1 shrink of .coding-hermes/board/tasks.jsonl was ACCEPTED (exit 0) in $fx — the marker is present, the arm never ran [$bound] ($dst)"
    return 1
  fi
  first_line="$(head -1 "$errfile" 2>/dev/null)"
  case "$first_line" in
    *.coding-hermes/board/tasks.jsonl*shrank:*)
      ;;
    *)
      refuse "installed $hook exited $shrink_rc on a staged board shrink WITHOUT naming the shrink — the arm is not doing its job [$bound] (stderr: ${first_line:-<none>})"
      return 1
      ;;
  esac
  # The fixture's own stand-in must be the validator that answered, or the fixture
  # measured something else (a host boardctl, a different PATH). This is checked
  # only once the hook is known to REACH its arm — a hook that never ran it has
  # nothing to attest to, and was already refused above for that reason.
  case "$healthy_out" in
    *"$fx/bin/boardctl"*) ;;
    *)
      refuse "probe fixture used a validator other than its own stand-in ($fx/bin/boardctl) — the fixture is not hermetic, so the run proves nothing"
      return 1
      ;;
  esac
  printf 'doctor-hooks: probe %s: EXECUTED — healthy fixture exit 0; staged %s->1 board shrink REFUSED exit %s: "%s"; fixture validator %s/bin/boardctl (stand-in); %s\n' \
    "$hook" "$PROBE_ROWS" "$shrink_rc" "$first_line" "$fx" "$bound"
  git -C "$fx" reset -q || { refuse "probe fixture could not unstage under $fx"; return 1; }
  return 0
}

# require_probe <root> <fixture> <hook> <installed-path>
# The EXECUTION arm's verdict. It is a separate function so the neuter control in
# scripts/doctor-hooks-selftest.py has exactly one site to force to success.
require_probe() { probe_hook "$1" "$2" "$3" "$4" || return 1; }

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
  local tool
  for tool in git python3 mktemp cp grep head; do
    command -v "$tool" >/dev/null 2>&1 ||
      { refuse "the execution probe needs $tool, which is not on PATH — the installed board arm cannot run on this host either, so presence-only is not reported as armed"; return 1; }
  done
  [ -f "$root/scripts/check-board.sh" ] ||
    { refuse "scripts/check-board.sh is missing ($root/scripts/check-board.sh) — every entrypoint's board arm execs it, so the installed hook cannot run; the probe will not fall back to presence-only"; return 1; }
  PROBE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/doctor-hooks-probe.XXXXXX")" ||
    { refuse 'the execution probe could not create a scratch directory under ${TMPDIR:-/tmp}'; return 1; }
  local fx="$PROBE_DIR/fixture"
  probe_build "$root" "$fx" ||
    { refuse "the execution probe could not build its fixture under $fx (requires git, python3 and a writable ${TMPDIR:-/tmp}); the hooks were NOT reported as armed on presence alone"; return 1; }

  # Phase order is load-bearing: the probe evidence is COLLECTED here and printed
  # only after every hook has passed, so a refusal can never follow a printed OK.
  local probe_lines=() evidence
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    dst="$hooks/$hook"
    evidence="$(require_probe "$root" "$fx" "$hook" "$dst")" || return 1
    probe_lines+=("$evidence")
  done

  # Print success only after ALL checks, never a partial green before a refusal.
  printf 'doctor-hooks: probe scope: all three entrypoints ran the installed hook in one staged-index fixture under %s (healthy board -> exit 0; staged %s->1 tasks.jsonl shrink -> non-zero naming the shrink); a real merge in progress or a real `git am` patch is NOT simulated, and the fixture boardctl is a stand-in. Each hook invocation is bounded (%ss, DOCTOR_PROBE_TIMEOUT to change); an unprobeable hook is REFUSED, never reported on presence alone.\n' \
    "$PROBE_DIR" "$PROBE_ROWS" "$PROBE_TIMEOUT"
  local i=0
  for hook in pre-commit pre-merge-commit pre-applypatch; do
    printf 'doctor-hooks: OK %s (executable, byte-identical, board-arm markers present, arm REACHES: a staged board shrink is refused; repository hooks at %s)\n' "$hook" "$hooks"
    printf '%s\n' "${probe_lines[i]}"
    i=$((i + 1))
  done
}

doctor_hooks "$@"
