#!/usr/bin/env bash
#
# scripts/check-readme-api-count.sh — fail when README.md's API inventory
# sentence disagrees with docs/openapi.yaml (DF-CRIER-492).
#
# WHY THIS EXISTS
# ---------------
# README.md publishes a hand-maintained inventory sentence —
#
#   "documented as 66 paths / 84 operations in `docs/openapi.yaml`"
#
# — and nothing kept it honest: it had already drifted to 61 / 78 while the
# spec declared 66 / 84, so the front page under-sold the documented surface.
# The sentence is prose, so every route added to the spec silently ages it.
#
# cmd/server/docsclaims_test.go's TestREADMEOpenAPIInventoryMatchesSpec pins
# the same invariant from the Go suite (yaml.v3). This script is the
# operator- and CI-visible arm over the same two files, using an INDEPENDENT
# parser (PyYAML) with its own selftest, so the two arms must agree: a
# disagreement between them is itself a signal that one parser is wrong.
#
# WHAT IT CHECKS
# --------------
#   1. docs/openapi.yaml parses and declares at least one path. A document
#      with zero paths is REFUSED (exit 2) — an inventory check over an empty
#      document would be a vacuous green.
#   2. "operations" counts METHOD keys only (get/put/post/delete/options/
#      head/patch/trace). OpenAPI also allows path-level keys that are NOT
#      operations — `parameters`, `summary`, `description`, `servers` — so
#      counting every element under a path would inflate the number and could
#      mask real drift (the selftest carries a path-level `parameters` fixture
#      whose naive element count is 4 against 3 real operations).
#   3. EVERY occurrence of the sentence in README.md must carry the spec's
#      counts. A second, stale sentence is a failure — checking only the first
#      match is how a duplicated paragraph escapes the gate.
#
# USAGE
# -----
#   bash scripts/check-readme-api-count.sh              # check the tracked pair
#   bash scripts/check-readme-api-count.sh --selftest   # prove this checker
#   bash scripts/check-readme-api-count.sh -h | --help
#
# ENV
#   README_FILE         README to read        (default <repo>/README.md)
#   SPEC_FILE           OpenAPI document      (default <repo>/docs/openapi.yaml)
#   README_API_PYTHON   interpreter with PyYAML (default: python3 on PATH)
#
# EXIT CODES
#   0  README's inventory sentence matches the spec
#   1  drift — counts disagree, the sentence is missing, or an occurrence is
#      unparseable (the failure names every offending occurrence)
#   2  refusal — a missing/unreadable file, a missing interpreter, an
#      interpreter without PyYAML, an unparseable spec, or a spec with no paths.
#      A refusal never prints a PASS line.
#
# OUTPUT CONTRACT (grep-able)
#   PASS  check-readme-api-count: README matches docs/openapi.yaml (N paths / M operations)
#   FAIL  check-readme-api-count: <what disagrees>
#   REFUSED  check-readme-api-count: <reason>
#
# SELFTEST — `bash scripts/check-readme-api-count.sh --selftest`
# (wired as `make readme-api-selftest` and run by CI). Every arm runs THIS
# script against fixtures under ${TMPDIR:-/tmp}, so the repo tree is untouched
# and the arms are deterministic:
#   a. a consistent pair is ACCEPTed (rc 0), and the PASS line carries the
#      parsed counts;
#   b. a README that over-counts operations by counting a path-level
#      `parameters:` key (2/4 vs the real 2/3) is REJECTed (rc 1) — the arm
#      that proves method-keyed counting and drift detection at once;
#   c. an under-counted path count (1/3 vs 2/3) is REJECTed (rc 1);
#   d. a README with NO inventory sentence is REJECTed (rc 1), never passed
#      over;
#   e. a README with a correct sentence AND a second stale one is REJECTed
#      (rc 1) — every occurrence is checked;
#   f. a spec declaring zero paths is REFUSED (rc 2);
#   g. an unparseable spec is REFUSED (rc 2);
#   h. a missing README and a missing spec are REFUSED (rc 2);
#   i. an interpreter without PyYAML is REFUSED (rc 2), named — never a skip;
#   j. the REAL tracked pair (README.md + docs/openapi.yaml) is ACCEPTed;
#   k. NEUTER proof: a copy of this script whose verdict line is forced to
#      success ACCEPTs fixture (b), which the real arm rejects — so the
#      rejection is caused by this verdict and not by an unrelated crash.
# Exit: 0 every arm behaved, 1 at least one arm misbehaved, 2 a dependency is
# missing (no mktemp / no python3).

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PROG="check-readme-api-count"

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_refused() { printf 'REFUSED  %s: %s\n' "$PROG" "$*" >&2; exit 2; }

usage() {
  cat <<'EOF'
check-readme-api-count.sh — README API inventory drift gate (DF-CRIER-492)

Verifies EVERY "documented as N paths / M operations in `docs/openapi.yaml`"
sentence in README.md against a fresh parse of docs/openapi.yaml, counting
METHOD keys only (path-level `parameters`/`summary`/... are not operations).

Usage:
  bash scripts/check-readme-api-count.sh              check the tracked pair
  bash scripts/check-readme-api-count.sh --selftest   prove this checker
  bash scripts/check-readme-api-count.sh -h | --help  this text

Env:
  README_FILE         README to read        (default <repo>/README.md)
  SPEC_FILE           OpenAPI document      (default <repo>/docs/openapi.yaml)
  README_API_PYTHON   interpreter with PyYAML (default python3)

Exit codes:
  0  README matches the spec
  1  drift — counts disagree, sentence missing, or an occurrence unparseable
  2  refusal — missing/unreadable file, missing interpreter, no PyYAML,
     unparseable spec, or a spec with zero paths (never a PASS line)

The full contract — why the gate exists, what it measures, and every selftest
arm — is in the header comment of this file.
EOF
}

# spec_counts <interpreter> <spec-path> — prints "<paths> <operations>" on
# stdout. Exit 3 = the interpreter cannot import PyYAML, 4 = the document does
# not parse / declares no paths. Both are refusals at the caller.
spec_counts() {
  local py="$1" spec="$2"
  "$py" - "$spec" <<'PY'
import sys

METHODS = {"get", "put", "post", "delete", "options", "head", "patch", "trace"}


def refuse(msg, code):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


try:
    import yaml
except Exception as exc:  # ImportError, or a broken PyYAML install
    refuse(
        "PyYAML is not importable with %s (%s: %s); install it "
        "(python3 -m pip install pyyaml) or point README_API_PYTHON at an "
        "interpreter that has it" % (sys.executable, type(exc).__name__, exc),
        3,
    )

spec_path = sys.argv[1]
try:
    with open(spec_path, "r", encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)
except Exception as exc:
    refuse("cannot parse %s: %s: %s" % (spec_path, type(exc).__name__, exc), 4)

if not isinstance(doc, dict):
    refuse("%s does not parse to a mapping (got %s)" % (spec_path, type(doc).__name__), 4)

paths = doc.get("paths")
if paths is None:
    refuse("%s declares no 'paths' key" % spec_path, 4)
if not isinstance(paths, dict):
    refuse("%s: 'paths' is not a mapping (got %s)" % (spec_path, type(paths).__name__), 4)
if not paths:
    refuse("%s declares zero paths; an inventory check over it would be vacuous" % spec_path, 4)

operations = 0
for item in paths.values():
    if not isinstance(item, dict):
        continue
    for key in item:
        if str(key).lower() in METHODS:
            operations += 1

sys.stdout.write("%d %d\n" % (len(paths), operations))
PY
}

# check_one <readme> <spec> <interpreter> — the verdict. Prints PASS and exits
# 0 only when every occurrence in the README carries the spec's counts.
check_one() {
  local readme="$1" spec="$2" py="$3"

  [ -f "$readme" ] || _refused "README file not found: $readme"
  [ -r "$readme" ] || _refused "README file not readable: $readme"
  [ -f "$spec" ] || _refused "OpenAPI document not found: $spec"
  [ -r "$spec" ] || _refused "OpenAPI document not readable: $spec"
  command -v "$py" >/dev/null 2>&1 || _refused "python interpreter not found: $py (set README_API_PYTHON)"

  local counts rc=0
  counts="$(spec_counts "$py" "$spec")" || rc=$?
  if [ "$rc" -eq 3 ]; then
    _refused "the interpreter has no PyYAML, so $spec cannot be parsed (message above)"
  elif [ "$rc" -ne 0 ]; then
    _refused "$spec could not be parsed into a usable API inventory (message above)"
  fi

  local spec_paths spec_ops
  read -r spec_paths spec_ops <<<"$counts"
  if ! [[ "$spec_paths" =~ ^[0-9]+$ && "$spec_ops" =~ ^[0-9]+$ ]]; then
    _refused "unexpected parse output from $spec: '$counts'"
  fi

  # The sentence shape the inventory section publishes. Backticks are literal
  # here because the whole pattern is single-quoted.
  local pattern='documented as [0-9]+ paths / [0-9]+ operations in `docs/openapi\.yaml`'
  local matches
  matches="$(grep -oE "$pattern" "$readme" || true)"
  if [ -z "$matches" ]; then
    printf 'FAIL  %s: %s has no API inventory sentence of the form "documented as N paths / M operations in `docs/openapi.yaml`"\n' \
      "$PROG" "$readme" >&2
    printf 'FAIL  %s: the spec declares %s paths / %s operations — publish that sentence\n' \
      "$PROG" "$spec_paths" "$spec_ops" >&2
    return 1
  fi

  local occurrence=0 mismatch=0 sentence paths ops
  while IFS= read -r sentence; do
    [ -n "$sentence" ] || continue
    occurrence=$((occurrence + 1))
    paths="$(printf '%s' "$sentence" | sed -E 's/^documented as ([0-9]+) paths \/ ([0-9]+) operations.*/\1/')"
    ops="$(printf '%s' "$sentence" | sed -E 's/^documented as ([0-9]+) paths \/ ([0-9]+) operations.*/\2/')"
    if [ "$paths" != "$spec_paths" ] || [ "$ops" != "$spec_ops" ]; then
      printf 'FAIL  %s: %s occurrence %d says %s paths / %s operations, %s declares %s paths / %s operations\n' \
        "$PROG" "$readme" "$occurrence" "$paths" "$ops" "$spec" "$spec_paths" "$spec_ops" >&2
      mismatch=1
    fi
  done <<<"$matches"

  if [ "$mismatch" -ne 0 ]; then
    printf 'FAIL  %s: update %s to match %s (or fix the spec if the routes moved)\n' \
      "$PROG" "$readme" "$spec" >&2
    return 1
  fi

  printf 'PASS  %s: %s matches %s (%s paths / %s operations, %d sentence occurrence(s))\n' \
    "$PROG" "$readme" "$spec" "$spec_paths" "$spec_ops" "$occurrence"
  return 0
}

# ---------- selftest ----------

selftest() {
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/readme-api-count-selftest.XXXXXX")" || {
    _err "mktemp failed; cannot build fixtures"
    return 2
  }
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT HUP INT TERM

  local failures=0
  _arm() { printf 'selftest: %s\n' "$*"; }
  _expect_rc() { # <label> <expected> <actual>
    if [ "$2" = "$3" ]; then
      _arm "ok   $1 (rc $3)"
    else
      _arm "FAIL $1: expected rc $2, got rc $3"
      failures=$((failures + 1))
    fi
  }

  # Fixture A: 2 paths / 3 operations — the second path carries a path-level
  # `parameters:` element that is NOT an operation, so a naive element count
  # reads 4 and a method-keyed count reads 3.
  cat >"$tmp/spec.yaml" <<'EOF'
openapi: 3.0.3
info:
  title: fixture
  version: 0.0.0
paths:
  /alpha:
    get:
      responses: {}
    post:
      responses: {}
  /beta:
    parameters:
      - name: id
        in: query
    get:
      responses: {}
EOF

  cat >"$tmp/readme-good.md" <<'EOF'
# fixture
The surface is documented as 2 paths / 3 operations in `docs/openapi.yaml`.
EOF

  cat >"$tmp/readme-element-count.md" <<'EOF'
# fixture
The surface is documented as 2 paths / 4 operations in `docs/openapi.yaml`.
EOF

  cat >"$tmp/readme-wrong-paths.md" <<'EOF'
# fixture
The surface is documented as 1 paths / 3 operations in `docs/openapi.yaml`.
EOF

  cat >"$tmp/readme-no-sentence.md" <<'EOF'
# fixture
The API surface is documented in docs/openapi.yaml.
EOF

  cat >"$tmp/readme-two-sentences.md" <<'EOF'
# fixture
The surface is documented as 2 paths / 3 operations in `docs/openapi.yaml`.
A stale paragraph repeats: documented as 9 paths / 9 operations in `docs/openapi.yaml`.
EOF

  cat >"$tmp/spec-empty.yaml" <<'EOF'
openapi: 3.0.3
paths: {}
EOF

  cat >"$tmp/spec-broken.yaml" <<'EOF'
openapi: 3.0.3
paths: [this is not a mapping
EOF

  # a. the consistent pair is accepted, and the PASS line carries the counts
  local out rc
  out="$(README_FILE="$tmp/readme-good.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" 2>&1)" && rc=0 || rc=$?
  _expect_rc "consistent pair accepted" 0 "$rc"
  case "$out" in
    *"PASS"*"2 paths / 3 operations"*) _arm "ok   PASS line carries 2 paths / 3 operations" ;;
    *) _arm "FAIL PASS line does not carry the parsed counts: $out"; failures=$((failures + 1)) ;;
  esac

  # b. over-counting (a path-level `parameters:` counted as an operation)
  README_FILE="$tmp/readme-element-count.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "path-level parameters is not an operation (2/4 rejected)" 1 "$rc"

  # c. under-counted path count
  README_FILE="$tmp/readme-wrong-paths.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "wrong path count rejected (1/3)" 1 "$rc"

  # d. no inventory sentence at all — never a pass over nothing
  README_FILE="$tmp/readme-no-sentence.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "missing inventory sentence rejected" 1 "$rc"

  # e. a correct sentence plus a stale duplicate — every occurrence counts
  README_FILE="$tmp/readme-two-sentences.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "a second stale sentence is rejected" 1 "$rc"

  # f. zero paths is a refusal (vacuous inventory)
  README_FILE="$tmp/readme-good.md" SPEC_FILE="$tmp/spec-empty.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "spec with zero paths refused" 2 "$rc"

  # g. unparseable spec is a refusal
  README_FILE="$tmp/readme-good.md" SPEC_FILE="$tmp/spec-broken.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "unparseable spec refused" 2 "$rc"

  # h. missing files are refusals
  README_FILE="$tmp/does-not-exist.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "missing README refused" 2 "$rc"
  README_FILE="$tmp/readme-good.md" SPEC_FILE="$tmp/does-not-exist.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "missing spec refused" 2 "$rc"

  # i. an interpreter without PyYAML is a NAMED refusal, never a skip
  cat >"$tmp/python-no-yaml" <<'EOF'
#!/bin/sh
echo "PyYAML is not importable with /fixture/python (ImportError: No module named yaml)" >&2
exit 3
EOF
  chmod +x "$tmp/python-no-yaml"
  out="$(README_FILE="$tmp/readme-good.md" SPEC_FILE="$tmp/spec.yaml" README_API_PYTHON="$tmp/python-no-yaml" "$SELF" 2>&1)" && rc=0 || rc=$?
  _expect_rc "interpreter without PyYAML refused" 2 "$rc"
  case "$out" in
    *REFUSED*PyYAML*) _arm "ok   the refusal names PyYAML" ;;
    *) _arm "FAIL the PyYAML refusal is not named: $out"; failures=$((failures + 1)) ;;
  esac

  # j. the REAL tracked pair must be consistent (this is the gate's own claim)
  README_FILE="$REPO_ROOT/README.md" SPEC_FILE="$REPO_ROOT/docs/openapi.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
  _expect_rc "tracked README.md matches tracked docs/openapi.yaml" 0 "$rc"

  # k. NEUTER proof: force the verdict line to success in a copy. The copy must
  # ACCEPT the very fixture arm (b) rejects — otherwise the rejection in (b)
  # came from something other than this verdict.
  local neutered="$tmp/check-neutered.sh"
  sed 's/^  if \[ "\$mismatch" -ne 0 \]; then$/  if false; then/' "$SELF" >"$neutered"
  chmod +x "$neutered"
  if cmp -s "$SELF" "$neutered"; then
    _arm "FAIL neuter proof could not rewrite the verdict line (copy is identical)"
    failures=$((failures + 1))
  else
    README_FILE="$tmp/readme-element-count.md" SPEC_FILE="$tmp/spec.yaml" "$neutered" >/dev/null 2>&1 && rc=0 || rc=$?
    _expect_rc "NEUTER: forced-success copy ACCEPTS the drifted fixture" 0 "$rc"
    # and the real script still rejects it, so the pair is the proof
    README_FILE="$tmp/readme-element-count.md" SPEC_FILE="$tmp/spec.yaml" "$SELF" >/dev/null 2>&1 && rc=0 || rc=$?
    _expect_rc "NEUTER control: the real script still rejects it" 1 "$rc"
  fi

  if [ "$failures" -ne 0 ]; then
    printf 'SELFTEST FAIL  %s: %d arm(s) misbehaved\n' "$PROG" "$failures" >&2
    return 1
  fi
  printf 'SELFTEST PASS  %s: every arm behaved (drift, missing sentence, duplicate sentence, empty/broken/missing spec, missing PyYAML, real tree, NEUTER)\n' "$PROG"
  return 0
}

# ---------- entry ----------

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
  --selftest)
    command -v python3 >/dev/null 2>&1 || _refused "python3 is required to build the selftest's fixtures"
    selftest
    exit $?
    ;;
  "")
    check_one \
      "${README_FILE:-$REPO_ROOT/README.md}" \
      "${SPEC_FILE:-$REPO_ROOT/docs/openapi.yaml}" \
      "${README_API_PYTHON:-python3}"
    exit $?
    ;;
  *)
    _err "unknown argument: $1 (use --selftest, -h or --help)"
    exit 2
    ;;
esac
