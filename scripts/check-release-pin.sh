#!/usr/bin/env bash
#
# scripts/check-release-pin.sh — fail when README.md's release pins disagree
# with VERSION (RELEASE-017).
#
# WHY THIS EXISTS
# ---------------
# README.md's Quick Start carries the release identity in THREE hand-editable
# places — the heading, the asset-count sentence and the pinned install
# example — and the release cut bumps them BY HAND together with the
# CHANGELOG entry (docs/releases.md §1). Nothing kept the three in sync with
# the tag actually being cut: `make release VERSION=v0.6.0` validated the tag
# grammar, the tree, the branch and the gates, then tagged — and could have
# published a release whose own README told every new visitor to install the
# PREVIOUS one. This gate refuses the cut while any pin is one release old.
#
# WHAT IT CHECKS
# --------------
# The offenders are EXACTLY these three pin shapes (every occurrence of each —
# a duplicated stale pin is a failure, not just the first match):
#   1. the heading   `### Install prebuilt binaries (vX.Y.Z release assets)`
#   2. the sentence  `Release ` + backtick + `vX.Y.Z` + backtick + ` publishes N assets`
#   3. the example   `sh -s -- --version vX.Y.Z`
# A pin shape with ZERO occurrences is not a pass either: a renamed heading or
# a deleted sentence makes the check vacuous, so it is REFUSED (exit 2) —
# never a green over input it could not read.
#
# VERSION resolution, in order: the first argument ($1), else the $VERSION
# environment variable, else `git describe --tags --abbrev=0` (the nearest tag
# reachable from HEAD) run against the repository this script lives in. There
# is NO network dependency: the check reads README.md and, at most, local git.
#
# EXIT CODES
#   0  every pinned occurrence names VERSION
#   1  drift — at least one pinned occurrence names another version; each
#      offender prints `README.md:<line>:<pinned> != <VERSION>`
#   2  refusal — a missing/unreadable README, a pin shape with zero
#      occurrences, a version token that could not be extracted, or no
#      VERSION to check against. A refusal never prints a PASS line.
#
# OUTPUT CONTRACT (grep-able)
#   PASS     check-release-pin: <readme> matches <version> (N pinned occurrence(s) ...)
#   <readme>:<line>:<pinned> != <version>   one line per offender, on drift
#   FAIL     check-release-pin: <summary of the drift>
#   REFUSED  check-release-pin: <reason>
#
# SELFTEST — `bash scripts/check-release-pin.sh --selftest`
# (wired as `make release-pin-selftest`). Every arm runs THIS script against
# fixtures under ${TMPDIR:-/tmp}; the tracked README.md is only ever READ,
# never written:
#   a. PASS — a fixture carrying all three pins at VERSION is accepted (rc 0);
#   b. drift — the SAME fixture pinned ONE RELEASE OLD is rejected (rc 1) and
#      the failure names all three offender lines, each once, by line number;
#   c. refusal — a missing README is exit 2;
#   d. refusal — a README with none of the three pin shapes is exit 2 (zero
#      matches can never grade as green);
#   e. refusal — a README missing ONE pin shape (the other two present) is
#      exit 2, naming the missing shape;
#   f. refusal — no $1, no $VERSION, and a git describe that cannot resolve
#      (GIT_DIR pointed outside a repository) is exit 2, naming "no VERSION";
#   g. the tracked tree — when the tracked README's pins agree with each other
#      AND with the nearest tag, running this script with no VERSION source is
#      accepted (rc 0). A tree prepared for a release NEWER than the nearest
#      tag (pins bumped ahead of the cut) is a legitimate mid-release state,
#      so the arm prints a named SKIP and proves nothing then — never a false
#      red;
#   h. NEUTER proof — a copy of this script whose drift verdict is forced to
#      success ACCEPTS the one-release-old fixture of arm (b), which the real
#      script rejects on the same input, so the rejection is caused by this
#      verdict and not by an unrelated crash.
# Exit: 0 every arm behaved, 1 at least one arm misbehaved, 2 a dependency is
# missing (no mktemp).
#
# ch:trace row=RELEASE-017 spec=none:board-acceptance wave=none:serial-fallback test=scripts/check-release-pin.sh::selftest evidence=stdout:release-pin-arms witness=none:hosted-ci-pending verdict=none:open-finding commit=none:open memory=none:worker-not-authoritative

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PROG="check-release-pin"

# The three pin shapes, as ERE with the pinned version captured. The extractors
# in pin_of() below MUST mirror these one for one — a defensive vX.Y.Z grammar
# check there refuses (exit 2) if the two ever drift apart.
PIN_PATTERNS=(
  '^### Install prebuilt binaries \((v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*) release assets\)'
  '^Release `(v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*)` publishes [0-9]+ assets'
  'sh -s -- --version (v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*)'
)
PIN_NAMES=(
  "the '### Install prebuilt binaries (vX.Y.Z release assets)' heading"
  "the 'Release vX.Y.Z publishes N assets' sentence"
  "the 'sh -s -- --version vX.Y.Z' install example"
)

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_refused() { printf 'REFUSED  %s: %s\n' "$PROG" "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
check-release-pin.sh — README release-pin drift gate (RELEASE-017)

Verifies EVERY release pin in README.md names VERSION:
  1. the heading   `### Install prebuilt binaries (vX.Y.Z release assets)`
  2. the sentence  `Release `vX.Y.Z` publishes N assets`
  3. the example   `sh -s -- --version vX.Y.Z`

VERSION comes from the first argument, else $VERSION, else the nearest tag
(`git describe --tags --abbrev=0`). No network.

Usage:
  bash scripts/check-release-pin.sh [VERSION]        check the tracked README
  bash scripts/check-release-pin.sh --selftest       prove this checker
  bash scripts/check-release-pin.sh -h | --help      this text

Env:
  README_FILE   README to read (default <repo>/README.md)
  VERSION       the tag to check against (used when no argument is given)

Exit codes:
  0  every pinned occurrence names VERSION
  1  drift — each offender prints `README.md:<line>:<pinned> != <VERSION>`
  2  refusal — missing/unreadable README, a pin shape with zero occurrences,
     an unextractable version token, or no VERSION (never a PASS line)

The full contract — why the gate exists and every selftest arm — is in the
header comment of this file.
EOF
}

# pin_of <shape-index> <matched-line-text> — print the pinned version token the
# matched line carries. Each extractor mirrors the same-index PIN_PATTERNS
# entry; the caller refuses anything that does not come back vX.Y.Z-shaped.
pin_of() {
  local text="$2"
  case "$1" in
    0) printf '%s' "$text" | sed -E 's/^### Install prebuilt binaries \((v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*) release assets\).*/\1/' ;;
    1) printf '%s' "$text" | sed -E 's/^Release `(v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*)` publishes [0-9]+ assets.*/\1/' ;;
    2) printf '%s' "$text" | sed -E 's/.*sh -s -- --version (v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*).*/\1/' ;;
  esac
}

# check_one <readme> <version> — the verdict. Prints PASS and exits 0 only when
# every pinned occurrence in the README names VERSION.
check_one() {
  local readme="$1" version="$2"

  [ -n "$version" ] || _refused "no VERSION to check against — pass the tag as the first argument, export VERSION, or make a tag reachable so git describe can resolve one"
  [ -f "$readme" ] || _refused "README file not found: $readme"
  [ -r "$readme" ] || _refused "README file not readable: $readme"

  local i lines rc total=0 mismatches=0 lineno text pinned
  for i in "${!PIN_PATTERNS[@]}"; do
    lines=""
    rc=0
    lines="$(grep -nE "${PIN_PATTERNS[$i]}" "$readme")" || rc=$?
    if [ "$rc" -eq 1 ]; then
      _refused "${PIN_NAMES[$i]} has no occurrence in $readme — the pin shape is gone, so that pin cannot be verified (zero matches never grade green)"
    elif [ "$rc" -ne 0 ]; then
      _refused "grep failed (rc $rc) reading $readme for ${PIN_NAMES[$i]}"
    fi
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      lineno="${line%%:*}"
      text="${line#*:}"
      pinned="$(pin_of "$i" "$text")"
      case "$pinned" in
        v[0-9]*.[0-9]*.[0-9]*) ;;
        *) _refused "could not extract a vX.Y.Z token from $readme:$lineno for ${PIN_NAMES[$i]} — the grep pattern and the extractor have drifted apart" ;;
      esac
      total=$((total + 1))
      if [ "$pinned" != "$version" ]; then
        printf '%s:%s:%s != %s\n' "$readme" "$lineno" "$pinned" "$version" >&2
        mismatches=$((mismatches + 1))
      fi
    done <<<"$lines"
  done

  if [ "$mismatches" -ne 0 ]; then
    printf 'FAIL  %s: %d of %d pinned occurrence(s) in %s do not match %s — bump every pin above together with the CHANGELOG entry (docs/releases.md section 1) before cutting this release\n' \
      "$PROG" "$mismatches" "$total" "$readme" "$version" >&2
    return 1
  fi
  printf 'PASS  %s: %s matches %s (%d pinned occurrence(s) across the 3 pin sites)\n' \
    "$PROG" "$readme" "$version" "$total"
  return 0
}

# ---------- selftest ----------

selftest() {
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/release-pin-selftest.XXXXXX")" || {
    _err "mktemp failed; cannot build fixtures"
    return 2
  }
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT HUP INT TERM

  local failures=0
  _arm_pass() { printf 'PASS  selftest: %s\n' "$*"; }
  _arm_fail() { printf 'FAIL  selftest: %s\n' "$*"; failures=$((failures + 1)); }

  # The fixture mirrors README.md's Quick Start shape, including the UNPINNED
  # install command, which must never match (only the `sh -s -- --version`
  # example is a pin). _fixture <version> writes all three pins at <version>.
  _fixture() {
    cat >"$tmp/readme.md" <<EOF
# fixture
## Quick Start

### Install prebuilt binaries ($1 release assets)

Release \`$1\` publishes 8 assets, the complete set.

The unpinned command installs the latest release and is NOT a pin:

\`\`\`bash
curl -fsSL https://example.invalid/install.sh | sh
\`\`\`

To pin the published release:

\`\`\`bash
curl -fsSL https://example.invalid/install.sh | sh -s -- --version $1 --dir "\$HOME/bin"
\`\`\`
EOF
  }

  local out rc offenders distinct
  _fixture v0.6.0

  # a. PASS — all three pins name VERSION
  out="$(README_FILE="$tmp/readme.md" VERSION=v0.6.0 "$SELF" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -q "^PASS  $PROG"; then
    _arm_pass "three matching pins accepted (rc 0)"
  else
    _arm_fail "three matching pins: expected rc 0 + PASS line, got rc $rc: $out"
  fi

  # b. drift — the SAME fixture pinned ONE RELEASE OLD, and the failure must
  #    name all three offender lines, each once, by line number.
  _fixture v0.5.0
  out="$(README_FILE="$tmp/readme.md" VERSION=v0.6.0 "$SELF" 2>&1)" && rc=0 || rc=$?
  offenders="$(printf '%s\n' "$out" | grep -cE ':v0\.5\.0 != v0\.6\.0$')" || offenders=0
  distinct="$(printf '%s\n' "$out" | grep -E ':v0\.5\.0 != v0\.6\.0$' | sed -E 's/^[^:]*:([0-9]+):.*/\1/' | sort -u | wc -l)"
  if [ "$rc" -eq 1 ] && [ "$offenders" -eq 3 ] && [ "$distinct" -eq 3 ]; then
    _arm_pass "one-release-old pins rejected (rc 1) naming 3 distinct lines"
  else
    _arm_fail "one-release-old pins: expected rc 1 with 3 distinct offender lines, got rc $rc, $offenders offender line(s), $distinct distinct line(s): $out"
  fi

  # c. refusal — a missing README
  README_FILE="$tmp/does-not-exist.md" VERSION=v0.6.0 "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  if [ "$rc" -eq 2 ]; then
    _arm_pass "missing README refused (rc 2)"
  else
    _arm_fail "missing README: expected rc 2, got rc $rc"
  fi

  # d. refusal — zero matches (none of the three pin shapes present)
  cat >"$tmp/readme-no-pins.md" <<'EOF'
# fixture
## Quick Start

Install from source with `make build` — this document carries no release pins.
EOF
  README_FILE="$tmp/readme-no-pins.md" VERSION=v0.6.0 "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  if [ "$rc" -eq 2 ]; then
    _arm_pass "zero matches refused (rc 2)"
  else
    _arm_fail "zero matches: expected rc 2, got rc $rc"
  fi

  # e. refusal — ONE pin shape missing (heading + sentence present, the pinned
  #    install example deleted): the remaining pins cannot cover for it.
  cat >"$tmp/readme-missing-example.md" <<EOF
# fixture
## Quick Start

### Install prebuilt binaries (v0.6.0 release assets)

Release \`v0.6.0\` publishes 8 assets, the complete set.

The pinned install command was accidentally deleted from this document.
EOF
  out="$(README_FILE="$tmp/readme-missing-example.md" VERSION=v0.6.0 "$SELF" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -eq 2 ] && printf '%s\n' "$out" | grep -q "REFUSED  $PROG.*install example"; then
    _arm_pass "missing pin shape refused (rc 2, named)"
  else
    _arm_fail "missing pin shape: expected rc 2 + REFUSED naming the shape, got rc $rc: $out"
  fi

  # f. refusal — no VERSION anywhere: no argument, no $VERSION, and a git
  #    describe that cannot resolve (GIT_DIR pointed outside a repository).
  out="$(env -u VERSION GIT_DIR="$tmp/not-a-repo" README_FILE="$tmp/readme.md" "$SELF" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" -eq 2 ] && printf '%s\n' "$out" | grep -q "no VERSION"; then
    _arm_pass "no VERSION refused (rc 2)"
  else
    _arm_fail "no VERSION: expected rc 2 + refusal naming it, got rc $rc: $out"
  fi

  # g. the tracked tree — the gate's own claim. When the tracked README's
  #    pins agree with the nearest tag, running this script with no VERSION
  #    source must be accepted. A tree whose pins were bumped ahead of the
  #    cut (mid-release preparation) is a legitimate state this arm cannot
  #    speak to: it prints a named SKIP rather than a false red.
  local nearest tracked_pin
  nearest="$(env -u GIT_DIR -u GIT_WORK_TREE git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null)" || nearest=""
  tracked_pin="$(sed -nE 's/^### Install prebuilt binaries \((v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*) release assets\)$/\1/p' "$REPO_ROOT/README.md" 2>/dev/null | head -1)"
  if [ -z "$nearest" ] || [ -z "$tracked_pin" ]; then
    printf 'SKIP  selftest: tracked-tree arm needs a resolvable nearest tag and a pinned heading (nearest=%s pin=%s) — proves nothing here\n' \
      "${nearest:-unresolved}" "${tracked_pin:-absent}"
  elif [ "$nearest" = "$tracked_pin" ]; then
    out="$(env -u VERSION -u GIT_DIR -u GIT_WORK_TREE "$SELF" 2>&1)" && rc=0 || rc=$?
    if [ "$rc" -eq 0 ]; then
      _arm_pass "tracked README.md matches its nearest tag $nearest (rc 0)"
    else
      _arm_fail "tracked README.md vs nearest tag $nearest: expected rc 0, got rc $rc: $out"
    fi
  else
    printf 'SKIP  selftest: tracked README pins %s while the nearest tag is %s — a release newer than the last tag is being prepared, so the tracked-tree arm proves nothing here\n' \
      "$tracked_pin" "$nearest"
  fi

  # h. NEUTER proof — the same drift input exits 0 ONLY when the check is
  #    disabled. Force the drift verdict to success in a copy: it must ACCEPT
  #    the one-release-old fixture arm (b) rejects, and the real script must
  #    still reject the same input, so the rejection is caused by this verdict.
  local neutered="$tmp/check-neutered.sh"
  sed 's/^  if \[ "\$mismatches" -ne 0 \]; then$/  if false; then/' "$SELF" >"$neutered"
  chmod +x "$neutered"
  if cmp -s "$SELF" "$neutered"; then
    _arm_fail "NEUTER proof could not rewrite the drift verdict (copy is identical)"
  else
    _fixture v0.5.0
    local neuter_rc real_rc
    README_FILE="$tmp/readme.md" VERSION=v0.6.0 "$neutered" >/dev/null 2>&1 && neuter_rc=0 || neuter_rc=$?
    README_FILE="$tmp/readme.md" VERSION=v0.6.0 "$SELF" >/dev/null 2>&1 && real_rc=0 || real_rc=$?
    if [ "$neuter_rc" -eq 0 ] && [ "$real_rc" -eq 1 ]; then
      _arm_pass "NEUTER: drift exits 0 only when disabled (neutered rc 0, real rc 1)"
    else
      _arm_fail "NEUTER: expected neutered rc 0 + real rc 1 on the same input, got neutered $neuter_rc / real $real_rc"
    fi
  fi

  if [ "$failures" -ne 0 ]; then
    printf 'SELFTEST FAIL  %s: %d arm(s) misbehaved\n' "$PROG" "$failures" >&2
    return 1
  fi
  printf 'SELFTEST PASS  %s: every arm behaved (matching pins, one-release-old drift naming 3 lines, missing README, zero matches, missing pin shape, no VERSION, tracked tree, NEUTER)\n' "$PROG"
  return 0
}

# ---------- entry ----------

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
  --selftest)
    command -v mktemp >/dev/null 2>&1 || _refused "mktemp is required to build the selftest's fixtures"
    selftest
    exit $?
    ;;
  *)
    version=""
    if [ "$#" -ge 1 ] && [ -n "$1" ]; then
      version="$1"
    elif [ -n "${VERSION:-}" ]; then
      version="$VERSION"
    else
      version="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null)" || version=""
    fi
    check_one "${README_FILE:-$REPO_ROOT/README.md}" "$version"
    exit $?
    ;;
esac
