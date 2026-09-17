#!/usr/bin/env bash
#
# scripts/check-make-docker.sh — Makefile + Dockerfile gate (DF-CRIER-209).
#
# WHY THIS EXISTS
# ---------------
# The commit gate is the gitreins pre-commit hook and its Tier-1 battery is
# secrets / go_build / go_lint / go_tests. NONE of those checks reads a
# Makefile recipe or a Dockerfile, and the shell/YAML arm (DF-CRIER-206) only
# reads shell scripts and .github/workflows/*.yml. So a diff made ONLY of a
# Makefile and/or a Dockerfile still landed on a green that verified nothing
# about it — since DF-CRIER-206 that green is *labelled*
# (`gate scope: nothing guardable staged — …`) and AGENTS.md lists the hole,
# but labelled is not covered. This checker is the second arm: it takes an
# explicit file list (default: every tracked file in the repo) and fails closed
# on the Makefiles and Dockerfiles inside it.
#
# USAGE
# -----
#   bash scripts/check-make-docker.sh                  # default: all tracked files
#   bash scripts/check-make-docker.sh FILE [FILE...]   # exactly the files you name
#   bash scripts/check-make-docker.sh --engine auto|hadolint|builtin FILE...
#   bash scripts/check-make-docker.sh --selftest
#
# WHAT IS CHECKED — AND WHAT IS NOT
# ---------------------------------
#   makefile    a DRY-PARSE per file: `make -n --no-print-directory -f <file>
#               <target>` over a target list derived from the file itself (see
#               below). A nonzero make exit — `missing separator` (a recipe line
#               indented with spaces), an unterminated `define`, a bad
#               `include`, an unresolvable prerequisite — is a REJECT, and
#               make's own message (which names file:line) is surfaced.
#               This catches Makefile STRUCTURE: recipe indentation, block
#               balance, include resolution, target semantics. It does NOT
#               check the shell INSIDE a recipe: `make -n` prints recipe lines,
#               it does not run them, and no reliable recipe-text extraction is
#               implemented here (a recipe line can be any shell text plus make
#               expansions, so a `bash -n` over an extracted slice would both
#               miss expansions and invent syntax errors). A shell bug inside a
#               recipe is out of scope for this arm — and honest: a recipe that
#               invokes a tracked script IS covered, because that script is
#               checked by the shell/YAML arm on its own.
#               NOTE: `make` expands `$(shell …)` at PARSE time, even under
#               `-n`. Parsing a Makefile therefore runs its `$(shell …)` calls,
#               exactly as GNU make does. Only tracked, reviewed files are
#               parsed here; nothing downloads or executes a recipe body.
#   dockerfile  `hadolint --failure-threshold error <file>` when hadolint is on
#               PATH (its version is printed), otherwise a built-in python3
#               (stdlib-only) structural parse. The engine that ran is printed.
#               SEVERITY POLICY (measured: CI run 35288203740, 2026-09-17): a
#               hadolint run WITHOUT `--failure-threshold` exits NONZERO on
#               warning-level findings (DL3018 `apk add` pinning, DL3013 pip,
#               DL3016 npm), which rejected this repo's own Dockerfile,
#               Dockerfile.mcp and 6 example images — a clean tree red. This arm
#               therefore fails on ERROR-level findings only and PRINTS
#               warning/info/style findings as `advisory (not fatal):` lines, so
#               the lint signal survives without breaking the build.
#               The built-in parse
#               checks: (a) the first token of a logical line is a known
#               Dockerfile instruction; (b) FROM has an image reference; (c)
#               `AS <alias>` is present-and-valid, and aliases are unique; (d)
#               a line continuation that dangles at EOF; (e) `COPY/ADD
#               --from=<ref>` that is empty or FORWARD-references a stage alias
#               defined later in the file (a real docker error) — an external
#               image ref (`nginx:alpine`, `ghcr.io/…`) is NOT rejected.
#               It does NOT check Dockerfile semantics beyond that: no base
#               image existence, no layer/download behaviour, no
#               hadolint-style lint rules, no `# escape=` directive handling.
#
# FILE CLASSIFICATION (basename only)
# -----------------------------------
#   makefile    basename is exactly `Makefile`, `makefile`, `GNUmakefile`,
#               or matches `*.mk` / `Makefile.*`
#   dockerfile  basename is `Dockerfile`, matches `Dockerfile.*`, or matches
#               `*.dockerfile` — case-insensitive on the `Dockerfile` spelling,
#               so `dockerfile` and `DOCKERFILE.mcp` classify too
#
# MAKE TARGET DERIVATION (documented, deterministic)
# --------------------------------------------------
#   1. the `.PHONY` list (first `.PHONY:` line), minus tokens containing `$` or
#      `%`, deduplicated — targets the file itself declares as its interface;
#   2. otherwise the first non-special target: the first column-0 line whose
#      name matches `[A-Za-z0-9_][A-Za-z0-9_.-]*` followed by `:`, excluding
#      special (`.x`) targets and variable assignments (`:=`, `=` before the
#      colon);
#   3. otherwise NO target argument is passed, so the dry-parse uses GNU make's
#      own DEFAULT GOAL — the first target in the file, which is what a bare
#      `make` in that directory resolves to. (No invented target name such as
#      `all` is ever passed: a repository without an `all` target would fail
#      with `No rule to make target 'all'`, which would be a false positive.)
#   Each derived target is dry-parsed in its own make invocation, stopping at
#   the first failure, so a rejection names the target it died on.
#
# EXPLICIT FILE LISTS FAIL CLOSED (DF-CRIER-208 discipline)
# --------------------------------------------------------
# The default (no FILE arguments) mode is scope-tolerant: a tracked file that is
# neither a Makefile nor a Dockerfile is counted as out of scope and the run can
# still pass on the files that ARE in scope.
#
# An explicit file list is different: the caller ASSERTED that every named path
# is something to check. A list in which NOTHING classifies as a makefile or a
# dockerfile verified nothing, so it is rejected (exit 1, every path named)
# instead of ending in a green — `PASS — 0 file(s) checked` is exactly the blank
# green DF-CRIER-206 exists to remove. A MIXED list carrying at least one
# classified file keeps the historic note-and-continue behaviour for its
# unclassifiable entries, which is the shape the tracked pre-commit wrapper
# produces (it passes only files its own copy of these predicates classified).
#
# ENGINES (--engine, or CHECK_MAKE_DOCKER_ENGINE)
#   auto      (default) hadolint if present, else the built-in python3 parse,
#             else exit 2
#   hadolint  require hadolint (exit 2 when absent)
#   builtin   force the built-in python3 parse (what CI uses when it cannot get
#             hadolint, and what --selftest exercises both arms with)
# The `make` binary is resolved whenever a makefile is in scope; a missing
# validator is a hard error (exit 2), never a silent skip.
#
# EXIT CODES
#   0  every file in scope passed
#   1  at least one file was rejected — which also covers a NAMED file that is
#      missing/unreadable, and an explicit file list in which nothing classifies
#   2  misuse (bad option / bad --engine) or a missing dependency (no make; no
#      hadolint and no usable python3)
#
# A tracked file that is ABSENT from the worktree is named in a note and left
# out of the counts (no content means nothing to verify) — but a file NAMED on
# the command line that does not exist is a hard error.
#
# OUTPUT CONTRACT (grep-able)
#   `PASS  makefile      <file>  [dry-parse]` / `PASS  dockerfile    <file>` per
#   checked file, `FAIL  …` plus the tool's own message on rejection, then
#      scope: N makefile(s), M dockerfile(s) verified (make dry-parse engine:
#             GNU Make 4.4.1; dockerfile engine: builtin-parse (python3 3.11.15))
#   `scope: N makefile(s), M dockerfile(s) in scope — K rejected`
#   The counts are real counts of files actually checked in this run.
#
# SELFTEST — used by `make make-docker-selftest`; it builds its fixtures in a
# scratch dir under ${TMPDIR:-/tmp} and asserts, by running THIS script on them:
#   1. a CLEAN Makefile + CLEAN Dockerfile + clean Dockerfile.mcp-shaped file are
#      ACCEPTED, with a real `scope:` line and no false positive
#   2. a recipe indented with SPACES is REJECTED (`missing separator`), rc≠0, and
#      the file is NAMED in the output
#   3. an unknown/misspelled instruction is REJECTED and named
#   4. a line continuation dangling at EOF is REJECTED and named
#   5. `FROM` with no image reference is REJECTED and named
#   6. an invalid `AS` alias is REJECTED and named
#   7. a duplicate stage alias is REJECTED and named
#   8. `COPY --from=` naming a stage defined LATER is REJECTED and named, while
#      an external image ref (`nginx:alpine`, `ghcr.io/…`) is NOT (control)
#   9. an explicit list in which NOTHING classifies is REJECTED (exit 1), the
#      path is named, and no PASS line is printed at all
#  10. a MIXED explicit list (a clean Makefile + an unclassifiable plain file) is
#      still ACCEPTED — the shape the pre-commit wrapper produces
#  11. the NEUTER PROOF (makefile arm): a copy of this script with that arm's
#      verdict call seds to a success return ACCEPTS the same broken Makefile
#      (rc=0) — the rejection in check 2 is caused by THAT arm, not by accident
#  12. the NEUTER PROOF (dockerfile arm, built-in engine) likewise for check 3
#  13. with a PATH that hides `make`, a makefile run exits 2 naming the missing
#      tool instead of skipping the arm
#  14. with a PATH that hides `hadolint` AND `python3`, a dockerfile run exits 2
#      naming the missing tool
#  15. a `hadolint` that is on PATH but CANNOT RUN (bad interpreter) does not turn
#      every Dockerfile into a rejection: auto falls back to the built-in parse
#      (rc 0), while an explicit --engine hadolint exits 2 naming the tool
#  16. the hadolint SEVERITY POLICY: a WARNING-level finding is reported as
#      `advisory (not fatal)` and does NOT fail the run (hadolint's DEFAULT
#      threshold fails on warnings — that reddened CI run 35288203740 against
#      this repo's own Dockerfiles), while an ERROR-level finding still rejects.
#      Proven with a hadolint shim that honours --failure-threshold, so the check
#      runs on a host with no hadolint installed.
#   `make make-docker-selftest` exits 0 only when all of them behaved.
#
# DEPENDENCIES: bash, git (only for the default file list), grep/sed/mktemp;
# plus `make` when a makefile is in scope and one Dockerfile validator
# (hadolint or python3) when a dockerfile is in scope.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-make-docker"
ENGINE_REQUESTED="${CHECK_MAKE_DOCKER_ENGINE:-auto}"
ENGINE_RESOLVED=""
HADOLINT_BIN=""
MAKE_BIN=""
MAKE_DESC=""
DOCKER_ENGINE_DESC=""

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

# ── file classification ───────────────────────────────────────────────────────

_is_makefile() {
  local base
  base="$(basename -- "$1")"
  case "$base" in
    Makefile | makefile | GNUmakefile) return 0 ;;
    *.mk) return 0 ;;
    Makefile.*) return 0 ;;
  esac
  return 1
}

_is_dockerfile() {
  local base lower
  base="$(basename -- "$1")"
  case "$base" in
    Dockerfile | Dockerfile.* | *.dockerfile) return 0 ;;
  esac
  lower="$(printf '%s' "$base" | tr '[:upper:]' '[:lower:]')"
  case "$lower" in
    dockerfile | dockerfile.* | *.dockerfile) return 0 ;;
  esac
  return 1
}

# ── the two arms ──────────────────────────────────────────────────────────────

# CHECK_DIAG carries the rejected file's tool output up to the reporter, so the
# report reads `FAIL <file>` first and the tool's own message underneath it.
CHECK_DIAG=""

# _make_targets <file> — the derived dry-parse target list, one per line.
# Derivation order is documented in the header: .PHONY, else the first
# non-special target, else nothing (meaning: let make use its default goal).
_make_targets() {
  local f="$1" line name rest t
  local -a out=()
  local phony=""
  phony="$(sed -n 's/^\.PHONY[[:space:]]*:[[:space:]]*//p' "$f" | head -n 1)"
  if [ -n "$phony" ]; then
    for t in $phony; do
      case "$t" in
        *'$'* | *%* | *'='* | .*) continue ;;
      esac
      local seen=0
      for name in ${out[@]+"${out[@]}"}; do
        [ "$name" = "$t" ] && seen=1
      done
      [ "$seen" -eq 0 ] && out+=("$t")
    done
  else
    while IFS= read -r line; do
      case "$line" in
        '' | '#'* | "$(printf '\t')"* | ' '*) continue ;;
      esac
      case "$line" in
        *:*) ;;
        *) continue ;;
      esac
      name="${line%%:*}"
      rest="${line#*:}"
      name="${name%"${name##*[![:space:]]}"}"
      [ -n "$name" ] || continue
      case "$name" in
        .*) continue ;;
        *'='*) continue ;;
      esac
      case "$rest" in '='*) continue ;; esac
      case "$name" in
        include | -include | sinclude | define | endef | ifeq | ifneq | ifdef | ifndef | else | endif | export | unexport | override | vpath) continue ;;
      esac
      printf '%s' "$name" | grep -qE '^[A-Za-z0-9_][A-Za-z0-9_.-]*$' || continue
      out=("$name")
      break
    done <"$f"
  fi
  for t in ${out[@]+"${out[@]}"}; do
    printf '%s\n' "$t"
  done
  return 0
}

# _check_makefile <file> — dry-parse; returns nonzero when make rejects the file.
_check_makefile() {
  local f="$1" out="" rc=0 t=""
  local -a targets=()
  while IFS= read -r t; do
    [ -n "$t" ] && targets+=("$t")
  done < <(_make_targets "$f")

  if [ "${#targets[@]}" -eq 0 ]; then
    out="$("$MAKE_BIN" -n --no-print-directory -f "$f" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      CHECK_DIAG="$out"
      return 1
    fi
    _info "make dry-parse $(basename -- "$f"): no target derivable — parsed GNU make's default goal"
    return 0
  fi

  for t in "${targets[@]}"; do
    out="$("$MAKE_BIN" -n --no-print-directory -f "$f" "$t" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      CHECK_DIAG="$out
(make dry-parse failed on target '$t')"
      return 1
    fi
  done
  _info "make dry-parse $(basename -- "$f"): ${#targets[@]} target(s) from $(if sed -n 's/^\.PHONY[[:space:]]*:[[:space:]]*//p' "$f" | head -n 1 | grep -q .; then echo '.PHONY'; else echo 'the first non-special target'; fi)"
  return 0
}

# _check_dockerfile <file> — hadolint, or the built-in python3 structural parse.
_check_dockerfile() {
  local f="$1" out="" rc=0
  case "$ENGINE_RESOLVED" in
    hadolint)
      # SEVERITY POLICY (measured on CI run 35288203740, 2026-09-17): hadolint
      # WITHOUT --failure-threshold exits NONZERO on warning-level findings
      # (DL3018 `apk add` pinning, DL3013 pip, DL3016 npm), which rejected this
      # repo's own Dockerfile, Dockerfile.mcp and 6 example images — a clean tree
      # turned red and every later Makefile/Dockerfile commit was blocked. The
      # gate fails on ERROR-level findings only; warning/info/style findings are
      # still reported (as `advisory (not fatal):` lines) so the signal survives
      # without breaking the build. hadolint's exit code stays the verdict.
      out="$("$HADOLINT_BIN" --failure-threshold error "$f" 2>&1)"
      rc=$?
      if [ "$rc" -eq 0 ] && [ -n "$out" ]; then
        printf '%s\n' "$out" | sed 's/^/      advisory (not fatal): /'
      fi
      ;;
    builtin)
      # stdlib-only structural parse; diagnostics are `path:line: message`.
      out="$(python3 - "$f" <<'PY' 2>&1
import re
import sys

KNOWN = {
    "ADD", "ARG", "CMD", "COPY", "ENTRYPOINT", "ENV", "EXPOSE", "FROM",
    "HEALTHCHECK", "LABEL", "MAINTAINER", "ONBUILD", "RUN", "SHELL",
    "STOPSIGNAL", "USER", "VOLUME", "WORKDIR",
}
ALIAS_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]*$")
HEREDOC_RE = re.compile(r"<<-?\s*['\"]?([A-Za-z_][A-Za-z0-9_]*)['\"]?")


class ParseError(Exception):
    pass


def logical_lines(lines):
    """[(lineno, joined_text)] for instruction lines.

    Comment-only and blank lines are dropped, as docker does. A trailing
    backslash joins the next raw line with the backslash removed; a
    continuation that runs off the end of the file is an error. A heredoc body
    (`RUN <<EOF`) is payload, not instructions, and is skipped.
    """
    out = []
    i = 0
    n = len(lines)
    while i < n:
        raw = lines[i]
        if raw.strip() == "" or raw.lstrip().startswith("#"):
            i += 1
            continue
        start = i + 1
        text = raw
        i += 1
        while text.rstrip().endswith("\\"):
            if i >= n:
                raise ParseError(start, "line continuation '\\' dangling at end of file")
            text = text.rstrip()[:-1] + " " + lines[i]
            i += 1
        out.append((start, text))
        for marker in HEREDOC_RE.findall(text):
            while i < n:
                body = lines[i]
                i += 1
                if body.strip() == marker:
                    break
    return out


def check(path):
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        raw = fh.read()
    lines = raw.split("\n")
    if lines and lines[-1] == "":
        lines.pop()

    try:
        lls = logical_lines(lines)
    except ParseError as exc:
        sys.stderr.write("%s:%d: %s\n" % (path, exc.args[0], exc.args[1]))
        return 1

    errors = []

    # pass A — every stage alias in the file, so a FORWARD reference is visible
    alias_line = {}
    for lineno, text in lls:
        toks = text.split()
        if not toks or toks[0].upper() != "FROM":
            continue
        pos = [t for t in toks[1:] if not t.startswith("--")]
        if not pos or pos[0].upper() == "AS":
            continue
        idx = None
        for k, a in enumerate(pos):
            if k >= 1 and a.upper() == "AS":
                idx = k
                break
        if idx is None or idx + 1 >= len(pos):
            continue
        alias = pos[idx + 1]
        if not ALIAS_RE.match(alias):
            errors.append((lineno, "invalid stage alias '%s'" % alias))
        elif alias in alias_line:
            errors.append((lineno, "duplicate stage alias '%s' (first defined at line %d)"
                           % (alias, alias_line[alias])))
        else:
            alias_line[alias] = lineno

    # pass B — instruction shape
    for lineno, text in lls:
        toks = text.split()
        if not toks:
            continue
        instr = toks[0].upper()
        if instr not in KNOWN:
            errors.append((lineno, "unknown instruction '%s' (not a Dockerfile instruction)" % toks[0]))
            continue
        args = toks[1:]
        if instr == "FROM":
            pos = [t for t in args if not t.startswith("--")]
            if not pos:
                errors.append((lineno, "FROM with no image reference"))
            elif pos[0].upper() == "AS":
                errors.append((lineno, "FROM with no image reference ('AS' follows FROM)"))
            else:
                idx = None
                for k, a in enumerate(pos):
                    if k >= 1 and a.upper() == "AS":
                        idx = k
                        break
                if idx is not None and idx + 1 >= len(pos):
                    errors.append((lineno, "'AS' with no stage alias"))
        elif instr in ("COPY", "ADD"):
            for a in args:
                if not a.startswith("--from="):
                    continue
                ref = a[len("--from="):]
                if ref == "":
                    errors.append((lineno, "'--from=' names neither a stage nor an image"))
                elif ref in alias_line and alias_line[ref] > lineno:
                    errors.append((lineno, "COPY --from=%s forward-references stage '%s' defined later at line %d"
                                   % (ref, ref, alias_line[ref])))

    errors.sort(key=lambda e: e[0])
    for lineno, msg in errors:
        sys.stderr.write("%s:%d: %s\n" % (path, lineno, msg))
    return 1 if errors else 0


sys.exit(check(sys.argv[1]))
PY
)"
      rc=$?
      ;;
    *)
      _err "internal: no dockerfile engine resolved"
      return 2
      ;;
  esac
  [ "$rc" -eq 0 ] && return 0
  CHECK_DIAG="$out"
  return 1
}

# ── engine resolution + version attribution ───────────────────────────────────

_have_make() { command -v make >/dev/null 2>&1; }
_have_hadolint() { command -v hadolint >/dev/null 2>&1; }
# Presence is not usability: a truncated download or a foreign-arch binary on
# PATH (`command -v` finds it, the exec fails) would turn every Dockerfile into
# a rejection — a false red, not a green. Probe the way `_have_pyyaml` in
# scripts/check-shell-yaml.sh probes python3: make the tool answer.
_have_hadolint_usable() { _have_hadolint && hadolint --version >/dev/null 2>&1; }
_have_builtin() { command -v python3 >/dev/null 2>&1 && python3 -c 'import sys' >/dev/null 2>&1; }

resolve_make_engine() { # sets MAKE_BIN / MAKE_DESC
  if ! _have_make; then
    _err "no Makefile parser available: 'make' is not on PATH."
    _err "  refusing to skip the makefile check — install make (or GNU make) and re-run."
    return 2
  fi
  MAKE_BIN="$(command -v make)"
  local ver
  ver="$("$MAKE_BIN" --version 2>/dev/null | head -n 1)"
  [ -n "$ver" ] || ver="make (unknown version)"
  MAKE_DESC="$ver"
  return 0
}

resolve_docker_engine() { # <mode> -> sets ENGINE_RESOLVED / HADOLINT_BIN / DOCKER_ENGINE_DESC
  local mode="$1" ver="" why=""
  case "$mode" in
    auto)
      if _have_hadolint_usable; then
        mode=hadolint
      elif _have_builtin; then
        mode=builtin
      else
        if ! _have_hadolint; then
          why="hadolint is not on PATH"
        else
          why="hadolint is on PATH but cannot run (its --version probe failed)"
        fi
        _err "no Dockerfile validator available: $why and python3 is unusable."
        _err "  refusing to skip the dockerfile check — install hadolint (or python3) and re-run."
        return 2
      fi
      ;;
    hadolint)
      if ! _have_hadolint; then
        _err "--engine hadolint was requested but hadolint is not on PATH."
        return 2
      fi
      if ! _have_hadolint_usable; then
        _err "--engine hadolint was requested but hadolint is on PATH and cannot run (its --version probe failed)."
        return 2
      fi
      ;;
    builtin)
      if ! _have_builtin; then
        _err "--engine builtin was requested but python3 cannot be executed."
        return 2
      fi
      ;;
    *)
      _err "unknown engine '$mode' (expected auto|hadolint|builtin)"
      return 2
      ;;
  esac

  ENGINE_RESOLVED="$mode"
  case "$mode" in
    hadolint)
      HADOLINT_BIN="$(command -v hadolint)"
      ver="$("$HADOLINT_BIN" --version 2>/dev/null | head -n 1)"
      [ -n "$ver" ] || ver="unknown version"
      DOCKER_ENGINE_DESC="hadolint $ver (failure-threshold error; warnings advisory)"
      ;;
    builtin)
      ver="$(python3 -c 'import platform; print(platform.python_version())' 2>/dev/null)"
      [ -n "$ver" ] || ver="unknown version"
      DOCKER_ENGINE_DESC="builtin-parse (python3 $ver)"
      ;;
  esac
  return 0
}

# ── main check ────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-make-docker.sh — Makefile + Dockerfile gate (DF-CRIER-209)

Usage:
  bash scripts/check-make-docker.sh [--engine auto|hadolint|builtin] [FILE...]
  bash scripts/check-make-docker.sh --selftest

With no FILE arguments every tracked file is considered (git ls-files); only the
Makefiles and Dockerfiles inside that set are checked, and the real counts and
the engines that ran are printed:

  scope: 1 makefile(s), 2 dockerfile(s) verified (make dry-parse engine: GNU Make 4.4.1; dockerfile engine: builtin-parse (python3 3.11.15))

A makefile is dry-parsed with `make -n -f <file> <target>` over a target list
derived from the file (.PHONY, else the first non-special target, else make's own
default goal). A dockerfile is checked with hadolint when it is on PATH,
otherwise with a built-in python3 structural parse. A missing validator is exit
2, never a silent skip. hadolint runs with --failure-threshold error: an
error-level finding rejects the file, while warning/info/style findings are
printed as advisory lines and do not fail the run.

An explicit FILE list fails closed: a list in which nothing classifies as a
makefile or a dockerfile is rejected (exit 1) with the paths named — a list that
verified nothing never prints a PASS.

Exit codes: 0 all passed, 1 at least one rejected, 2 misuse/missing dependency.
EOF
}

run_check() { # <files...>
  local -a files=("$@")
  local -a make_files=() docker_files=() other_files=()
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
  #     the scope counts, and does not fail the run,
  #   - an UNREADABLE file is a failure in both modes.
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
    if _is_makefile "$f"; then
      make_files+=("$f")
    elif _is_dockerfile "$f"; then
      docker_files+=("$f")
    else
      other_files+=("$f")
    fi
  done

  if [ "$from_default" -eq 1 ]; then
    _info "scope source: git ls-files ($n_tracked tracked file(s)) from $repo_root"
    if [ "${#missing_tracked_list[@]}" -gt 0 ]; then
      local shown=0
      printf '%s: note: %d tracked file(s) are absent from the worktree and were not checked (no content = nothing to verify): %s' \
        "$PROG" "${#missing_tracked_list[@]}" "$repo_root/" >&2
      for f in "${missing_tracked_list[@]}"; do
        shown=$((shown + 1))
        [ "$shown" -le 10 ] && printf '%s ' "${f#"$repo_root/"}" >&2
      done
      [ "${#missing_tracked_list[@]}" -gt 10 ] && printf '(+%d more)' "$((${#missing_tracked_list[@]} - 10))" >&2
      printf '\n' >&2
    fi
  fi

  # ── fail-closed rule for an EXPLICIT file list (DF-CRIER-208 discipline) ─────
  # The default mode names nothing, so a tracked file that is neither a makefile
  # nor a dockerfile is simply out of scope (unchanged). An explicit list is a
  # caller ASSERTION that every named path is something to check; when NOTHING
  # in it classified, this run verified nothing and the paths are named instead
  # of a green. A MIXED list carrying at least one classified file keeps the
  # historic note-and-continue treatment for its unclassifiable entries — the
  # shape the tracked pre-commit wrapper produces.
  local policy_rejected=0
  if [ "$from_default" -eq 0 ]; then
    local n_classified=$((${#make_files[@]} + ${#docker_files[@]}))
    if [ "$n_classified" -eq 0 ] && [ "${#other_files[@]}" -gt 0 ]; then
      _err "explicit file list: none of the ${#files[@]} named file(s) is a Makefile or a Dockerfile — refusing to report an empty green over a list it verified nothing in:"
      for f in "${other_files[@]}"; do
        _err "  not a makefile, not a dockerfile: $f"
      done
      _err "  fix: name the Makefile/Dockerfile you meant, or drop the path from the list."
      policy_rejected="${#other_files[@]}"
    fi
  fi

  # Resolve + announce the engines BEFORE checking, so the evidence is
  # attributable even if a later check aborts. A missing validator is exit 2.
  if [ "${#make_files[@]}" -gt 0 ]; then
    resolve_make_engine || return 2
    _info "makefile arm: dry-parse engine = $MAKE_DESC"
  fi
  MAKE_DESC_DISPLAY="${MAKE_DESC:-not resolved (no makefile in scope)}"

  if [ "${#docker_files[@]}" -gt 0 ]; then
    resolve_docker_engine "$ENGINE_REQUESTED" || return 2
    _info "dockerfile arm: engine = $DOCKER_ENGINE_DESC"
  elif [ "$ENGINE_REQUESTED" = "hadolint" ] || [ "$ENGINE_REQUESTED" = "builtin" ]; then
    # Explicitly requested: honour the request even with nothing to check, so a
    # missing tool is still an error rather than a quiet no-op.
    resolve_docker_engine "$ENGINE_REQUESTED" || return 2
    _info "dockerfile arm: engine = $DOCKER_ENGINE_DESC (no dockerfile in scope)"
  fi
  DOCKER_DESC_DISPLAY="${DOCKER_ENGINE_DESC:-not resolved (no dockerfile in scope)}"

  for f in "${make_files[@]}"; do
    checked=$((checked + 1))
    # NEUTER-MARK[makefile-arm-verdict]: the makefile arm's verdict call. The
    # selftest seds exactly this line (and asserts the copy changed) to prove a
    # rejected fixture is rejected BY THIS ARM and not by accident.
    if ! _check_makefile "$f"; then
      printf 'FAIL  makefile    %s\n' "$f"
      [ -n "$CHECK_DIAG" ] && printf '%s\n' "$CHECK_DIAG" | sed 's/^/      /'
      _err "makefile dry-parse failed: $f"
      fails=$((fails + 1))
    else
      printf 'PASS  makefile    %s  [dry-parse]\n' "$f"
    fi
  done

  for f in "${docker_files[@]}"; do
    checked=$((checked + 1))
    # NEUTER-MARK[dockerfile-arm-verdict]: the dockerfile arm's verdict call
    # (hadolint or the built-in parse, whichever ENGINE_RESOLVED says). The
    # selftest seds exactly this line to prove causality for the built-in engine.
    if ! _check_dockerfile "$f"; then
      printf 'FAIL  dockerfile  %s  [%s]\n' "$f" "${ENGINE_RESOLVED:-none}"
      [ -n "$CHECK_DIAG" ] && printf '%s\n' "$CHECK_DIAG" | sed 's/^/      /'
      _err "dockerfile check failed: $f"
      fails=$((fails + 1))
    else
      printf 'PASS  dockerfile  %s  [%s]\n' "$f" "${ENGINE_RESOLVED:-none}"
    fi
  done

  # Anything handed to the checker that is neither: say so out loud (a silent
  # skip is the defect class this script exists to close).
  if [ "${#other_files[@]}" -gt 0 ]; then
    if [ "$from_default" -eq 1 ]; then
      _info "note: $((n_tracked - checked - ${#missing_tracked_list[@]})) tracked file(s) are neither Makefiles nor Dockerfiles and are out of scope"
    elif [ "$policy_rejected" -eq 0 ]; then
      local shown=0
      printf 'note: %d supplied file(s) are neither Makefiles nor Dockerfiles (not checked):' "${#other_files[@]}" >&2
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

  if [ "$fails" -eq 0 ] && [ "$missing" -eq 0 ] && [ "$policy_rejected" -eq 0 ]; then
    _info "scope: ${#make_files[@]} makefile(s), ${#docker_files[@]} dockerfile(s) verified (make dry-parse engine: $MAKE_DESC_DISPLAY; dockerfile engine: $DOCKER_DESC_DISPLAY)"
    _info "PASS — $checked file(s) checked, 0 rejected"
    return 0
  fi

  _info "scope: ${#make_files[@]} makefile(s), ${#docker_files[@]} dockerfile(s) in scope — $((fails + missing + policy_rejected)) rejected"
  _err "FAIL — $checked file(s) checked, $((fails + missing + policy_rejected)) rejected"
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
  _SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-make-docker-selftest.XXXXXX")" || {
    _err "selftest: mktemp -d failed"
    return 1
  }
  local tmp="$_SELFTEST_TMP"
  trap '_selftest_cleanup' EXIT

  # ── fixtures ────────────────────────────────────────────────────────────────
  # clean: a Makefile with a .PHONY interface and a tab-indented recipe, a
  # Dockerfile with a build stage referenced by a LATER stage, and a
  # Dockerfile.mcp-shaped twin.
  cat >"$tmp/clean.mk" <<'MK'
.PHONY: all build
all: build
	@echo all
build:
	@echo build
MK
  cat >"$tmp/clean.Dockerfile" <<'DF'
# ---- Build Stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN echo building \
    && echo done
# ---- Run Stage ----
FROM alpine:3.21
COPY --from=build /bin/app /usr/local/bin/app
ENTRYPOINT ["app"]
DF
  cat >"$tmp/Dockerfile.mcp" <<'DF'
FROM golang:1.26-alpine AS build
RUN echo a \
    && echo b
FROM alpine:3.21
COPY --from=build /bin/app /usr/local/bin/app
ENTRYPOINT ["app-mcp"]
DF

  # broken Makefile: a recipe line indented with SPACES -> `missing separator`
  cat >"$tmp/broken.mk" <<'MK'
.PHONY: all
all:
    @echo hi
MK

  # malformed Dockerfiles — two or more distinct breakages each
  printf 'FROM alpine:3.21\nRUM echo hi\n' >"$tmp/broken-unknown.Dockerfile"
  printf 'FROM alpine:3.21\nRUN echo hi \\\n' >"$tmp/broken-dangling.Dockerfile"
  printf 'FROM\nRUN echo hi\n' >"$tmp/broken-from.Dockerfile"
  printf 'FROM alpine:3.21 AS build:1\nRUN echo hi\n' >"$tmp/broken-alias.Dockerfile"
  printf 'FROM alpine AS build\nRUN echo hi\nFROM golang AS build\n' >"$tmp/broken-dup.Dockerfile"
  printf 'FROM alpine AS runtime\nCOPY --from=build /x /y\nFROM golang AS build\nRUN echo build\n' >"$tmp/broken-fwd.Dockerfile"
  # control: external image refs are NOT forward references
  printf 'FROM alpine AS runtime\nCOPY --from=nginx:alpine /etc/nginx /etc/nginx\nCOPY --from=ghcr.io/acme/app:v1 /a /b\n' >"$tmp/external-from.Dockerfile"

  # a plain file that classifies as neither (the wrapper's mixed-list shape)
  printf 'a plain text file: neither a Makefile nor a Dockerfile\n' >"$tmp/notes.md"

  local out="" rc=0 out2="" rc2=""

  # 1. clean Makefile + clean Dockerfile + clean Dockerfile.mcp-shaped file are
  #    ACCEPTED with a real scope line and no false positive
  checks=$((checks + 1))
  out="$(bash "$SELF" --engine builtin "$tmp/clean.mk" "$tmp/clean.Dockerfile" "$tmp/Dockerfile.mcp" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the clean makefile+dockerfile trio was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q 'FAIL'; then
    printf '%s selftest: FAIL: the clean trio was accepted but a FAIL line was printed (false positive)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'scope: 1 makefile(s), 2 dockerfile(s) verified'; then
    printf '%s selftest: FAIL: the clean run did not report real counts (scope line)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'dockerfile engine: builtin-parse'; then
    printf '%s selftest: FAIL: the clean run did not name the dockerfile engine it used\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a clean Makefile + Dockerfile + Dockerfile.mcp-shaped file are all accepted, with a scope line naming the counts and engines\n'
  fi

  # 2. a spaces-indented recipe is REJECTED, rc≠0, and the file is named
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/broken.mk" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a Makefile with a spaces-indented recipe was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'FAIL  makefile'; then
    printf '%s selftest: FAIL: the broken Makefile was rejected but not reported as a makefile FAIL\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "$tmp/broken.mk"; then
    printf '%s selftest: FAIL: the broken Makefile was rejected but the file was not named in the output\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'missing separator'; then
    printf "%s selftest: FAIL: the broken Makefile was rejected without make's own message\n  output: %s\n" "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf "PASS: a spaces-indented recipe is rejected (rc=%d) with the file named and make's message surfaced\n" "$rc"
  fi

  # 3-9. each malformed Dockerfile is REJECTED, named, and quotes the exact
  #      breakage the built-in parse claims to catch
  local fixture="" expect="" label=""
  for spec in \
    "broken-unknown.Dockerfile|unknown instruction 'RUM'|an unknown/misspelled instruction" \
    "broken-dangling.Dockerfile|dangling at end of file|a line continuation dangling at EOF" \
    "broken-from.Dockerfile|FROM with no image reference|FROM with no image reference" \
    "broken-alias.Dockerfile|invalid stage alias 'build:1'|an invalid AS alias" \
    "broken-dup.Dockerfile|duplicate stage alias 'build'|a duplicate stage alias" \
    "broken-fwd.Dockerfile|forward-references stage 'build'|a COPY --from naming a stage defined later"; do
    fixture="${spec%%|*}"
    spec="${spec#*|}"
    expect="${spec%%|*}"
    label="${spec#*|}"
    checks=$((checks + 1))
    out="$(bash "$SELF" --engine builtin "$tmp/$fixture" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      printf '%s selftest: FAIL: %s was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$label" "$out" >&2
      fails=$((fails + 1))
    elif ! printf '%s' "$out" | grep -q "$tmp/$fixture"; then
      printf '%s selftest: FAIL: %s was rejected but the file was not named\n  output: %s\n' "$PROG" "$label" "$out" >&2
      fails=$((fails + 1))
    elif ! printf '%s' "$out" | grep -qF "$expect"; then
      printf '%s selftest: FAIL: %s was rejected without the expected diagnosis (%s)\n  output: %s\n' "$PROG" "$label" "$expect" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: %s is rejected (rc=%d) and named — %s\n' "$label" "$rc" "$expect"
    fi
  done

  # the external-ref control: NOT a forward reference, must be ACCEPTED
  checks=$((checks + 1))
  out="$(bash "$SELF" --engine builtin "$tmp/external-from.Dockerfile" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: an external COPY --from=<image> ref was REJECTED (rc=%d) — false positive\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an external COPY --from=<image> ref is not mistaken for a stage (control, rc=0)\n'
  fi

  # 10. an explicit list in which NOTHING classifies is REJECTED, names the path,
  #     and prints no green at all
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/notes.md" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: an explicit list in which nothing classifies was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "not a makefile, not a dockerfile: $tmp/notes.md"; then
    printf '%s selftest: FAIL: the unclassifiable list was rejected but the path was not named\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q 'PASS'; then
    printf '%s selftest: FAIL: the unclassifiable list was rejected but a PASS line was printed anyway\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an explicit list in which nothing classifies is rejected (rc=%d) with the path named and no green printed\n' "$rc"
  fi

  # 11. the mixed shape the pre-commit wrapper produces is still ACCEPTED
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/clean.mk" "$tmp/notes.md" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: a mixed explicit list (1 clean Makefile + 1 plain file) was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "PASS  makefile    $tmp/clean.mk"; then
    printf '%s selftest: FAIL: the mixed list passed without reporting the makefile it checked\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'scope: 1 makefile(s), 0 dockerfile(s) verified'; then
    printf '%s selftest: FAIL: the mixed list passed without a real scope line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a mixed list (1 clean Makefile + 1 unclassifiable plain file) is still accepted (rc=%d)\n' "$rc"
  fi

  # 12. NEUTER PROOF, makefile arm: force the arm's verdict call to success in a
  #     copy of this script and re-run the SAME broken fixture — it must now be
  #     ACCEPTED, which is what proves the rejection above is caused by that arm.
  checks=$((checks + 1))
  local neutered_mk="$tmp/neutered-makefile-arm.sh"
  cp "$SELF" "$neutered_mk"
  sed -i.tmp -e 's|^\([[:space:]]*\)if ! _check_makefile "\$f"; then$|\1if false; then|' "$neutered_mk"
  rm -f "$neutered_mk.tmp"
  if cmp -s "$SELF" "$neutered_mk"; then
    printf '%s selftest: FAIL: the neuter sed no longer matches the makefile arm verdict line (NEUTER-MARK[makefile-arm-verdict]) — the causality proof would be vacuous\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(bash "$neutered_mk" "$tmp/broken.mk" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      printf '%s selftest: FAIL: NEUTER PROOF (makefile arm): with the arm neutered the same broken Makefile is STILL rejected (rc=%d) — the rejection does not come from that arm\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: NEUTER PROOF (makefile arm): neutered copy differs from the original and ACCEPTS the same broken Makefile (rc=0) — the rejection is caused by that arm\n'
    fi
  fi

  # 13. NEUTER PROOF, dockerfile arm (built-in engine), same construction
  checks=$((checks + 1))
  local neutered_df="$tmp/neutered-dockerfile-arm.sh"
  cp "$SELF" "$neutered_df"
  sed -i.tmp -e 's|^\([[:space:]]*\)if ! _check_dockerfile "\$f"; then$|\1if false; then|' "$neutered_df"
  rm -f "$neutered_df.tmp"
  if cmp -s "$SELF" "$neutered_df"; then
    printf '%s selftest: FAIL: the neuter sed no longer matches the dockerfile arm verdict line (NEUTER-MARK[dockerfile-arm-verdict]) — the causality proof would be vacuous\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(bash "$neutered_df" --engine builtin "$tmp/broken-unknown.Dockerfile" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      printf '%s selftest: FAIL: NEUTER PROOF (dockerfile arm, builtin): with the arm neutered the same malformed Dockerfile is STILL rejected (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: NEUTER PROOF (dockerfile arm, builtin engine): neutered copy differs from the original and ACCEPTS the same malformed Dockerfile (rc=0) — the rejection is caused by that arm\n'
    fi
  fi

  # ── missing-validator fail-closed (exit 2, the tool named) ──────────────────
  # A PATH shim directory holding symlinks to everything this script needs
  # EXCEPT the tool being hidden — so the check proves the script's own
  # resolution path, not a broken environment.
  local shim="$tmp/shim-bin"
  mkdir -p "$shim"
  local b p
  for b in sh bash dash env git grep sed awk cat rm mkdir mktemp dirname basename cmp printf ls chmod head tr uname cut sort wc; do
    p="$(command -v "$b" 2>/dev/null)" && ln -sf "$p" "$shim/$b"
  done

  # 14. no `make` at all
  checks=$((checks + 1))
  out="$(env PATH="$shim" bash "$SELF" "$tmp/clean.mk" 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: with make hidden the run exited %d, not 2 (fail-closed)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "'make' is not on PATH"; then
    printf '%s selftest: FAIL: with make hidden the error did not name the missing tool\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: with make hidden the makefile arm exits 2 and names the missing tool (rc=%d)\n' "$rc"
  fi

  # 15. hadolint AND python3 hidden -> the dockerfile arm cannot run
  checks=$((checks + 1))
  out="$(env PATH="$shim" bash "$SELF" --engine auto "$tmp/clean.Dockerfile" 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: with hadolint and python3 hidden the run exited %d, not 2 (fail-closed)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'hadolint is not on PATH and python3 is unusable'; then
    printf '%s selftest: FAIL: with both validators hidden the error did not name them\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: with hadolint and python3 hidden the dockerfile arm exits 2 and names the missing tools (rc=%d)\n' "$rc"
  fi

  # 16. a hadolint that is on PATH but CANNOT RUN must not turn every Dockerfile
  #     into a false rejection: auto falls back to the built-in parse, and an
  #     explicit --engine hadolint exits 2 naming the tool.
  checks=$((checks + 1))
  local broken_bin="$tmp/broken-bin"
  mkdir -p "$broken_bin"
  printf '#!/nonexistent/interpreter\n' >"$broken_bin/hadolint"
  chmod +x "$broken_bin/hadolint"
  out="$(env PATH="$broken_bin:$PATH" bash "$SELF" --engine auto "$tmp/clean.Dockerfile" 2>&1)"
  rc=$?
  out2="$(env PATH="$broken_bin:$PATH" bash "$SELF" --engine hadolint "$tmp/clean.Dockerfile" 2>&1)"
  rc2=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: an unusable hadolint on PATH made auto mode REJECT a clean Dockerfile (rc=%d) — false positive\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'dockerfile engine: builtin-parse'; then
    printf '%s selftest: FAIL: auto mode did not fall back to the built-in parse when hadolint cannot run\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif [ "$rc2" -ne 2 ]; then
    printf '%s selftest: FAIL: --engine hadolint with an unusable hadolint exited %d, not 2\n  output: %s\n' "$PROG" "$rc2" "$out2" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out2" | grep -q 'cannot run'; then
    printf '%s selftest: FAIL: the unusable-hadolint error did not name the problem\n  output: %s\n' "$PROG" "$out2" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an unusable hadolint does not cause a false rejection — auto falls back to the built-in parse (rc=%d) and --engine hadolint exits 2 (rc=%d)\n' "$rc" "$rc2"
  fi

  # 17. hadolint's SEVERITY POLICY: a WARNING-level finding must not fail the run
  #     and an ERROR-level finding must. CI caught the opposite (run
  #     35288203740, 2026-09-17: DL3018/DL3013/DL3016 warnings rejected this
  #     repo's own Dockerfiles because no --failure-threshold was passed). The
  #     shim below behaves like real hadolint: it reports the warning and exits
  #     nonzero UNLESS `--failure-threshold error` is passed, and it always exits
  #     nonzero for an error-level finding. So this check fails if the arm ever
  #     stops passing that flag — which is the regression that broke CI.
  checks=$((checks + 1))
  local hl_bin="$tmp/hadolint-severity-shim"
  mkdir -p "$hl_bin"
  cat >"$hl_bin/hadolint" <<'SHIM'
#!/usr/bin/env bash
# minimal hadolint stand-in: honours --failure-threshold
thr="warning"
prev=""
f=""
for a in "$@"; do
  case "$a" in
    --version) echo "hadolint SHIM 0.0.1"; exit 0 ;;
    --failure-threshold=*) thr="${a#*=}" ;;
  esac
  if [ "$prev" = "--failure-threshold" ]; then thr="$a"; fi
  case "$a" in -*) ;; *) f="$a" ;; esac
  prev="$a"
done
if grep -q 'SHIM_ERROR_LEVEL_FINDING' "$f" 2>/dev/null; then
  echo "$f:2 DL3000 error: shim error-level finding"
  exit 1
fi
if grep -Eq 'apk add|apt-get install|pip install|npm install' "$f" 2>/dev/null; then
  echo "$f:3 DL3018 warning: Pin versions in apk add."
  [ "$thr" = "error" ] || exit 1
  exit 0
fi
exit 0
SHIM
  chmod +x "$hl_bin/hadolint"
  printf 'FROM alpine\nRUN apk add curl\n' >"$tmp/warn.Dockerfile"
  printf 'FROM alpine\nSHIM_ERROR_LEVEL_FINDING\n' >"$tmp/err.Dockerfile"
  out="$(env PATH="$hl_bin:$PATH" bash "$SELF" --engine hadolint "$tmp/warn.Dockerfile" 2>&1)"
  rc=$?
  out2="$(env PATH="$hl_bin:$PATH" bash "$SELF" --engine hadolint "$tmp/err.Dockerfile" 2>&1)"
  rc2=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: a WARNING-level finding REJECTED the file (rc=%d) — the severity policy regressed, which is the CI-red class\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'advisory (not fatal)'; then
    printf '%s selftest: FAIL: the warning-level finding was neither fatal nor reported as advisory\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif [ "$rc2" -eq 0 ]; then
    printf '%s selftest: FAIL: an ERROR-level finding did NOT reject the file (rc=0) — the arm is now toothless\n  output: %s\n' "$PROG" "$out2" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: hadolint severity policy holds — a warning-level finding is advisory (rc=%d, reported, not fatal) while an error-level finding still rejects (rc=%d)\n' "$rc" "$rc2"
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
      --engine)
        [ "$#" -ge 2 ] || {
          _err "--engine needs a value (auto|hadolint|builtin)"
          return 2
        }
        ENGINE_REQUESTED="$2"
        shift 2
        ;;
      --engine=*)
        ENGINE_REQUESTED="${1#*=}"
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

  case "$ENGINE_REQUESTED" in
    auto | hadolint | builtin) ;;
    *)
      _err "unknown engine '$ENGINE_REQUESTED' (expected auto|hadolint|builtin)"
      return 2
      ;;
  esac

  run_check "${files[@]}"
}

main "$@"
exit $?
