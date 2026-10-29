#!/usr/bin/env bash
#
# scripts/check-chat-js.sh — whole-page JavaScript syntax gate for the served
# chat page (QA-CRIER-47).
#
# WHY THIS EXISTS
# ---------------
# cmd/server/chat.html shipped with TWO unresolved merge-conflict blocks
# (`<<<<<<< HEAD` / `=======` / `>>>>>>> 34abd95`) from daecd07, the DF-CRIER-449
# groups merge. The served /chat page was therefore a JavaScript syntax error:
# the page could not execute at all. NOTHING IN THE REPO SAW IT, and every gate
# that ran printed a green over it:
#
#   * scripts/chat-refusal-truth.cjs (41/41) and scripts/chat-lifecycle-truth.cjs
#     (24/24) PASSED on the broken file, because both extract named function
#     REGIONS by string index and eval them in isolation — a marker outside those
#     regions is invisible to them, so their verdict is scoped to those regions
#     and never to the page;
#   * scripts/chat-management-browser.cjs and scripts/chat-ui-browser-selftest.cjs
#     SKIP (exit 77) when Playwright is absent, which is the normal CI shape;
#   * the Go suite only EMBEDS the file (go:embed), so it compiles the marker
#     bytes happily;
#   * scripts/e2e-battery.sh never parses the page JS.
#
# The defect class is "nothing parses the artifact whole". This arm parses the
# artifact whole: it extracts EVERY inline <script> block from the page and runs
# `node --check` on each one, so a marker (or any other syntax error) in the
# served page fails the build like a compile error instead of shipping.
#
# USAGE
# -----
#   bash scripts/check-chat-js.sh                 # the tracked page (cmd/server/chat.html)
#   bash scripts/check-chat-js.sh --page FILE     # any page (used by the selftest)
#   make chat-js-check                            # the gate, as wired into CI
#
# WHAT IS CHECKED
# ---------------
#   1. merge-conflict markers anywhere in the page — a line whose first
#      non-space characters are `<<<<<<<`, `=======` or `>>>>>>>` (the HTML body
#      included: a marker outside a <script> block is still a broken artifact,
#      and the per-block parse below cannot see it).
#   2. every INLINE <script> block — extracted by scripts/check-chat-js.sh's own
#      awk extractor (tag on one line, tag wrapped over several lines, block on a
#      single line, and several blocks per page are all handled) and parsed with
#      `node --check`. A block with `src=` carries no inline code and a block with
#      a non-JavaScript `type=` (e.g. application/json) is not JavaScript: both are
#      SKIPPED with a printed reason, never silently. `type="module"` IS parsed
#      (written as .mjs so node --check reads it as a module).
#
# THE LINE OFFSET IS NAMED, NOT JUST THE FILE
#   A rejected block is reported with BOTH the page line its content starts at
#   (the block's offset, from the extractor) and the page line node's own
#   `file:line` maps to, so a reader is sent to the marker in the PAGE, not to a
#   line number inside a temp file that no longer exists.
#
# THE ZERO-BLOCK RULE (never a vacuous PASS)
#   A run that extracts ZERO inline JavaScript blocks exits 2 and prints no
#   ACCEPT line. A green over zero parsed blocks is exactly the blank green this
#   arm exists to remove — a page whose <script> tag was renamed, or a passing
#   path that stopped finding the block, would otherwise report success while
#   verifying nothing.
#
# EXIT CODES
#   0  ACCEPT — every inline block parsed and no marker was found
#   1  REJECT — a marker was found, or at least one inline block failed to parse
#   2  REFUSED — node missing, page missing/unreadable, or zero inline blocks
#      extracted (nothing was verified, so nothing may be reported as passing)
#
# LIMITS (stated, not implied)
#   * It checks SYNTAX only — a page that parses can still be a broken UI. The
#     truth harnesses and the browser selftests own behaviour.
#   * It does not execute the page, start a server, or touch the network.
#   * A block is delimited by the first `</script` at or after its content, which
#     is the HTML parser's own rule: a JS string carrying the literal `</script>`
#     ends the block for the browser too, so the extractor is faithful to the
#     defect rather than papering over it.
#   * A `<script>` tag inside an HTML comment is extracted. This page has none;
#     excluding it would need a real HTML parser, which is not worth a dependency
#     for the gate's purpose (a mis-extracted block is a loud REJECT, never a
#     silent accept).
#
# DEPENDENCIES: bash, awk, grep, sed, mktemp, wc — and node, which is the
# validator this arm cannot run without (a missing node is exit 2, named).

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PROG="check-chat-js"
DEFAULT_PAGE="cmd/server/chat.html"
PAGE="$DEFAULT_PAGE"
NODE_BIN=""
NODE_DESC=""
NODE_ERR_LIMIT="${CHECK_CHAT_JS_ERR_LIMIT:-20}"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }
# The verdict lines start at column 1 so a caller (and the selftest) can grep a
# verdict without matching the diagnostics that explain it.
_accept() { printf 'ACCEPT — %s\n' "$*"; }
_reject() { printf 'REJECT — %s\n' "$*"; }

usage() {
  cat <<'EOF'
check-chat-js.sh — whole-page JavaScript syntax gate for the served chat page (QA-CRIER-47)

Usage:
  bash scripts/check-chat-js.sh                  # check cmd/server/chat.html
  bash scripts/check-chat-js.sh --page FILE      # check FILE instead

Extracts EVERY inline <script> block from the page, parses each one with
`node --check`, and rejects the page if any block fails to parse or if the page
carries a merge-conflict marker (`<<<<<<<` / `=======` / `>>>>>>>`).

Exit codes: 0 ACCEPT (all blocks parsed, no marker), 1 REJECT (a marker or an
unparseable block), 2 REFUSED (node missing, page missing, or zero inline blocks
extracted — a run that verified nothing never reports a PASS).

A rejected block is named by its page line offset, so the report points at the
page a reader has, not at a temporary file.
EOF
}

# ── argument parsing ──────────────────────────────────────────────────────────
while [ "$#" -gt 0 ]; do
  case "$1" in
    --page)
      if [ "$#" -lt 2 ]; then
        _err "--page needs a FILE argument"
        exit 2
      fi
      PAGE="$2"
      shift 2
      ;;
    --page=*)
      PAGE="${1#--page=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      _err "unknown argument: $1 (see --help)"
      exit 2
      ;;
  esac
done

# A relative --page is resolved against the working directory first (so
# `--page /tmp/fixture.html` and `--page fixture.html` from a scratch dir both
# behave), then against the repo root (so `--page cmd/server/chat.html` works
# from anywhere).
case "$PAGE" in
  /*) ;;
  *)
    if [ -f "$PAGE" ]; then
      PAGE="$PWD/$PAGE"
    else
      PAGE="$REPO_ROOT/$PAGE"
    fi
    ;;
esac

# ── validator resolution ─────────────────────────────────────────────────────
# A missing validator is exit 2 with the mode named — never a silent skip.
if ! command -v node >/dev/null 2>&1; then
  _err "node is not on PATH — refusing to skip the page JavaScript parse."
  _err "  mode: node --check per inline <script> block"
  _err "  install Node.js (18+ ships `node --check`) and re-run."
  exit 2
fi
NODE_BIN="$(command -v node)"
NODE_DESC="node $NODE_BIN ($("$NODE_BIN" --version 2>/dev/null || echo version-unresolvable))"

if [ ! -f "$PAGE" ]; then
  _err "page not found: $PAGE"
  _err "  a page that is not there cannot be parsed, so this is a refusal (2), not a pass."
  exit 2
fi
if [ ! -r "$PAGE" ]; then
  _err "page not readable: $PAGE"
  exit 2
fi

# ── scratch space ────────────────────────────────────────────────────────────
TMPDIR_BASE="${TMPDIR:-/tmp}"
CHAT_JS_TMP="$(mktemp -d "$TMPDIR_BASE/check-chat-js.XXXXXX")" || {
  _err "mktemp -d failed under $TMPDIR_BASE"
  exit 2
}
cleanup() { [ -n "${CHAT_JS_TMP:-}" ] && [ -d "$CHAT_JS_TMP" ] && rm -rf "$CHAT_JS_TMP"; }
trap cleanup EXIT

# ── the extractor ────────────────────────────────────────────────────────────
# Every inline <script> block, written to <tmp>/block-<n>.js (or .mjs for
# type="module"), with "<n> <first-content-page-line> <attrs>" appended to
# <tmp>/manifest. Deliberately self-contained: no python, no node in the
# extraction path (node is the VALIDATOR, and using it to find the blocks would
# make the extractor part of what it is supposed to be checking).
cat >"$CHAT_JS_TMP/extract.awk" <<'AWK'
function cut(s,   i) {
  i = index(tolower(s), "</script")
  if (i > 0) return substr(s, 1, i - 1)
  return s
}
function add(s) {
  if (have) buf = buf "\n" s; else { buf = s; have = 1 }
}
function flush(   f, ext) {
  n++
  ext = (attrs ~ /type[[:space:]]*=[[:space:]]*["'\'']?module/) ? ".mjs" : ".js"
  f = dir "/block-" n ext
  print buf > f
  close(f)
  print n " " off " " attrs >> (dir "/manifest")
  close(dir "/manifest")
  inblock = 0; pending = ""; have = 0; buf = ""; attrs = ""
}
{
  line = $0
  low = tolower(line)
  if (!inblock) {
    p = index(low, "<script")
    if (p == 0) next
    after = substr(line, p + 7)
    lgt = index(tolower(after), ">")
    if (lgt == 0) {                       # the open tag wraps onto the next line
      inblock = 1; pending = tolower(after); off = NR + 1; have = 0; buf = ""; attrs = ""
      next
    }
    attrs = tolower(substr(after, 1, lgt - 1))
    body = substr(after, lgt + 1)
    inblock = 1
    if (body ~ /^[[:space:]]*$/) { off = NR + 1; have = 0; buf = "" } else { off = NR; have = 1; buf = body }
    if (index(tolower(body), "</script") > 0) flush()
    next
  }
  if (pending != "") {                    # still reading a wrapped open tag
    lgt = index(low, ">")
    if (lgt == 0) { pending = pending " " low; next }
    attrs = pending " " substr(low, 1, lgt - 1); pending = ""
    body = substr(line, lgt + 1)
    if (body ~ /^[[:space:]]*$/) { have = 0; buf = "" } else { have = 1; buf = body }
    if (index(tolower(body), "</script") > 0) flush()
    next
  }
  if (index(low, "</script") > 0) { add(cut(line)); flush(); next }
  add(line)
}
AWK

awk -v dir="$CHAT_JS_TMP" -f "$CHAT_JS_TMP/extract.awk" "$PAGE" || {
  _err "block extraction failed (awk exited nonzero) — nothing was verified"
  exit 2
}

# ── report the validator and the scope before any verdict ───────────────────
_info "page: $PAGE"
_info "validator: $NODE_DESC — node --check on every inline <script> block"

manifest="$CHAT_JS_TMP/manifest"
total_blocks=0
if [ -f "$manifest" ]; then
  total_blocks="$(wc -l <"$manifest" | tr -d ' ')"
fi

# ── arm 1: merge-conflict markers, anywhere in the page ─────────────────────
# Reported for the whole file, not only inside a <script> block: a marker in the
# HTML body is the same broken artifact and no per-block parse can see it.
marker_count=0
marker_report=""
while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  marker_count=$((marker_count + 1))
  marker_report="$marker_report"$'\n'"  marker at page line ${hit%%:*}: ${hit#*:}"
done <<EOF
$(grep -n -E '^[[:space:]]*(<<<<<<<|=======|>>>>>>>)([[:space:]]|$)' "$PAGE" || true)
EOF

# ── arm 2: parse every inline JavaScript block ──────────────────────────────
checked=0
skipped=0
failed=0
failure_report=""

if [ "$total_blocks" -gt 0 ]; then
  while IFS=' ' read -r block_no block_off block_attrs; do
    [ -n "${block_no:-}" ] || continue
    block_attrs="${block_attrs:-}"

    # An external block has no inline code to parse.
    case "$block_attrs" in
      *src=*)
        skipped=$((skipped + 1))
        _info "SKIP block $block_no (page line $block_off): external src= — no inline JavaScript to parse"
        continue
        ;;
    esac

    # A block whose type is not JavaScript is not JavaScript.
    case "$block_attrs" in
      *type=*)
        case "$block_attrs" in
          *type=*javascript*|*type=*ecmascript*|*type=*module*) : ;;
          *)
            skipped=$((skipped + 1))
            _info "SKIP block $block_no (page line $block_off): $block_attrs is not a JavaScript type — not parsed"
            continue
            ;;
        esac
        ;;
    esac

    case "$block_attrs" in
      *type=*module*) block_file="$CHAT_JS_TMP/block-$block_no.mjs" ;;
      *) block_file="$CHAT_JS_TMP/block-$block_no.js" ;;
    esac

    checked=$((checked + 1))
    node_err="$CHAT_JS_TMP/block-$block_no.err"
    if "$NODE_BIN" --check "$block_file" >"$node_err" 2>&1; then
      continue
    fi

    failed=$((failed + 1))
    # node --check prints `<path>:<line>` as its first line; that line is
    # BLOCK-relative, so the page line is offset + (rel - 1).
    rel_line=""
    rel_line="$(sed -n '1s/^.*:\([0-9][0-9]*\)$/\1/p' "$node_err")"
    page_line=""
    if [ -n "$rel_line" ]; then
      page_line=$((block_off + rel_line - 1))
    fi
    _reject "$PAGE: block $block_no of $total_blocks (content starts at page line $block_off) does not parse — node --check exited nonzero"
    if [ -n "$page_line" ]; then
      _info "  failing page line: $page_line (node reported block-relative line $rel_line)"
    else
      _info "  failing page line: unresolved — node's report carried no file:line (block content starts at page line $block_off)"
    fi
    _info "  node --check output (first $NODE_ERR_LIMIT line(s)):"
    sed -n "1,${NODE_ERR_LIMIT}p" "$node_err" | sed 's/^/    /'
  done <"$manifest"
fi

# ── the zero-block refusal: nothing verified means nothing may pass ─────────
if [ "$total_blocks" -eq 0 ]; then
  _err "extracted zero <script> blocks from $PAGE — refusing a vacuous PASS."
  _err "  scope: 0 block(s) found. A page whose inline JavaScript cannot be found is"
  _err "  not a page that parses: check the <script> tag, or pass the right --page."
  exit 2
fi
if [ "$checked" -eq 0 ]; then
  _err "found $total_blocks <script> block(s) but ZERO inline JavaScript block(s) to parse"
  _err "  (every block was external or a non-JavaScript type) — refusing a vacuous PASS."
  exit 2
fi

# ── arm 1 verdict, printed after the parse detail so both arms are visible ───
if [ "$marker_count" -gt 0 ]; then
  _reject "$PAGE: $marker_count merge-conflict marker line(s) in the page — the page cannot execute"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    _info "$line"
  done <<EOF
$marker_report
EOF
fi

if [ "$marker_count" -gt 0 ] || [ "$failed" -gt 0 ]; then
  _info "scope: $total_blocks block(s) extracted, $checked parsed, $skipped skipped (external/non-JS), $failed unparseable; $marker_count marker line(s)"
  exit 1
fi

_info "scope: $total_blocks block(s) extracted, $checked parsed, $skipped skipped (external/non-JS), 0 unparseable; 0 marker line(s)"
_accept "$PAGE: $checked inline script block(s) parsed clean by node --check and no merge-conflict marker in the page"
exit 0
