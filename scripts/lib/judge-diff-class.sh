#!/usr/bin/env bash
#
# scripts/lib/judge-diff-class.sh — fail-closed diff classifier: authorizes
# `gitreins task complete <id> --skip-tier2` ONLY for a diff whose changed path
# set contains zero source-bearing files (DF-CRIER-278).
#
# WHY THIS EXISTS
# ---------------
# Measured on tick 365. `gitreins task complete df-crier-203-expires-null-panel`
# was run twice on a BOARD-ONLY diff — three `.coding-hermes/` JSONL files, zero
# source files. Run 1 returned a merit FAIL (the judge read the pre-land tree).
# Run 2 died INCOMPLETE:
#
#     Cap exceeded: Input token budget (48.0M) exceeded (48.1M used)
#
# `.gitreins/history/` usage for those two tier-2 runs recorded 33,824,117 and
# 48,064,774 `tokens_in`, against 188K-706K for a normal source-diff judge. The
# evaluator's own repo exploration (`file_scope: full`) is what burns ~48M per
# run, and a board-only diff has nothing in it that a larger budget would
# resolve: the judge's exploration IS the cost, and there is nothing local for it
# to find.
#
# Raising the cap is explicitly rejected: it is the most expensive knob in the
# repo, and a bigger cap buys a longer exploration, so the next run starves at a
# higher number. The fix is crier-side and fail-closed — this classifier is the
# authorization gate. gitreins itself (pipx, fleet-wide) is out of scope.
#
# THE RULE
# --------
# Non-source iff the path matches ONLY this list (DF-CRIER-278):
#
#     docs/**, *.md (any depth), .coding-hermes/**, .gitreins/**,
#     LICENSE, NOTICE, .gitignore, .gitattributes,
#     .github/**, Makefile, Dockerfile*, *.yml/*.yaml under .github/ or docs/
#
# and "Be conservative: anything else (Go files, shell scripts, python,
# examples/**, scripts/**, specs/**, openapi yaml) counts as SOURCE — spec/
# openapi files drive generated code and gates, so they are source for this
# purpose."
#
#   board-only     := at least one changed path AND zero source-bearing paths
#   source-bearing := at least one source-bearing path
#
# BOUNDARY DECISIONS (this list is internally ambiguous; each resolution below
# leans toward SOURCE, which is the fail-closed direction — a source-bearing
# verdict only costs a normal tier-2 run, while a wrongly-granted board-only
# verdict skips the judge on a diff the rule meant to protect):
#
#   1. `examples/**`, `scripts/**` and `specs/**` are SOURCE *trees* and win over
#      the `*.md` rule, because the rule's own rationale names them as source
#      ("spec/openapi files drive generated code and gates"). So
#      `specs/AGENT-ECOSYSTEM.md` is SOURCE even though `*.md` (any depth) is
#      listed as non-source: the tree-level statement is the more specific
#      intent and the safer reading.
#   2. `openapi.yaml` / `openapi.yml` is SOURCE wherever it lives, because
#      "openapi yaml" is named as source and `docs/openapi.yaml` is the OpenAPI
#      SOURCE in this repo (`cmd/server/openapi.yaml` is the GENERATED copy,
#      asserted byte-identical by TestOpenAPIDocsSpec). Any other yaml under
#      `docs/` or `.github/` stays non-source, per the explicit list.
#   3. `Makefile` and `Dockerfile*` are recognized at the REPO ROOT only. A nested
#      one lives inside a source-bearing tree (this repo ships
#      `examples/agent-ecosystem/battery/Dockerfile`), so a nested build file is
#      SOURCE — the conservative direction for a root-anchored `Makefile,
#      Dockerfile*` pattern.
#
# Everything else — Go, shell, Python, TypeScript, proto, go.mod, generated
# copies, anything unrecognized — is SOURCE. An unrecognized path is never
# silently board-only.
#
# USAGE
# -----
#   # the authorization check (what docs/ops-evidence.md requires before a skip)
#   git diff --name-only <merge-base>...HEAD | bash scripts/lib/judge-diff-class.sh
#   -> verdict=board-only        (exit 0)
#   -> verdict=source-bearing    (exit 0)
#
#   # strict-gate form: a source-bearing diff is refused
#   ... | bash scripts/lib/judge-diff-class.sh --strict-verify
#   -> verdict=board-only        (exit 0)  |  verdict=source-bearing (exit 1)
#
#   # audit form: print the verdict, never gate (a source-bearing diff still 0)
#   ... | bash scripts/lib/judge-diff-class.sh --allow-source
#
# FLAGS
#   --strict-verify   gate mode: exit 0 on board-only, exit 1 on source-bearing.
#   --allow-source    the caller explicitly accepts a source-bearing diff: the
#                     verdict is still printed and the run still exits 0. This
#                     wins over --strict-verify when both are given (it is the
#                     more specific, explicitly-stated intent); an empty diff is
#                     refused regardless of either flag.
#   -h, --help        this text (stdin is not read).
#
# EXIT CODES
#   0  classified — board-only (always), or source-bearing with --allow-source
#      or without --strict-verify
#   1  --strict-verify and the diff is source-bearing
#   2  misuse (unknown flag, unexpected positional argument) — a usage line is
#      printed
#   3  EMPTY INPUT — the list carried no path at all (empty or blank lines only):
#      `error: empty diff — refusing to classify`. Refused in EVERY flag
#      combination, because there is no diff to classify and therefore nothing to
#      authorize: no diff = no authorization. No verdict line is printed.
#
# OUTPUT CONTRACT (grep-able)
#   stdout carries EXACTLY ONE line per classified run and nothing else:
#       `verdict=board-only` | `verdict=source-bearing`
#   The verdict token appears nowhere else — every diagnostic goes to stderr —
#   so `v=$(... | bash scripts/lib/judge-diff-class.sh)` is a one-token read.
#   stderr carries: one line per source-bearing path found (first 10, then a
#   `(+N more)` note), and one summary line
#       `... classified: board-only (2 unique path(s): 0 source-bearing, 2 non-source)`
#   The counts are real counts of UNIQUE paths read from stdin in this run;
#   duplicates are folded (a path counted once), so the summary cannot inflate.
#   A trailing CR on a line (CRLF input) is stripped, and blank lines are
#   ignored, so a path list that travelled through a CRLF-aware pipe still
#   classifies; a line that is only whitespace is not a path.
#
# SIDE EFFECTS: none. No network access, no writes, no temp files — a pure
# stdin -> verdict classifier. Sourcing it runs nothing.
#
# SELFTEST: scripts/lib/judge-diff-class-selftest.sh (`make
# judge-diff-class-selftest`) proves the contract on synthetic path lists,
# including a NEUTER proof that a verdict-forced copy of this script cannot pass
# the selftest's source-bearing assertions.
#
# DEPENDENCIES: bash. Nothing else (no git, no network, no temp files).

set -euo pipefail

PROG="judge-diff-class"

# How many source-bearing paths are named on stderr before the `(+N more)` note.
SOURCE_PATH_LIMIT=10

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*" >&2; }

# ── the verdict emitter ───────────────────────────────────────────────────────

# NEUTER-MARK[classifier-verdict]: the single line that puts a verdict on stdout.
# The selftest seds exactly this line (forcing the literal board-only verdict)
# and asserts the copy changed, so "the source-bearing case is decided BY THIS
# OUTPUT and not by accident" is a measured property — see the neuter proof in
# scripts/lib/judge-diff-class-selftest.sh.
_emit_verdict() { printf 'verdict=%s\n' "$1"; }

# ── classification ────────────────────────────────────────────────────────────

# _is_source_path <path> -> 0 when the path is SOURCE-BEARING, 1 when non-source.
# The order of the cases IS the precedence: the source-bearing trees and the
# openapi file are tested first (see BOUNDARY DECISIONS), then the non-source
# list, and anything unrecognized falls through to SOURCE.
_is_source_path() {
  local p="$1" base=""

  # A trailing slash is not part of a path as git reports it; normalize it away
  # so a directory-shaped entry still classifies against the same rules.
  p="${p%/}"
  base="${p##*/}"

  # ── SOURCE-BEARING first (the conservative direction) ───────────────────────
  # The trees the rule names as source. They win over `*.md`: a spec drives
  # generated code and gates, so a spec that happens to be markdown is source.
  case "$p" in
    specs | specs/* | examples | examples/* | scripts | scripts/*) return 0 ;;
  esac
  # "openapi yaml" is named as source, and docs/openapi.yaml is the OpenAPI
  # SOURCE of this repo (cmd/server/openapi.yaml is the generated copy).
  case "$base" in
    openapi.yaml | openapi.yml) return 0 ;;
  esac

  # ── NON-SOURCE, exactly the listed shapes ──────────────────────────────────
  # Board/state trees of the foreman and the co-harness — board JSONL, history
  # and status files. Root-anchored: `.coding-hermes/` and `.gitreins/` are the
  # repo-root directories, so anything elsewhere is not this pattern.
  case "$p" in
    .coding-hermes | .coding-hermes/*) return 1 ;;
    .gitreins | .gitreins/*) return 1 ;;
  esac
  # docs/** — prose and operator documentation.
  case "$p" in
    docs | docs/*) return 1 ;;
  esac
  # .github/** — workflow and CI configuration (the explicit yaml clause below is
  # subsumed by this one, and is kept for the paths it also covers under docs/).
  case "$p" in
    .github | .github/*) return 1 ;;
  esac
  # *.md at ANY depth, and the bare repository-metadata names at any depth.
  case "$base" in
    *.md) return 1 ;;
    LICENSE | LICENSE.* | NOTICE | NOTICE.* | .gitignore | .gitattributes) return 1 ;;
  esac
  # yaml under docs/ (the .github/ arm above already covers the other half; the
  # openapi exception was handled by the source pass).
  case "$p" in
    docs/*.yml | docs/*.yaml | docs/*.YML | docs/*.YAML) return 1 ;;
  esac
  # Root-level build files only — a nested one is inside a source tree.
  case "$p" in
    Makefile | makefile | GNUmakefile | Dockerfile | Dockerfile.*) return 1 ;;
  esac

  # Anything else — Go, shell, Python, TypeScript, go.mod, an unrecognized
  # extension, an extensionless file — is SOURCE. Never a silent board-only.
  return 0
}

# ── usage ─────────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
judge-diff-class.sh — fail-closed classifier for the tier-2 judge: board-only vs
source-bearing (DF-CRIER-278).

Usage:
  git diff --name-only <merge-base>...HEAD | bash scripts/lib/judge-diff-class.sh [FLAGS]

Reads one changed path per line on stdin and prints EXACTLY ONE line on stdout:
  verdict=board-only      no source-bearing path in the diff (>= 1 path, 0 source)
  verdict=source-bearing  at least one source-bearing path

Flags:
  --strict-verify   gate mode: exit 0 on board-only, exit 1 on source-bearing
  --allow-source    the caller explicitly accepts a source-bearing diff: the
                    verdict is still printed and the run still exits 0 (wins over
                    --strict-verify when both are given)
  -h, --help        this text (stdin is not read)

Exit codes:
  0  classified (board-only, or source-bearing per the flags above)
  1  --strict-verify and the diff is source-bearing
  2  misuse (unknown flag, unexpected positional argument)
  3  empty input — no changed path at all. Refused in EVERY flag combination
     ("error: empty diff — refusing to classify"): no diff = no authorization.
     No verdict line is printed for a refusal.

Non-source iff the path matches ONLY: docs/**, *.md (any depth),
.coding-hermes/**, .gitreins/**, LICENSE, NOTICE, .gitignore, .gitattributes,
.github/**, Makefile, Dockerfile*, *.yml/*.yaml under .github/ or docs/.
Anything else is SOURCE — including examples/**, scripts/** and specs/**, which
are source TREES and win over the *.md rule (a nested Makefile/Dockerfile* is
source for the same reason, and openapi.yaml/openapi.yml is source wherever it
lives). See the header of this script for each boundary decision.

The documented authorization/evidence flow is in docs/ops-evidence.md.
EOF
}

# ── main ──────────────────────────────────────────────────────────────────────

main() {
  local strict_verify=0 allow_source=0

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --strict-verify)
        strict_verify=1
        shift
        ;;
      --allow-source)
        allow_source=1
        shift
        ;;
      -h | --help | help)
        usage
        return 0
        ;;
      --)
        shift
        if [ "$#" -gt 0 ]; then
          _err "unexpected positional argument '$1' — the path list is read on stdin (try --help)"
          usage >&2
          return 2
        fi
        ;;
      -*)
        _err "unknown option '$1' — the path list is read on stdin (try --help)"
        usage >&2
        return 2
        ;;
      *)
        _err "unexpected positional argument '$1' — the path list is read on stdin, e.g. git diff --name-only <base>...HEAD | bash \$0 (try --help)"
        usage >&2
        return 2
        ;;
    esac
  done

  local -A seen=()
  local -a source_paths=()
  local line="" n_unique=0 n_source=0 n_nonsource=0

  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    # Skip a blank (or whitespace-only) line: it is not a path.
    [ -n "${line//[[:space:]]/}" ] || continue
    # Fold duplicates: the counts below are counts of UNIQUE paths.
    if [ -n "${seen["$line"]+set}" ]; then
      continue
    fi
    seen["$line"]=1
    n_unique=$((n_unique + 1))
    if _is_source_path "$line"; then
      n_source=$((n_source + 1))
      if [ "${#source_paths[@]}" -lt "$SOURCE_PATH_LIMIT" ]; then
        source_paths+=("$line")
      fi
    else
      n_nonsource=$((n_nonsource + 1))
    fi
  done

  # FAIL CLOSED on an empty diff, in every flag combination: there is nothing to
  # classify, so there is nothing to authorize.
  if [ "$n_unique" -eq 0 ]; then
    _err "error: empty diff — refusing to classify (no changed path on stdin; a board-only verdict requires at least one path)"
    return 3
  fi

  local verdict="board-only"
  if [ "$n_source" -gt 0 ]; then
    verdict="source-bearing"
  fi

  local p
  for p in ${source_paths[@]+"${source_paths[@]}"}; do
    _info "source-bearing: $p"
  done
  if [ "$n_source" -gt "${#source_paths[@]}" ]; then
    _info "(+$((n_source - ${#source_paths[@]})) more source-bearing path(s) not shown)"
  fi
  _info "classified: $verdict ($n_unique unique path(s): $n_source source-bearing, $n_nonsource non-source)"

  _emit_verdict "$verdict"

  if [ "$verdict" = "source-bearing" ] && [ "$strict_verify" -eq 1 ] && [ "$allow_source" -eq 0 ]; then
    _info "refusing: --strict-verify and the diff is source-bearing (a source-bearing diff never qualifies for --skip-tier2; raise the cap rung through the normal measured process instead)"
    return 1
  fi
  return 0
}

rc=0
main "$@" || rc=$?
exit "$rc"
