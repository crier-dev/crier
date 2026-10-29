#!/usr/bin/env bash
#
# scripts/release-upload-selftest.sh — RELEASE-010: the tag publisher's asset-set
# check accepts a CORRECT release and still rejects an UNKNOWN asset.
#
# WHY THIS EXISTS
# ---------------
# .github/workflows/release.yml publishes the asset set, then verifies it against
# dist/SHA256SUMS. Both sides of that comparison are legitimate: SHA256SUMS is
# uploaded WITH the assets (a release without a manifest cannot be verified by
# scripts/install.sh), and the manifest never names itself (a manifest that
# hashed itself could never be verified — scripts/release-artifacts.sh writes it
# from the output directory and skips the file itself).
#
# So a set-comparison that demands EQUALITY rejects a perfectly correct release.
# Measured: run 39539975561 (tag v0.2.0, commit c0d3142) exited 1 on
#   ::error::Release v0.2.0 carries asset(s) SHA256SUMS does not know:SHA256SUMS
# with all 8 assets attached and a fresh download verifying `sha256sum -c`
# cleanly. That is a red tag-CI on the one commit that must be green, and it
# would red every future cut. The rule the check must implement is:
#
#   the set must carry every asset named by SHA256SUMS (MISSING is fatal),
#   it may — and does — also carry SHA256SUMS itself (exempt from UNKNOWN),
#   and any OTHER asset the manifest does not name is genuinely unknown (fatal).
#
# WHAT THIS PROVES, with no network, no docker, no keys and no published release:
#
#   [1/5] scripts/release-upload.sh ACCEPTS a correct fixture — every required
#         release asset for every target, install.sh, and SHA256SUMS — exiting 0
#         and printing the set/manifest match. (`--dry-run`, so nothing is
#         published; `gh` is a PATH shim, so no network and no auth.)
#   [2/5] scripts/release-upload.sh REJECTS a fixture whose set carries an asset
#         SHA256SUMS does not know (a manifest written before that asset was
#         added): nonzero exit, FAIL naming the asset.
#   [3/5] the WORKFLOW's own verification block — extracted from
#         .github/workflows/release.yml and executed with `bash -e`, exactly as
#         Actions runs it, against a `gh` shim reporting the attached names —
#         accepts a Release carrying every manifest asset plus SHA256SUMS, and
#         rejects BOTH a Release carrying an unknown asset and a Release missing
#         a manifest asset. This is the arm that actually red-ed v0.2.0, so it is
#         proven by execution, not by reading the YAML.
#   [4/5] NEUTER proof: the same extracted block with the SHA256SUMS exemption
#         removed REJECTS the correct fixture, reproducing the historical error
#         text. The exemption is therefore load-bearing, not decoration — a green
#         [3/5] cannot be vacuous.
#   [5/5] `bash -n` on both scripts and a YAML parse of the workflow, so the
#         fixtures above are driving scripts that are syntactically sound.
#
# The two implementations are the same rule in two places (a workflow cannot
# source a shell function); this drive runs BOTH against the same fixtures, which
# is what keeps them from drifting apart.
#
# Requirements: bash, awk, sort, comm, sed, grep, wc, python3 (to extract the
# workflow step — stdlib only), sha256sum/shasum.
#
# Env:
#   RELEASE_UPLOAD_SELFTEST_VERSION  version tag the fixture set is published as
#                                    (default v0.0.0-uploadtest)
#   RELEASE_UPLOAD_SELFTEST_TARGETS  release targets the fixture carries
#                                    (default: the three the release ships)
#
# Exit status: 0 only when every arm passed. Fixtures live under one mktemp -d
# that is removed on exit; nothing is backgrounded and nothing is published.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
WORKFLOW="$REPO_ROOT/.github/workflows/release.yml"

SELFTEST_VERSION="${RELEASE_UPLOAD_SELFTEST_VERSION:-v0.0.0-uploadtest}"
SELFTEST_TARGETS="${RELEASE_UPLOAD_SELFTEST_TARGETS:-linux/amd64 linux/arm64 darwin/arm64}"

WORKDIR="$(mktemp -d)"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
indent() { sed 's/^/    | /'; }

if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL="shasum -a 256"
else
  fail "neither 'sha256sum' nor 'shasum' is on PATH — the fixtures cannot carry a real manifest"
fi
command -v python3 >/dev/null 2>&1 || fail "'python3' is required to extract the verification step out of ${WORKFLOW}"
[ -f "$WORKFLOW" ] || fail "${WORKFLOW} is missing — the workflow arm has nothing to drive"

echo "==> release-upload-selftest (RELEASE-010)"
echo "    version : ${SELFTEST_VERSION}"
echo "    targets : ${SELFTEST_TARGETS}"
echo "    workflow: ${WORKFLOW#"$REPO_ROOT"/}"

# ── The gh shim ─────────────────────────────────────────────────────────────
# The publish half is a `gh` call. Both arms below drive it against a PATH shim,
# so nothing here needs a network, a token, or a published Release. `release
# view` reports the asset names in GH_ASSETS_FILE when it is set (the shape the
# workflow's verification step consumes) and "no such release" when it is not
# (the shape release-upload.sh's create-vs-upload probe wants).
GH_SHIM_DIR="$WORKDIR/gh-path"
GH_LOG="$GH_SHIM_DIR/gh-invocations.log"
mkdir -p "$GH_SHIM_DIR"
cat > "$GH_SHIM_DIR/gh" <<'SHIM'
#!/bin/sh
# RELEASE-010 test double: records every invocation, and answers `release view`
# with the asset names the fixture says GitHub reports. No network, no auth.
log="$(dirname "$0")/gh-invocations.log"
printf 'gh %s\n' "$*" >> "$log"
case "$*" in
  *"release view"*)
    if [ -n "${GH_ASSETS_FILE:-}" ] && [ -f "${GH_ASSETS_FILE}" ]; then
      cat "$GH_ASSETS_FILE"
      exit 0
    fi
    exit 1
    ;;
esac
exit 1
SHIM
chmod 0755 "$GH_SHIM_DIR/gh"

# ── Fixtures ────────────────────────────────────────────────────────────────
# A fixture is the real shape of dist/: crier + crier-mcp for every release
# target, install.sh, and a SHA256SUMS that covers all of them and NOT itself —
# exactly what scripts/release-artifacts.sh writes. `omit` drops one asset from
# the MANIFEST while leaving the file in the set: the "genuinely unknown asset"
# shape (a manifest written before that asset existed).
make_fixture() { # <dir> [asset the manifest must not know]
  local dir="$1" omit="${2:-}" name target os arch names=""
  mkdir -p "$dir"
  for target in $SELFTEST_TARGETS; do
    os="${target%%/*}"
    arch="${target##*/}"
    names="${names} crier_${os}_${arch} crier-mcp_${os}_${arch}"
  done
  names="${names} install.sh"
  for name in $names; do
    printf 'release-upload-selftest fixture payload: %s\n' "$name" > "$dir/$name"
  done
  (
    cd "$dir"
    for name in *; do
      [ -f "$name" ] || continue
      # The manifest never covers itself — the same rule release-artifacts.sh
      # applies, and the reason the comparison needs an exemption.
      [ "$name" = "SHA256SUMS" ] && continue
      [ -n "$omit" ] && [ "$name" = "$omit" ] && continue
      # shellcheck disable=SC2086
      printf '%s  %s\n' "$($SHA_TOOL "$name" | awk '{print $1}')" "$name"
    done | LC_ALL=C sort > SHA256SUMS
  )
  (
    cd "$dir"
    # shellcheck disable=SC2086
    $SHA_TOOL -c --status SHA256SUMS || fail "the fixture manifest in ${dir} does not verify — the fixture itself is broken"
  )
}

# The names GitHub would report for a Release built from <dir>, plus SHA256SUMS
# (uploaded with the set) and any extra names a negative arm wants carried.
write_assets_file() { # <out-file> <dist-dir> [extra name…]
  local out="$1" dist="$2"
  shift 2
  awk '{n=split($2, a, "/"); print a[n]}' "$dist/SHA256SUMS" > "$out"
  printf 'SHA256SUMS\n' >> "$out"
  local extra
  for extra in "$@"; do printf '%s\n' "$extra" >> "$out"; done
  LC_ALL=C sort -u -o "$out" "$out"
}

# The last target's server asset is the one the "unknown" fixtures leave out of
# the manifest, so the omitted name always belongs to the shipped set.
LAST_TARGET="${SELFTEST_TARGETS##* }"
OMITTED_ASSET="crier_${LAST_TARGET%%/*}_${LAST_TARGET##*/}"

CORRECT="$WORKDIR/fixture-correct"
UNKNOWN="$WORKDIR/fixture-unknown-in-set"
make_fixture "$CORRECT"
make_fixture "$UNKNOWN" "$OMITTED_ASSET"
printf 'RELEASE-010 selftest release notes\n' > "$WORKDIR/notes.md"

# ── [1/5] release-upload.sh accepts the correct set ────────────────────────
echo
echo "==> [1/5] release-upload.sh accepts a CORRECT set (every manifest asset + SHA256SUMS)"
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$CORRECT" \
  ALLOW_TAG_DRIFT=1 bash "$SCRIPT_DIR/release-upload.sh" --dry-run --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh REJECTED a correct asset set (exit ${RC}) — this is the v0.2.0 defect"
printf '%s\n' "$OUT" | grep -q 'set matches SHA256SUMS' \
  || fail "release-upload.sh exited 0 but never reported the set/manifest match — the arm is not proven"
printf '%s\n' "$OUT" | grep -q "carries asset(s) SHA256SUMS does not know" \
  && fail "release-upload.sh still calls SHA256SUMS an asset its own manifest does not know"
CORRECT_COUNT="$(awk '{n=split($2, a, "/"); print a[n]}' "$CORRECT/SHA256SUMS" | wc -l)"
echo "    PASS: correct set accepted — ${CORRECT_COUNT} manifest asset(s) + SHA256SUMS itself (exit 0)"

# ── [2/5] release-upload.sh rejects a genuinely unknown asset ──────────────
echo
echo "==> [2/5] release-upload.sh rejects an asset SHA256SUMS does not know (${OMITTED_ASSET})"
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$UNKNOWN" \
  ALLOW_TAG_DRIFT=1 bash "$SCRIPT_DIR/release-upload.sh" --dry-run --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ACCEPTED a set carrying '${OMITTED_ASSET}', which SHA256SUMS does not know (exit 0)"
printf '%s\n' "$OUT" | grep -q 'FAIL' || fail "release-upload.sh exited ${RC} without printing FAIL"
printf '%s\n' "$OUT" | grep -q "${OMITTED_ASSET}" || fail "release-upload.sh refused the set but never named the unknown asset '${OMITTED_ASSET}'"
echo "    FAIL (expected): the unknown asset '${OMITTED_ASSET}' was refused by name — exit ${RC}"
echo "    PASS: a genuinely unknown asset is still fatal"

# ── [3/5] the WORKFLOW's verification block, executed ──────────────────────
# The defect lived in the workflow step, so the workflow step is what gets
# driven. It is extracted verbatim from the YAML and run with `bash -e`, the way
# Actions runs a `run:` block, against a dist/ tree and a gh shim.
cat > "$WORKDIR/extract.py" <<'PY'
import sys, re

src, out, mode = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(src, encoding="utf-8").read().split("\n")

start = None
for i, line in enumerate(lines):
    if line.lstrip().startswith("- name:") and "Verify the Release carries" in line:
        start = i
        break
if start is None:
    sys.exit('release.yml: no step named "Verify the Release carries …" — the workflow arm has nothing to drive')

body = None
for i in range(start + 1, len(lines)):
    if re.match(r"^\s*run:\s*\|", lines[i]):
        body = i + 1
        break
if body is None:
    sys.exit("release.yml: the verification step has no `run: |` block")

block, indent = [], None
for line in lines[body:]:
    if line.strip() == "":
        block.append("")
        continue
    cur = len(line) - len(line.lstrip(" "))
    if indent is None:
        indent = cur
    if cur < indent:
        break
    block.append(line[indent:])
text = "\n".join(block).rstrip("\n") + "\n"

if mode == "neuter":
    # Remove the SHA256SUMS exemption: the comparison then demands equality
    # between the attached set and the manifest, which is the shape that red-ed
    # v0.2.0. If this neutered copy still accepts the correct fixture, the
    # exemption is not load-bearing and the green arm proves nothing.
    kept, neutered = [], False
    for line in text.split("\n"):
        if "grep -vx" in line and "SHA256SUMS" in line:
            kept.append('attached="$(printf \'%s\\n\' "${attached_all}")"')
            neutered = True
        else:
            kept.append(line)
    if not neutered:
        sys.exit("neuter proof: the SHA256SUMS exemption line was not found — the proof would be vacuous")
    text = "\n".join(kept) + "\n"

open(out, "w", encoding="utf-8").write(text)
PY

VERIFY_BLOCK="$WORKDIR/verify-release-assets.sh"
NEUTERED_BLOCK="$WORKDIR/verify-release-assets-neutered.sh"
python3 "$WORKDIR/extract.py" "$WORKFLOW" "$VERIFY_BLOCK" keep
python3 "$WORKDIR/extract.py" "$WORKFLOW" "$NEUTERED_BLOCK" neuter
echo
echo "==> [3/5] the workflow's own verification block, executed (bash -e, gh is a shim)"
echo "    extracted $(wc -l < "$VERIFY_BLOCK") line(s) from $(basename "$WORKFLOW")"

# The block reads `dist/SHA256SUMS`, so each arm runs from a work dir whose dist/
# is the fixture — the same layout the workflow's checkout has.
run_workflow_block() { # <block> <assets-file> ; sets WF_OUT, returns block exit
  local script="$1" assets="$2" rc=0
  set +e
  WF_OUT="$(cd "$WORKDIR/wf" && \
    VERSION="$SELFTEST_VERSION" CRIER_REPO="crier-dev/crier" GH_TOKEN="selftest-token" \
    GH_ASSETS_FILE="$assets" PATH="$GH_SHIM_DIR:$PATH" bash -e "$script" 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "$WF_OUT" | indent
  return "$rc"
}

mkdir -p "$WORKDIR/wf"
ln -sfn "$CORRECT" "$WORKDIR/wf/dist"

# 3a — a Release carrying the whole manifest PLUS SHA256SUMS is CORRECT.
write_assets_file "$WORKDIR/attached-correct.txt" "$CORRECT"
if ! run_workflow_block "$VERIFY_BLOCK" "$WORKDIR/attached-correct.txt"; then
  fail "the workflow's verification block REJECTED a correct Release (every manifest asset + SHA256SUMS) — the exact v0.2.0 failure"
fi
printf '%s\n' "$WF_OUT" | grep -q 'carries every manifest asset' \
  || fail "the workflow block exited 0 without reporting the manifest match — the arm is not proven"
echo "    PASS: workflow accepted a Release carrying every manifest asset + SHA256SUMS"

# 3b — an asset the manifest does not know is still FATAL.
write_assets_file "$WORKDIR/attached-unknown.txt" "$CORRECT" "release-notes.txt"
if run_workflow_block "$VERIFY_BLOCK" "$WORKDIR/attached-unknown.txt"; then
  fail "the workflow's verification block ACCEPTED a Release carrying 'release-notes.txt', which SHA256SUMS does not know"
fi
printf '%s\n' "$WF_OUT" | grep -q 'release-notes.txt' || fail "the workflow block refused the Release but never named the unknown asset"
echo "    FAIL (expected): an unknown attached asset was refused by name"
echo "    PASS: a genuinely unknown asset is still fatal in the workflow arm"

# 3c — a Release MISSING a manifest asset is still FATAL.
write_assets_file "$WORKDIR/attached-partial.txt" "$CORRECT"
grep -vx "$OMITTED_ASSET" "$WORKDIR/attached-partial.txt" > "$WORKDIR/attached-partial.tmp"
mv "$WORKDIR/attached-partial.tmp" "$WORKDIR/attached-partial.txt"
if run_workflow_block "$VERIFY_BLOCK" "$WORKDIR/attached-partial.txt"; then
  fail "the workflow's verification block ACCEPTED a Release missing '${OMITTED_ASSET}' from the manifest set"
fi
printf '%s\n' "$WF_OUT" | grep -q 'is missing manifest asset' || fail "the workflow block refused the Release but never reported the missing asset"
echo "    FAIL (expected): a missing manifest asset was refused by name"
echo "    PASS: a partial release is still fatal in the workflow arm"

# ── [4/5] NEUTER proof ─────────────────────────────────────────────────────
echo
echo "==> [4/5] NEUTER proof: without the SHA256SUMS exemption the correct fixture reds"
if run_workflow_block "$NEUTERED_BLOCK" "$WORKDIR/attached-correct.txt"; then
  fail "the NEUTERED workflow block (exemption removed) ACCEPTED the correct Release — the exemption is not load-bearing and the green above is vacuous"
fi
printf '%s\n' "$WF_OUT" | grep -q 'carries asset(s) SHA256SUMS does not know:SHA256SUMS' \
  || fail "the neutered block failed, but not with the historical SHA256SUMS error — the reproduction does not match the v0.2.0 red"
echo "    FAIL (expected): the neutered block reproduced the v0.2.0 error verbatim"
echo "    PASS: the SHA256SUMS exemption is load-bearing"

# ── [5/5] the scripts under test are syntactically sound ───────────────────
echo
echo "==> [5/5] syntax: bash -n on both scripts, YAML parse of the workflow"
bash -n "$SCRIPT_DIR/release-upload.sh" || fail "bash -n rejected scripts/release-upload.sh"
bash -n "$SCRIPT_DIR/release-upload-selftest.sh" || fail "bash -n rejected scripts/release-upload-selftest.sh"
python3 - "$WORKFLOW" <<'PY'
import sys
try:
    import yaml
except ImportError:
    yaml = None
path = sys.argv[1]
raw = open(path, encoding="utf-8").read()
if yaml is None:
    print("    (PyYAML not installed — skipping the optional YAML parse, bash -n already ran)")
else:
    doc = yaml.safe_load(raw)
    assert isinstance(doc, dict) and "jobs" in doc, f"{path} did not parse to a workflow document"
    print(f"    (PyYAML parse clean: {len(doc['jobs'])} job(s))")
PY
echo "    PASS: both scripts parse and the workflow is well-formed"

echo
echo "release-upload-selftest: PASS — a correct asset set (every manifest asset + SHA256SUMS)"
echo "                         is accepted by BOTH the publish script and the workflow's"
echo "                         self-check, a genuinely unknown asset is still refused by name,"
echo "                         and the SHA256SUMS exemption is proven load-bearing."
