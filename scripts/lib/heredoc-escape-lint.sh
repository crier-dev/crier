#!/usr/bin/env bash
#
# scripts/lib/heredoc-escape-lint.sh — heredoc-escaping lint for the
# fleet-shared bunker-qa.sh generator (QA-CRIER-32).
#
# WHY THIS EXISTS
# ---------------
# ~/.hermes/scripts/bunker-qa.sh builds the per-agent remote QA script inside
# ONE generation heredoc: `build_remote_script()` opens `cat <<EOF` with an
# UNQUOTED delimiter, so the generator's own bash expands every unescaped `$`
# and backtick AT GENERATION TIME, on the generation host. Anything meant to
# reach the generated script must be escaped `\$` / `\` instead. Three workers
# hit this class on 2026-09-21 alone:
#   1. a double-escaped `\$\(curl\)` + a bare `$BX_TAG` shipped — generation
#      exited 1 and BX_TAG was unbound for ALL cells;
#   2. a backticked curl inside a block comment EXECUTED ON THE HOST at
#      generation time (curl stderr on every run; measured live on this very
#      tree: `bash bunker-qa.sh __gen-remote …` prints `curl: (2) no URL
#      specified` while still exiting 0);
#   3. two more backticks were removed from comments by hand the same day.
# Nothing guards the generator — the gitreins Tier-1 battery reads Go source
# only and bunker-qa.sh is fleet-shared and UNTRACKED, so no repo gate sees it.
# This lint is the guard: it reads the generator (path argument, default
# ~/.hermes/scripts/bunker-qa.sh, or `-` for stdin), isolates the
# build_remote_script heredoc region, and rejects the broken escape classes
# BEFORE the generator ships another corrupted remote script.
#
# USAGE
# -----
#   bash scripts/lib/heredoc-escape-lint.sh                  # live generator
#   bash scripts/lib/heredoc-escape-lint.sh FILE             # a generator copy
#   bash scripts/lib/heredoc-escape-lint.sh -                # generator on stdin
#
# WHAT IS FLAGGED (grep-level, fail-closed; documented heuristic —
# false-positive tolerance beats false negatives, but the CLEAN current
# generator must pass; the selftest proves both sides)
#
#   R1  backtick        any backtick in the heredoc body that is not
#                       immediately preceded by a backslash (and any
#                       double-escaped `` \\` ``): inside the unquoted heredoc
#                       a backtick is a command substitution that RUNS ON THE
#                       GENERATION HOST — even on a `#` comment line, because
#                       heredoc lines are data, not shell (that is exactly how
#                       the curl-in-a-comment incident fired).
#   R2  double-escape   `\\$` (two backslashes then `$`): the backslash is
#                       escaped, not the expansion — the generator bakes a
#                       literal backslash plus a LIVE expansion into the
#                       remote script. The file's convention is ONE backslash.
#   R3  bare $          any unescaped `$` that is not one of the sanctioned
#                       generation-time bakes — it either silently corrupts
#                       the generated text with a value from the generation
#                       host, or aborts generation outright under `set -u`.
#                       Allowed tokens (baked per-run by build_remote_script):
#                         $proj $agent $install_cmd $ci_cmd $native_cmd
#                         $ACT_URL $DETECT_TAG_COUNT $DETECT_PREV_TAG
#                         $DETECT_PREV_VERSION $DETECT_PKG_NAME
#                         $DETECT_PKG_ECOSYSTEM
#                       Comment-line prose quirks (documented in the
#                       generator, first non-blank char `#` ONLY — the same
#                       bare form in code is still rejected):
#                         $PATH          (log-prose in a comment; the
#                                         `[$]PATH` bracket idiom the CODE
#                                         uses is naturally outside this
#                                         scan: its `$` is followed by `]`)
#                         $(ulimit -v)   (QA-WARPFS-11 comment; pure command,
#                                         retained deliberately)
#                       Always rejected on ANY line (silent corruption or
#                       set -u abort, never legitimate in the body):
#                         $? $! $* $@ $# $$ $- $0..$9 and bare ${…}
#
# EXIT CODES
#   0  clean — PASS line carries the scanned region line range
#   1  violations found (each on stderr with its line number), or the
#      heredoc region could not be located (generator convention changed —
#      also a reject, never a silent skip)
#   2  the generator path was given but is missing/unreadable — fail closed,
#      naming the path (mirrors the shell-yaml checker's missing-tool rule)

set -euo pipefail

PROG=$(basename "$0")
DEFAULT_GENERATOR="$HOME/.hermes/scripts/bunker-qa.sh"

# Generation-time bakes: the ONLY bare `$WORD` tokens the generator intends
# to expand while building the heredoc (they are the function's own inputs).
ALLOWED_BAKES="proj,agent,install_cmd,ci_cmd,native_cmd,ACT_URL,DETECT_TAG_COUNT,DETECT_PREV_TAG,DETECT_PREV_VERSION,DETECT_PKG_NAME,DETECT_PKG_ECOSYSTEM"

_usage() {
  cat <<USAGE
usage: $PROG [FILE|-]

  no arg   lint the live generator ($DEFAULT_GENERATOR)
  FILE     lint that generator copy
  -        read the generator from stdin
  -h       this help

exit 0 clean · exit 1 violations/unlocatable region · exit 2 file missing
USAGE
}

_err() { printf '%s: %s\n' "$PROG" "$*" >&2; }

VIOL_COUNT=0
_reject() { # NEUTER-MARK[reject]: the selftest neuters this verdict choke-point
  # args: CLASS ABS_LINE MESSAGE — every rejection must carry its line number
  printf '%s: FAIL  %-16s line %s: %s\n' "$PROG" "$1" "$2" "$3" >&2
  VIOL_COUNT=$((VIOL_COUNT + 1))
}

# ── region location ───────────────────────────────────────────────────────────
# The generator has exactly one UNQUOTED heredoc, `cat <<EOF` … `EOF`, inside
# build_remote_script(). Deliberately narrow: a quoted `<<'EOF'` or `<<-EOF`
# does NOT match, so the lint can never wander into the generator's python
# heredocs (`<<'PY'`) or `<<<` here-strings.

_locate_region() { # FILE → prints "CATLINE EOFLINE" on stdout
  awk '
    started && /^EOF$/ { print start " " NR; exit }
    !started && /^[[:space:]]*cat[[:space:]]*<<EOF[[:space:]]*$/ { started = 1; start = NR }
  ' "$1"
}

_lint_file() { # FILE SOURCE_LABEL
  local file=$1 label=$2
  local region cat_line eof_line body_s body_e

  region="$(_locate_region "$file")"
  if [ -z "$region" ]; then
    _err "no unquoted 'cat <<EOF' heredoc found — generator convention changed, region not lintable: $label"
    return 1
  fi
  cat_line=${region%% *}
  eof_line=${region##* }
  body_s=$((cat_line + 1))
  body_e=$((eof_line - 1))
  if [ "$body_e" -lt "$body_s" ]; then
    _err "empty heredoc body (lines $cat_line-$eof_line): $label"
    return 1
  fi
  # Sanity: this must be build_remote_script's heredoc, not some other
  # unquoted heredoc that wandered in above the function.
  if ! awk -v s="$cat_line" 'NR < s && /build_remote_script/ { found = 1 } END { exit !found }' "$file"; then
    _err "the unquoted heredoc at line $cat_line is not preceded by build_remote_script — refusing to guess: $label"
    return 1
  fi
  printf '%s: heredoc region: cat<<EOF at line %s, EOF at line %s (scanning body lines %s-%s) of %s\n' \
    "$PROG" "$cat_line" "$eof_line" "$body_s" "$body_e" "$label"

  # slice — one read, all rule scans attach their own absolute line numbers
  local slice
  slice=$(sed -n "${body_s},${body_e}p" "$file")

  # ── R1a: unescaped backtick (executes on the generation host) ──────────────
  # R1b (double-escaped \\`) separately below; the lookbehind keeps the
  # file's 14 sanctioned \-escaped backticks green.
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    _reject "backtick" "$(( ${hit%%:*} + body_s - 1 ))" \
      "unescaped backtick in heredoc body — command substitution executes ON THE GENERATION HOST (escape as \\\` or remove): ${hit#*:}"
  done < <(printf '%s\n' "$slice" | grep -nP '(?<!\\)`' || true)

  # ── R1b: double-escaped backtick (\\`) ─────────────────────────────────────
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    _reject "backtick-double" "$(( ${hit%%:*} + body_s - 1 ))" \
      "double-escaped backtick (\\\\\`) — the generator bakes a literal backslash plus a live backtick into the remote script"
  done < <(printf '%s\n' "$slice" | grep -nP '\\\\`' || true)

  # ── R2: double-escaped dollar (\\$) ─────────────────────────────────────────
  # `\$\(curl\)` from incident 1 lands here: backslash-backslash-dollar.
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    _reject "double-escape-\$" "$(( ${hit%%:*} + body_s - 1 ))" \
      "double-escaped dollar (\\\\\$) — escapes the BACKSLASH, not the expansion; the generator bakes a literal backslash plus a LIVE \${…}/\$(…) into the remote script (convention: single backslash)"
  done < <(printf '%s\n' "$slice" | grep -nP '\\\\\$' || true)

  # ── R3a: bare $WORD / $( / ${ outside the bake allowlist ───────────────────
  # grep only nominates lines; the walk below re-derives each match in bash
  # and consumes it by OFFSET (prefix-strip cannot advance past a match that
  # sits mid-line — the ${var#pat} form only removes at the string's head and
  # made the first draft spin forever). The allowlist and the escaped-\$ skip
  # are applied per match on the ORIGINAL line text.
  local hit rel abs text comment fragment abspos before mstart mlen allowed
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    rel=${hit%%:*}
    abs=$((rel + body_s - 1))
    text=${hit#*:}
    if [[ $text =~ ^[[:space:]]*\# ]]; then comment=1; else comment=0; fi
    fragment=$text
    while [[ $fragment =~ \$([A-Za-z_][A-Za-z_0-9]*|\(|\{) ]]; do
      mstart=${BASH_REMATCH[0]} # '$proj' | '$(' | '${'
      mlen=${#mstart}
      local prefix=${fragment%%"$mstart"*}
      abspos=$(( ${#text} - ${#fragment} + ${#prefix} ))
      if [ "$abspos" -gt 0 ]; then before=${text:abspos-1:1}; else before=""; fi
      if [ "$before" = "\\" ]; then
        # correctly single-escaped \$ — the file's convention
        fragment=${text:abspos+mlen}
        continue
      fi
      allowed=0
      local tok=${mstart#\$}
      if [[ ",$ALLOWED_BAKES," == *",${tok},"* ]]; then
        allowed=1
      elif [ "$comment" -eq 1 ] && [ "$tok" = "PATH" ]; then
        # documented prose quirk: log text quoting $PATH inside a comment
        allowed=1
      elif [ "$comment" -eq 1 ] && [ "$mstart" = "\$(" ] \
        && [[ ${text:abspos} == '$(ulimit -v)'* ]]; then
        # documented QA-WARPFS-11 comment; ulimit -v is pure, retained deliberately
        allowed=1
        mlen=${#mstart}
      fi
      if [ "$allowed" -ne 1 ]; then
        _reject "bare-\$-token" "$abs" \
          "bare \\\$(…)/\\\${…} expansion in heredoc body — expands at generation time (silent corruption) or aborts under set -u; escape the \\\$ so it reaches the remote script"
      fi
      fragment=${text:abspos+mlen}
    done
  done < <(printf '%s\n' "$slice" | grep -nP '(?<!\\)\$[A-Za-z_{(]' || true)

  # ── R3b: bare special params/positionals — never legitimate in the body ────
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    _reject "bare-\$-special" "$(( ${hit%%:*} + body_s - 1 ))" \
      "bare special parameter/positional (\$? \$! \$* \$@ \$# \$\$ \$- \$0-9) in heredoc body — bakes the GENERATOR's value into the remote script (or aborts under set -u)"
  done < <(printf '%s\n' "$slice" | grep -nP '(?<!\\)\$[?@*#!$0-9-]' || true)

  if [ "$VIOL_COUNT" -gt 0 ]; then
    printf '%s: FAIL — %d violation(s) in build_remote_script heredoc (body lines %s-%s) of %s\n' \
      "$PROG" "$VIOL_COUNT" "$body_s" "$body_e" "$label"
    return 1
  fi
  printf '%s: PASS — 0 violation(s); scanned build_remote_script heredoc of %s (cat<<EOF line %s, EOF line %s, body lines %s-%s)\n' \
    "$PROG" "$label" "$cat_line" "$eof_line" "$body_s" "$body_e"
  return 0
}

main() {
  local target=""
  case "${1:-}" in
    -h | --help | help) _usage; return 0 ;;
    "") target=$DEFAULT_GENERATOR ;;
    -) target="-" ;;
    -*) _err "unknown option '$1' (try --help)"; return 2 ;;
    *) target=$1 ;;
  esac

  if [ "$target" = "-" ]; then
    local tmp
    tmp=$(mktemp "${TMPDIR:-/tmp}/heredoc-escape-lint.XXXXXX") || {
      _err "mktemp failed"
      return 2
    }
    trap 'rm -f "$tmp"' EXIT
    cat >"$tmp"
    _lint_file "$tmp" "<stdin>"
    return $?
  fi

  # fail closed: a missing/unreadable generator is exit 2 naming the path —
  # never a silent skip (mirrors check-shell-yaml's missing-tool rule)
  if [ ! -r "$target" ]; then
    _err "generator not readable: $target"
    return 2
  fi
  _lint_file "$target" "$target"
}

main "$@"
exit $?
