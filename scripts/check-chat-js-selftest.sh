#!/usr/bin/env bash
#
# scripts/check-chat-js-selftest.sh — prove the chat page JS gate still bites
# (QA-CRIER-47).
#
# WHY THIS EXISTS
# ---------------
# scripts/check-chat-js.sh is the arm that would have caught the DF-CRIER-449
# merge-conflict markers in the served cmd/server/chat.html. A gate nobody
# proves is a gate that decays into a green: this selftest drives the REAL
# checker over fixtures it creates under ${TMPDIR:-/tmp} and requires it to
#
#   REJECT  a page carrying a bare conflict marker in an inline <script> block,
#           naming the page line the marker sits on;
#   ACCEPT  the SHIPPED cmd/server/chat.html (the page the gate exists to
#           protect) — the ACCEPT line the row asks for;
#   REJECT  a page whose inline block is a plain syntax error and carries NO
#           marker at all, so the verdict does not depend on text matching a
#           marker shape;
#   REJECT  a marker sitting in the HTML BODY, outside every <script> block,
#           which no per-block parse can see;
#   ACCEPT  the SAME marker fixture with only its marker lines removed — the
#           causal proof that the rejection above came from the marker and not
#           from the fixture's shape (the neuter-style control this repo's other
#           arms carry);
#   REFUSE (exit 2, and no ACCEPT line) a page with ZERO <script> blocks, and a
#           page whose only <script> block is external (src=) — the vacuous-green
#           refusal, so "found nothing" can never be reported as "passed".
#
# NO PLAYWRIGHT, NO BROWSER, NO NETWORK: the only tools are bash, node (the same
# validator the gate uses), awk and grep. This selftest exits 0 on a clean tree
# and 1 on a decayed gate — never the exit 77 "prerequisite missing" skip that
# scripts/chat-ui-browser-selftest.cjs takes when Playwright is absent.
#
# USAGE
#   bash scripts/check-chat-js-selftest.sh      # make chat-js-selftest
#
# EXIT CODES: 0 every case behaved, 1 at least one case did not (the failing
# case is named with the command that reproduces it), 2 the harness itself could
# not run (node missing, no checker on disk).

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PROG="chat-js-selftest"
CHECKER="$SCRIPT_DIR/check-chat-js.sh"
SHIPPED_PAGE="$REPO_ROOT/cmd/server/chat.html"

_info() { printf '%s: %s\n' "$PROG" "$*"; }
_fail() { printf '%s: FAIL: %s\n' "$PROG" "$*" >&2; }

if ! command -v node >/dev/null 2>&1; then
  _fail "node is not on PATH — the gate's validator is missing, so nothing here can run"
  exit 2
fi
if [ ! -f "$CHECKER" ]; then
  _fail "checker not found: $CHECKER"
  exit 2
fi
if [ ! -f "$SHIPPED_PAGE" ]; then
  _fail "shipped page not found: $SHIPPED_PAGE"
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/check-chat-js-selftest.XXXXXX")" || {
  _fail "mktemp -d failed"
  exit 2
}
cleanup() { [ -n "${WORK:-}" ] && [ -d "$WORK" ] && rm -rf "$WORK"; }
trap cleanup EXIT

pass=0
fail=0

# _verdict <name> <ok:0|1> <detail>
_verdict() {
  if [ "$2" -eq 0 ]; then
    pass=$((pass + 1))
    _info "case $1: PASS — $3"
  else
    fail=$((fail + 1))
    _fail "case $1: $3"
  fi
}

# run_checker <page> -> CHECK_RC, CHECK_OUT (stdout+stderr)
run_checker() {
  local page="$1"
  CHECK_OUT="$(bash "$CHECKER" --page "$page" 2>&1)"
  CHECK_RC=$?
}

has_line() { printf '%s\n' "$CHECK_OUT" | grep -q -- "$1"; }

# ── fixtures ────────────────────────────────────────────────────────────────
MARKER_PAGE="$WORK/marker-in-script.html"
MARKERLESS_PAGE="$WORK/marker-in-script-clean.html"
BROKEN_PAGE="$WORK/broken-no-marker.html"
BODY_MARKER_PAGE="$WORK/marker-in-body.html"
EMPTY_PAGE="$WORK/no-script-blocks.html"
EXTERNAL_PAGE="$WORK/external-only.html"

cat >"$MARKER_PAGE" <<'HTML'
<!doctype html>
<html>
<body>
<div id="app"></div>
<script>
<<<<<<< HEAD
const view = { branch: false, expandedBranches: [] };
=======
const groups = { groups: [], group: null };
>>>>>>> 34abd95 (feat(groups): add POST /groups and GET /groups)
const after = 1;
</script>
</body>
</html>
HTML

# The same fixture with ONLY its marker lines removed. Valid JavaScript on both
# sides of the removed markers, so a rejection of the fixture above cannot be
# blamed on anything but the markers.
grep -v -E '^(<<<<<<<|=======|>>>>>>>)' "$MARKER_PAGE" >"$MARKERLESS_PAGE"

cat >"$BROKEN_PAGE" <<'HTML'
<!doctype html>
<html>
<body>
<script>
const ok = 1;
const broken = ;
</script>
</body>
</html>
HTML

# A marker OUTSIDE every script block: the per-block parse cannot see this one,
# only the page-wide marker arm can.
cat >"$BODY_MARKER_PAGE" <<'HTML'
<!doctype html>
<html>
<body>
<<<<<<< HEAD
<div id="app"></div>
=======
<div id="other"></div>
>>>>>>> 34abd95
<script>
const fine = true;
</script>
</body>
</html>
HTML

cat >"$EMPTY_PAGE" <<'HTML'
<!doctype html>
<html>
<body>
<div id="app">no scripts on this page at all</div>
</body>
</html>
HTML

cat >"$EXTERNAL_PAGE" <<'HTML'
<!doctype html>
<html>
<body>
<div id="app"></div>
<script src="/static/app.js"></script>
</body>
</html>
HTML

# ── case 1: the shipped page is ACCEPTED ────────────────────────────────────
run_checker "$SHIPPED_PAGE"
if [ "$CHECK_RC" -eq 0 ] && has_line '^ACCEPT — '; then
  printf '%s\n' "$CHECK_OUT" | grep -- '^ACCEPT — '
  _verdict "shipped-page-accepted" 0 "cmd/server/chat.html ACCEPTed by the real gate (exit 0)"
else
  _verdict "shipped-page-accepted" 1 "expected ACCEPT + exit 0 from the shipped page, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 2: a bare conflict marker in an inline block is REJECTED ───────────
run_checker "$MARKER_PAGE"
marker_line="$(grep -n -m1 '^<<<<<<<' "$MARKER_PAGE" | cut -d: -f1)"
if [ "$CHECK_RC" -eq 1 ] && has_line '^REJECT — ' \
  && has_line 'merge-conflict marker' && has_line "page line $marker_line"; then
  printf '%s\n' "$CHECK_OUT" | grep -E '^(REJECT — |check-chat-js:   failing page line|check-chat-js:   marker at page line)'
  _verdict "marker-fixture-rejected" 0 "REJECT for the marker fixture, exit 1, page line $marker_line named"
else
  _verdict "marker-fixture-rejected" 1 "expected REJECT naming page line $marker_line + exit 1, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 3: the same fixture WITHOUT its marker lines is ACCEPTED ───────────
run_checker "$MARKERLESS_PAGE"
if [ "$CHECK_RC" -eq 0 ] && has_line '^ACCEPT — '; then
  printf '%s\n' "$CHECK_OUT" | grep -- '^ACCEPT — '
  _verdict "markerless-control-accepted" 0 "the same fixture with only its marker lines removed is ACCEPTed — the rejection in case 2 is caused by the marker"
else
  _verdict "markerless-control-accepted" 1 "expected ACCEPT + exit 0 from the marker-free control, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 4: an inline syntax error with NO marker is REJECTED ──────────────
run_checker "$BROKEN_PAGE"
if [ "$CHECK_RC" -eq 1 ] && has_line '^REJECT — ' && has_line 'does not parse'; then
  printf '%s\n' "$CHECK_OUT" | grep -E '^(REJECT — |check-chat-js:   failing page line)'
  _verdict "syntax-error-rejected" 0 "a plain syntax error with no marker is REJECTed (exit 1) — the parse arm stands on its own"
else
  _verdict "syntax-error-rejected" 1 "expected REJECT + exit 1 for a marker-free syntax error, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 5: a marker in the HTML body is REJECTED ──────────────────────────
run_checker "$BODY_MARKER_PAGE"
if [ "$CHECK_RC" -eq 1 ] && has_line '^REJECT — ' && has_line 'merge-conflict marker'; then
  printf '%s\n' "$CHECK_OUT" | grep -E '^(REJECT — |check-chat-js:   marker at page line)'
  _verdict "body-marker-rejected" 0 "a marker outside every script block is REJECTed — the arm is page-wide, not block-only"
else
  _verdict "body-marker-rejected" 1 "expected REJECT + exit 1 for a marker in the HTML body, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 6: zero <script> blocks is a REFUSAL (exit 2), never a PASS ───────
run_checker "$EMPTY_PAGE"
if [ "$CHECK_RC" -eq 2 ] && ! has_line '^ACCEPT — '; then
  _verdict "zero-blocks-refused" 0 "a page with no <script> block exits 2 and prints no ACCEPT line — no vacuous PASS"
else
  _verdict "zero-blocks-refused" 1 "expected exit 2 and no ACCEPT line for a script-free page, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── case 7: only-external blocks is also a REFUSAL (exit 2) ───────────────
run_checker "$EXTERNAL_PAGE"
if [ "$CHECK_RC" -eq 2 ] && ! has_line '^ACCEPT — '; then
  _verdict "external-only-refused" 0 "a page whose only <script> is external exits 2 and prints no ACCEPT line"
else
  _verdict "external-only-refused" 1 "expected exit 2 and no ACCEPT line for an external-only page, got exit $CHECK_RC:
$CHECK_OUT"
fi

# ── summary ─────────────────────────────────────────────────────────────────
total=$((pass + fail))
_info "dependency profile: bash + node ($(node --version 2>/dev/null)) + awk + grep — no Playwright, no browser, no network (this run exits 0, never a 77 skip)"
if [ "$fail" -ne 0 ]; then
  _fail "$fail of $total case(s) failed — the chat page JS gate has decayed; reproduce with: bash $CHECKER --page <fixture>"
  exit 1
fi
_info "PASS — $pass/$total case(s): the gate still rejects a conflict marker, a plain syntax error and a body marker; still accepts the shipped page and its marker-free control; still refuses a zero-block run."
exit 0
