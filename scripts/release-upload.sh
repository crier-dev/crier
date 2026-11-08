#!/usr/bin/env bash
#
# scripts/release-upload.sh — CR-FEAT-028: the release front door, PUBLISH half.
#
# WHY THIS EXISTS
# ---------------
# docs/releases.md §4 was a hand-written `gh release create … --notes-file
# /tmp/release-notes-<version>.md`: the tag went out, the Release object was
# created by hand, and NOTHING attached a binary. `gh release view v0.1.0-rc2`
# answered `assets: []` — the Release object carried notes only, so the only way
# into the project was `git clone` + a Go toolchain + `make build` (DISPATCH ·
# CRI-001, Carter, 2026-09-25). A hand step that can be forgotten was forgotten.
#
# This script is that step, as one command: it takes the asset set
# `make release-artifacts` built (or `make release` built on the very commit it
# tagged), verifies it, and attaches it to the GitHub Release object — creating
# the object from the version's own CHANGELOG section when it does not exist yet.
#
# WHAT IT NEVER DOES: it never pushes a git tag and never moves one. `make
# release` still refuses to push (`docs/releases.md` §2/§3 are unchanged), and a
# tag from a branch commit is still the caller's mistake to make by hand. This
# script touches the Release OBJECT only.
#
# Env / flags:
#   VERSION            the release tag (e.g. v0.1.0-rc3). Required.
#   RELEASE_OUTDIR     where the asset set lives (default: dist/)
#   CRIER_REPO         GitHub repo (default: crier-dev/crier)
#   CRIER_TARGETS      targets the asset set must carry (default: the three shipped)
#   PRERELEASE=1       mark the release a prerelease as it is created
#   CLOBBER=1          deliberately REPLACE an attached asset whose content differs
#   ALLOW_TAG_DRIFT=1  attach assets built from a commit other than the tag's
#   --notes-file <f>   use this release-notes file instead of the CHANGELOG section
#   --dry-run          verify everything and PRINT the gh commands, run none
#
# IDEMPOTENCE — re-publishing an IDENTICAL asset set is a NO-OP
# -------------------------------------------------------------
# GitHub refuses an upload whose name is already attached ("asset under the same
# name already exists"), so the pre-fix publisher could only ever run ONCE per
# tag. One release has TWO publishers — the documented manual `make
# release-upload` (docs/releases.md §4) and the tag-triggered
# .github/workflows/release.yml — so whichever ran second died on the collision,
# and that red run could never be repaired by re-running it.
# Measured: run 42246232196 (tag v0.3.0, e950fdb1) failed at
# "Publish the Release object with every asset" with
#   asset under the same name already exists: [crier_linux_amd64 … SHA256SUMS]
# while all 8 assets were attached and verified (RELEASE-012).
#
# So the publish step RECONCILES before it uploads. It reads the assets the
# Release already carries (name, sha256 digest, size) and compares each against
# the local file:
#   same content  -> skipped; a fully-identical set exits 0 reporting convergence
#                    (no upload is attempted, so there is no collision to fail on),
#   name absent   -> uploaded (a half-failed publish is REPAIRED, not re-done),
#   other content -> ADOPTED by BUILD IDENTITY when both sides provably carry the
#                    same build (see below) — the difference is the BUILD_TIME a
#                    rebuild always stamps in, so it is reported as expected
#                    metadata, never as a conflict, and nothing is uploaded;
#                    otherwise REFUSED unless CLOBBER=1 (a published asset is
#                    never silently replaced — docs/releases.md §4).
#
# BUILD IDENTITY — why a same-commit rebuild is ADOPTED, not refused
# ------------------------------------------------------------------
# Both publishers of one release build the SAME tagged commit: the hand `make
# release-upload` (docs/releases.md §4) and the tag-triggered release.yml. But
# release-artifacts.sh stamps the wall-clock BUILD_TIME into every binary, so
# the second publisher's build is never byte-identical to the first — and the
# byte comparison above correctly reports DIFFERENT for a set that is in every
# meaningful way the same build. That is what red-ed run 42422236749 (tag
# v0.4.0): the manual publish landed first, the workflow rebuilt with a later
# BUILD_TIME, and RELEASE-012 correctly refused to swap the published binaries.
# A release that shipped correctly wore a red run on its own tag.
#
# The remedy reads IDENTITY out of the artifact bytes, never out of the tag
# name. The obvious anchor — the -ldflags string, which older Go toolchains
# record verbatim in the binary's build settings — is NOT available on the
# toolchain this repo builds with: go1.26.10 records no -ldflags build setting
# at all (measured: `go version -m` lists none, and the byte string "ldflags"
# does not appear anywhere in a release binary), so the reader greps the
# stamped VALUES, not a flag blob. Measured shapes of a release binary's
# bytes (crier and crier-mcp, linux and darwin alike):
#   - the ${VERSION} token appears exactly once, standalone;
#   - the commit stamp is one exactly-40-character hex run — the ONLY one once
#     decimal-float mantissa runs are excluded (the server binaries carry
#     277555756156289135105907917022705078125e in their rodata: 39 digits and
#     an exponent marker, which length-40 hex matching alone cannot tell from
#     a commit);
#   - `vcs.modified=<true|false>` appears in the build settings of toolchains
#     that record VCS status (go1.26.10 does not; the check is kept for those
#     that do — a build from a modified tree must not be adopted).
#
# THE METADATA BOUND (INT-CI-034) — adoption is bounded, not unconditional
# ---------------------------------------------------------------------------
# The identity stamps above are a HEURISTIC, and on the toolchain this repo
# builds with they are the only one: go1.26.10 records NO vcs.revision and NO
# vcs.modified at all (measured), so the clean-tree gate is wired but VACUOUS
# there — a rebuild from a DIRTY tree at the same commit would carry the same
# Version token, the same single commit candidate and no `vcs.modified=true`.
# Adoption therefore also measures the byte divergence between the local asset
# and the attached copy (both are on disk by then) and tolerates only what
# build metadata explains. MEASURED spread of two builds of the SAME commit
# differing only in BUILD_TIME: size-identical, exactly 63 differing byte
# offsets on linux/amd64, 63 on linux/arm64 and 153 on darwin/arm64 (the
# Mach-O ad-hoc signature covers the stamped bytes). The bounds
# RELEASE_ADOPT_MAX_SIZE_DELTA (default 0 — the measured rebuild is
# size-identical) and RELEASE_ADOPT_MAX_DIFF_OFFSETS (default 4096 — >10x the
# largest measured spread) accept that with headroom; a body that diverges
# beyond them keeps the RELEASE-012 refusal, with the measured numbers quoted.
# The bound is a metadata-only heuristic, NOT a proof: a body crafted to
# differ within it would still be adopted. The honest claim is "same identity
# stamps AND a metadata-scale byte difference" — what a rebuild produces and
# what a different build does not.
#
# We READ BYTES and never execute the artifact: a downloaded release binary is
# compared, not run. A differing binary is ADOPTED (skipped: no upload, no
# replacement) only when BOTH sides carry the same Version token AND the same
# single commit candidate, and the ATTACHED side does not report
# `vcs.modified=true`, and the byte divergence stays within the metadata
# bound above. Anything else keeps the refusal, with the deciding fact
# named in the log: identity differs / identity unreadable or ambiguous (fail
# closed — adoption never rests on a missing field, and a binary whose commit
# stamp is ambiguous is refused, never guessed) / non-binary drift / divergence
# beyond the metadata bound. Two special
# members: SHA256SUMS is the manifest OF the set and is adopted only when every
# differing binary in the same run was adopted (it is the manifest of the
# adopted set; if any binary is refused, it is refused too); install.sh carries
# no stamp and must converge byte-identically as before.
#
# Content identity is the only thing that converges, and it is always MEASURED: a
# missing remote digest is downloaded and hashed, never assumed equal.
#
# Exit status: 0 only when the asset set verified AND the Release then carries it —
# created, uploaded, already converged, or converged with build-identity
# adoptions (or printed, under --dry-run).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${VERSION:-}"
RELEASE_OUTDIR="${RELEASE_OUTDIR:-dist}"
CRIER_REPO="${CRIER_REPO:-crier-dev/crier}"
CRIER_TARGETS="${CRIER_TARGETS:-linux/amd64 linux/arm64 darwin/arm64}"
# INT-CI-034 — the byte divergence BUILD-IDENTITY ADOPTION tolerates. A
# same-commit rebuild stamps a fresh BUILD_TIME and (on go1.26.10) nothing
# else, and what that buys is measured and SMALL: two builds of the SAME
# commit differing only in BUILD_TIME are size-identical and differ in exactly
# 63 byte offsets (linux/amd64), 63 (linux/arm64) and 153 (darwin/arm64 — the
# Mach-O ad-hoc signature covers the stamped bytes). The bound below must
# accept that spread with headroom and refuse a materially different body (a
# dirty-tree rebuild at the same commit): on go1.26.10 the toolchain records
# NO vcs.revision / vcs.modified at all, so the `vcs.modified=true` gate in
# identity_refusal_reason is wired but VACUOUS on this toolchain and this
# bound is the only discriminator there. Both knobs are env-tunable so a
# future toolchain that stamps more metadata can widen them deliberately,
# never silently.
#   RELEASE_ADOPT_MAX_SIZE_DELTA   default 0: the measured metadata-only
#                  rebuild is size-identical, so ANY size difference is
#                  outside the measured spread — the conservative choice.
#   RELEASE_ADOPT_MAX_DIFF_OFFSETS default 4096: >10x the largest measured
#                  spread (153), so the metadata-only rebuild passes with
#                  more than an order of magnitude of headroom while a body
#                  that differs beyond build metadata is caught.
RELEASE_ADOPT_MAX_SIZE_DELTA="${RELEASE_ADOPT_MAX_SIZE_DELTA:-0}"
RELEASE_ADOPT_MAX_DIFF_OFFSETS="${RELEASE_ADOPT_MAX_DIFF_OFFSETS:-4096}"
case "$RELEASE_ADOPT_MAX_SIZE_DELTA" in
  ''|*[!0-9]*) echo "release-upload: RELEASE_ADOPT_MAX_SIZE_DELTA must be a non-negative integer (got '$RELEASE_ADOPT_MAX_SIZE_DELTA')" >&2; exit 2 ;;
esac
case "$RELEASE_ADOPT_MAX_DIFF_OFFSETS" in
  ''|*[!0-9]*) echo "release-upload: RELEASE_ADOPT_MAX_DIFF_OFFSETS must be a non-negative integer (got '$RELEASE_ADOPT_MAX_DIFF_OFFSETS')" >&2; exit 2 ;;
esac
# DRY_RUN is settable as an env var too, because `make release-upload VERSION=vX
# DRY_RUN=1` passes the make variable through: an empty value (the Makefile's
# default) is "not a dry run", never an error.
DRY_RUN="${DRY_RUN:-0}"
case "$DRY_RUN" in
  0|"") DRY_RUN=0 ;;
  1) DRY_RUN=1 ;;
  *) echo "release-upload: DRY_RUN must be 0 or 1 (got '$DRY_RUN')" >&2; exit 2 ;;
esac
# CLOBBER is validated the same way: an empty value is "do not clobber", never an
# error, because `make release-upload CLOBBER=` would pass one through.
CLOBBER="${CLOBBER:-0}"
case "$CLOBBER" in
  0|"") CLOBBER=0 ;;
  1) CLOBBER=1 ;;
  *) echo "release-upload: CLOBBER must be 0 or 1 (got '$CLOBBER')" >&2; exit 2 ;;
esac
NOTES_FILE=""
CHANGELOG="CHANGELOG.md"

usage() {
  cat <<'EOF'
scripts/release-upload.sh — attach the built asset set to a GitHub Release (CR-FEAT-028).

Usage: VERSION=v0.1.0-rc3 bash scripts/release-upload.sh [--dry-run] [--notes-file <path>]

  --dry-run          verify the asset set and print the gh commands, run none
  --notes-file <p>   release notes to use instead of the CHANGELOG section

Env: VERSION (required), RELEASE_OUTDIR, CRIER_REPO, CRIER_TARGETS,
     PRERELEASE=1, CLOBBER=1, ALLOW_TAG_DRIFT=1,
     RELEASE_ADOPT_MAX_SIZE_DELTA, RELEASE_ADOPT_MAX_DIFF_OFFSETS.

Requires: gh (authenticated with write access to the repo), sha256sum/shasum, cmp.

Re-publishing an asset set that the Release already carries IDENTICAL content for
is a no-op (exit 0, nothing uploaded). A same-commit rebuild is ADOPTED by build
identity: binaries carrying the same embedded Version and Commit (read from the
artifact bytes; the attached tree must be clean) whose byte divergence stays
within the metadata bound (RELEASE_ADOPT_MAX_SIZE_DELTA /
RELEASE_ADOPT_MAX_DIFF_OFFSETS) differ only in the BUILD_TIME a rebuild stamps
in, so they are skipped and the difference is reported as expected metadata —
nothing is uploaded. Content that genuinely differs (another commit, another
tree, an unreadable identity, divergence beyond the metadata bound, a
non-binary like install.sh) is refused unless CLOBBER=1. Never pushes or moves
a git tag — see docs/releases.md §3/§4.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --notes-file) [ "${2:-}" ] || { echo "release-upload: --notes-file needs a value" >&2; exit 2; }; NOTES_FILE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "release-upload: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "release-upload: FAIL: $*" >&2; exit 1; }
note() { echo "    $*"; }
run() {
  if [ "$DRY_RUN" = "1" ]; then
    echo "    (dry-run) $*"
  else
    "$@"
  fi
}

# ── The tag ─────────────────────────────────────────────────────────────────
# The asset set is the release's front door, so the version is never inferred:
# a git-describe string ("v0.1.0-rc3-14-gabc1234") names a commit ahead of the
# tag, which is exactly the wrong thing to attach to a tag.
[ -n "$VERSION" ] || fail "VERSION is not set — pass the tag explicitly (example: VERSION=v0.1.0-rc3 make release-upload)"
case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "VERSION '$VERSION' is not a vX.Y.Z tag (example: v0.1.0-rc3)" ;;
esac
case "$VERSION" in
  *-[0-9]*-g[0-9a-f][0-9a-f]*) fail "VERSION '$VERSION' looks like git describe output, not a tag — attach assets to the tag, never to a commit description" ;;
esac

echo "==> crier release upload (CR-FEAT-028)"
echo "    version : ${VERSION}"
echo "    repo    : ${CRIER_REPO}"
echo "    assets  : ${RELEASE_OUTDIR}/"

if [ "$DRY_RUN" = "1" ]; then
  note "dry-run: nothing will be uploaded, no Release object will be created"
fi

# ── The tooling ─────────────────────────────────────────────────────────────
if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL="shasum -a 256"
else
  fail "neither 'sha256sum' nor 'shasum' is on PATH — the asset set cannot be verified before publishing"
fi
command -v gh >/dev/null 2>&1 || fail "'gh' is not on PATH — the Release object cannot be created or uploaded to"

# ── The asset set ───────────────────────────────────────────────────────────
echo
echo "==> asset set"
[ -d "$RELEASE_OUTDIR" ] || fail "${RELEASE_OUTDIR}/ does not exist — run 'make release-artifacts VERSION=${VERSION}' first (make release does it for the tag it cuts)"
[ -f "$RELEASE_OUTDIR/SHA256SUMS" ] || fail "${RELEASE_OUTDIR}/SHA256SUMS is missing — the asset set is incomplete (a release without a manifest cannot be verified by scripts/install.sh)"
(
  cd "$RELEASE_OUTDIR"
  # shellcheck disable=SC2086
  $SHA_TOOL -c --status SHA256SUMS || fail "the SHA256SUMS in ${RELEASE_OUTDIR}/ does not verify — refusing to publish an asset set that does not match its own manifest"
)
note "SHA256SUMS verifies ($(wc -l < "$RELEASE_OUTDIR/SHA256SUMS") file(s))"

ASSETS=""
missing=""
add_asset() {
  if [ -f "$RELEASE_OUTDIR/$1" ]; then
    ASSETS="${ASSETS}${ASSETS:+ }${RELEASE_OUTDIR}/$1"
  else
    missing="${missing} ${1}"
  fi
}
# EVERY target the release claims must be present: a tag published with the
# linux pair and no darwin artifact is a release that tells a Mac tester the
# front door is closed for them, and nothing else would notice.
for target in $CRIER_TARGETS; do
  os="${target%%/*}"
  arch="${target##*/}"
  add_asset "crier_${os}_${arch}"
  add_asset "crier-mcp_${os}_${arch}"
done
add_asset "install.sh"
[ -z "$missing" ] || fail "the asset set is missing:${missing} — rebuild it with 'make release-artifacts VERSION=${VERSION}' (every release target must ship, plus install.sh)"
add_asset "SHA256SUMS"

# ── The set must be the manifest, PLUS the manifest itself ──────────────────
# SHA256SUMS is the SET's own manifest, not a member of the set it covers:
# scripts/release-artifacts.sh writes it from the output directory and skips the
# file itself, because a manifest that hashed itself could never be verified
# (circular). So SHA256SUMS is the one asset this script publishes that the
# manifest cannot name — it rides IN the set, it is not covered BY it.
#
# Comparing the two sets without that exemption rejects a perfectly CORRECT set.
# Measured: .github/workflows/release.yml run 39539975561 (tag v0.2.0) exited 1
# on "Release v0.2.0 carries asset(s) SHA256SUMS does not know:SHA256SUMS" with
# all 8 assets attached and a fresh download verifying `sha256sum -c` cleanly —
# a red tag-CI on the one commit that must be green (RELEASE-010).
#
# This arm and the one in .github/workflows/release.yml are the same rule;
# `make release-upload-selftest` drives BOTH against the same fixtures, so they
# cannot drift apart.
MANIFEST="$RELEASE_OUTDIR/SHA256SUMS"
manifest_names="$(awk '{n=split($2, a, "/"); print a[n]}' "$MANIFEST" | LC_ALL=C sort -u)"
[ -n "$manifest_names" ] || fail "${MANIFEST} names no assets — a manifest that covers nothing verifies nothing, so the asset set cannot be checked against it"
covered=""
for asset in $ASSETS; do
  name="$(basename "$asset")"
  # THE EXEMPTION: the manifest rides in the set, it is not covered by it.
  [ "$name" = "SHA256SUMS" ] && continue
  covered="${covered}${covered:+$'\n'}${name}"
done
covered="$(printf '%s\n' "$covered" | LC_ALL=C sort -u)"
unknown="$(comm -13 <(printf '%s\n' "$manifest_names") <(printf '%s\n' "$covered") | sed '/^$/d')"
[ -z "$unknown" ] || fail "the asset set would publish asset(s) SHA256SUMS does not know:$(printf ' %s' $unknown) — SHA256SUMS is the set's own manifest and cannot name itself, so every OTHER asset must be covered by it; rebuild the set with 'make release-artifacts VERSION=${VERSION}'"
uncarried="$(comm -23 <(printf '%s\n' "$manifest_names") <(printf '%s\n' "$covered") | sed '/^$/d')"
[ -z "$uncarried" ] || fail "SHA256SUMS names asset(s) this upload does not carry:$(printf ' %s' $uncarried) — the published manifest would point a downloader at files that are not attached"
note "set matches SHA256SUMS: $(printf '%s\n' "$manifest_names" | wc -l) covered asset(s) + SHA256SUMS itself"

for asset in $ASSETS; do
  note "$(printf '%-34s' "$(basename "$asset")") $(wc -c < "$asset" | awk '{printf "%.1f MB", $1/1048576}')"
done

# ── Provenance: the assets must belong to the tag ───────────────────────────
echo
echo "==> provenance"
if git rev-parse -q --verify "refs/tags/${VERSION}" >/dev/null 2>&1; then
  TAG_COMMIT="$(git rev-parse "refs/tags/${VERSION}^{commit}")"
  HEAD_COMMIT="$(git rev-parse HEAD)"
  note "tag ${VERSION} -> $(git rev-parse --short "refs/tags/${VERSION}^{commit}")"
  if [ "$TAG_COMMIT" != "$HEAD_COMMIT" ] && [ "${ALLOW_TAG_DRIFT:-0}" != "1" ]; then
    fail "these assets were built from HEAD ($(git rev-parse --short HEAD)), but ${VERSION} names $(git rev-parse --short "$TAG_COMMIT") — a release artifact must be the tag's own build. Check out the tag and rebuild ('git checkout ${VERSION} && make release-artifacts VERSION=${VERSION}'), or set ALLOW_TAG_DRIFT=1 if you know why they differ"
  fi
  note "assets were built from the tagged commit"
else
  # Not fatal: the publish step can legitimately run from a fresh clone that has
  # not fetched tags. It is still the loudest warning this script prints, because
  # it is the one case where the assets cannot be tied to a commit here.
  note "WARNING: no local tag ${VERSION} — the assets cannot be tied to the tag's commit from this clone (run 'git fetch --tags'); the Release object is created by TAG NAME either way"
fi
if [ -n "$(git status --porcelain -- "$RELEASE_OUTDIR" 2>/dev/null || true)" ]; then
  note "WARNING: ${RELEASE_OUTDIR}/ has uncommitted changes tracked by git — that directory is supposed to be gitignored build output"
fi

# ── Release notes ───────────────────────────────────────────────────────────
echo
echo "==> release notes"
NOTES_TMP=""
NOTES_FOR_UPLOAD=""
if [ -n "$NOTES_FILE" ]; then
  [ -f "$NOTES_FILE" ] || fail "--notes-file ${NOTES_FILE} does not exist"
  note "using ${NOTES_FILE}"
else
  # docs/releases.md §1: the version's own CHANGELOG section, plus a compare link.
  SECTION="$(awk -v v="${VERSION#v}" '
    $0 ~ "^## \\[" v "\\]" { inside = 1; print; next }
    inside && /^## \[/ { exit }
    inside { print }
  ' "$CHANGELOG")"
  if [ -z "$SECTION" ]; then
    fail "${CHANGELOG} has no '## [${VERSION#v}]' section — write the changelog entry first (docs/releases.md §1), or pass --notes-file <path>"
  fi
  PREV="$(git tag --list 'v*' --sort=-v:refname | grep -vx "$VERSION" | head -1 || true)"
  NOTES_TMP="$(mktemp)"
  {
    printf '%s\n' "$SECTION"
    if [ -n "$PREV" ]; then
      printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' "$CRIER_REPO" "$PREV" "$VERSION"
    fi
  } > "$NOTES_TMP"
  note "notes extracted from ${CHANGELOG} (section [${VERSION#v}])$( [ -n "$PREV" ] && printf ', compare link vs %s' "$PREV" )"
  note "$(wc -l < "$NOTES_TMP") line(s) -> ${NOTES_TMP}"
fi
[ -z "$NOTES_TMP" ] || NOTES_FOR_UPLOAD="$NOTES_TMP"
[ -n "$NOTES_FOR_UPLOAD" ] || NOTES_FOR_UPLOAD="$NOTES_FILE"
note "NOTE: a release marked --prerelease is EXCLUDED from /releases/latest, which is the URL scripts/install.sh resolves for a default (unpinned) install — mark an rc prerelease only if you mean to close that default path."

# ── Publish ─────────────────────────────────────────────────────────────────
echo
echo "==> publish"

# Reading the current state of the Release is a READ, so a dry run performs it
# too: which of the create / upload / converge shapes this publish takes is a
# property of the Release, not of the flag.
EXISTS=0
if gh release view "$VERSION" --repo "$CRIER_REPO" >/dev/null 2>&1; then
  EXISTS=1
fi

WORK_TMP="$(mktemp -d)"
trap 'rm -rf "$WORK_TMP"' EXIT

local_sha() {
  # shellcheck disable=SC2086
  $SHA_TOOL "$1" | awk '{print $1}' | tr '[:upper:]' '[:lower:]'
}

CONVERGED=""         # names already attached with IDENTICAL content
ADOPTED=""           # names attached with DIFFERENT content but the SAME build identity (adopted: skipped, never uploaded)
ADOPTED_BINARIES=""  # the build-stamped subset of ADOPTED (gates the SHA256SUMS adoption)
DIFFERENT=""         # names attached with DIFFERENT content, refused
DIFFERENT_REASONS="" # one "<name>: <fact>" line per refused asset, printed with the refusal
MANIFEST_DIFFERENT=0 # SHA256SUMS differs — its decision waits for the binaries (manifest arm)
TO_UPLOAD=""         # asset paths this publish still has to upload

# ── Build identity, read from the artifact bytes ────────────────────────────
# The release binaries are stamped at link time: `-ldflags "-X
# github.com/crier-dev/crier/internal/buildinfo.Version=<v> …Commit=<40-hex sha>
# …BuildTime=<RFC3339>"`. go1.26.10 does NOT record that flag string in the
# binary (see the header comment), so the reader greps the stamped VALUE
# shapes out of the file bytes — the same bytes on linux/amd64, linux/arm64
# and darwin/arm64, and the same shapes the selftest's synthetic fixtures
# carry. We READ BYTES and never execute the artifact: a downloaded release
# binary is compared, not run.
RE_IDENT_VCSMOD='vcs\.modified=(true|false)'

identity_field() { # <file> <ERE> -> the value after the first '=' of the first match (empty when absent)
  local hit=""
  hit="$(grep -aom1 -E -- "$2" "$1" 2>/dev/null | head -n1 || true)"
  printf '%s' "${hit#*=}"
}

# identity_of <file> -> sets I_COMMIT / I_COMMIT_CANDIDATES / I_VERSION_STAMPED /
# I_VCSMOD for the file's bytes.
#
# Commit stamp: the exactly-40-character hex runs of the file (grep the MAXIMAL
# runs, then keep length==40, so a commit is never carved out of a longer hex
# blob), minus decimal-float mantissa runs (measured: go1.26.10's server
# rodata carries 277555756156289135105907917022705078125e — digits and an
# exponent marker — which length-40 hex matching alone cannot tell from a
# commit). Exactly one survivor = the commit stamp; zero or several = unreadable
# / ambiguous, and the caller refuses (fail closed: an ambiguous stamp is never
# guessed).
identity_of() {
  local file="$1" runs="" c=""
  I_COMMIT=""
  I_COMMIT_CANDIDATES=0
  runs="$(grep -aoE '[0-9a-f]{40,}' "$file" 2>/dev/null | LC_ALL=C sort -u || true)"
  for c in $runs; do
    [ "${#c}" -eq 40 ] || continue
    case "$c" in
      *[!0-9e]*) ;;     # carries a hex letter beyond digits/e -> a commit shape
      *) continue ;;    # only digits (+ trailing e): a decimal-float mantissa
    esac
    I_COMMIT_CANDIDATES=$((I_COMMIT_CANDIDATES + 1))
    I_COMMIT="$c"
  done
  # Version stamp: the publisher's own VERSION token, standalone in the bytes
  # (bounded by non-alphanumerics). Measured unique in every binary of the set;
  # a rebuild of the same tag carries the same token.
  I_VERSION_STAMPED=0
  if [ -n "${VERSION:-}" ] && grep -aqE "(^|[^0-9A-Za-z])${VERSION//./\\.}([^0-9A-Za-z]|$)" "$file" 2>/dev/null; then
    I_VERSION_STAMPED=1
  fi
  # VCS status: recorded only by toolchains that stamp build settings (go1.26.10
  # does not). When present, vcs.modified=true means the bytes are NOT the
  # commit they name — the caller refuses.
  I_VCSMOD="$(identity_field "$file" "$RE_IDENT_VCSMOD")"
}

# byte_divergence <local-file> <attached-file>
# Sets D_SIZE_DELTA (|local size - attached size| in bytes) and D_DIFF_OFFSETS
# (the count of differing byte offsets `cmp -l` reports — one record per
# differing byte). MEASURED, never assumed: cmp's nonzero exit when the files
# differ is expected, and the record count is the datum — the exit status is
# not the gate. Used ONLY on the build-identity adoption path, after
# identity_refusal_reason has proven both copies carry the same Version+Commit
# identity and both are on disk; a metadata-only rebuild's ~63-153 differing
# offsets over ~24 MB cost one cmp well under a second (no per-byte shell
# loop).
byte_divergence() { # <local-file> <attached-file>
  local lsize asize records
  lsize="$(wc -c < "$1" | tr -d '[:space:]')"
  asize="$(wc -c < "$2" | tr -d '[:space:]')"
  D_SIZE_DELTA=$(( lsize > asize ? lsize - asize : asize - lsize ))
  records="$(cmp -l "$1" "$2" 2>/dev/null | wc -l | tr -d '[:space:]' || true)"
  D_DIFF_OFFSETS="${records:-0}"
}

is_release_binary() { # <name> — the build-stamped members of the set; anything else (install.sh) carries no stamp
  case "$1" in
    crier_*|crier-mcp_*) return 0 ;;
    *) return 1 ;;
  esac
}

# identity_refusal_reason <local-asset-path> <attached-name>
# Sets ADOPT_REASON: EMPTY when the differing asset may be ADOPTED by build
# identity, otherwise a one-line fact explaining the refusal. Also leaves the
# I_LOCAL_* / I_ATTACH_* fields set for the caller's report. Fail closed: an
# unreadable or ambiguous stamp (no commit candidate, several, or the VERSION
# token absent), a Version/Commit mismatch, an attached `vcs.modified=true`,
# and any non-binary asset -> a non-empty reason (DIFFERENT, refused). Adoption
# never rests on a missing or multi-candidate field.
identity_refusal_reason() {
  local asset="$1" name="$2" attached_copy=""
  ADOPT_REASON=""
  D_SIZE_DELTA="" D_DIFF_OFFSETS=""
  I_LOCAL_COMMIT="" I_LOCAL_COMMIT_CANDIDATES=0 I_LOCAL_VERSION_STAMPED=0
  I_ATTACH_COMMIT="" I_ATTACH_COMMIT_CANDIDATES=0 I_ATTACH_VERSION_STAMPED=0 I_ATTACH_VCSMOD=""
  if ! is_release_binary "$name"; then
    ADOPT_REASON="carries no build stamp (non-binary asset) — real content drift; a published non-binary asset is never replaced silently"
    return 0
  fi
  # Both sides must read as: exactly one commit candidate, and this publish's
  # own VERSION token present. Read the LOCAL side first, so a local fixture or
  # stale build that carries no identity refuses before anything is downloaded.
  identity_of "$asset"
  I_LOCAL_COMMIT="$I_COMMIT" I_LOCAL_COMMIT_CANDIDATES="$I_COMMIT_CANDIDATES" \
    I_LOCAL_VERSION_STAMPED="$I_VERSION_STAMPED"
  if [ "$I_LOCAL_COMMIT_CANDIDATES" -ne 1 ]; then
    ADOPT_REASON="build identity unreadable in the LOCAL ${name} (commit stamp: ${I_LOCAL_COMMIT_CANDIDATES} exactly-40-hex candidate(s) after the mantissa filter, want exactly 1) — refusing on an unreadable identity"
    return 0
  fi
  if [ "$I_LOCAL_VERSION_STAMPED" -ne 1 ]; then
    ADOPT_REASON="build identity unreadable in the LOCAL ${name} (the VERSION token ${VERSION} is not in the bytes) — refusing on an unreadable identity"
    return 0
  fi
  # The attached bytes: reuse the copy the digest-less compare already fetched,
  # otherwise download them now. A digest on the asset proves its content hash,
  # not its build identity — the stamp has to be read from the bytes.
  mkdir -p "$WORK_TMP/download/identity"
  attached_copy="$WORK_TMP/download/identity/$name"
  if [ -f "$WORK_TMP/download/$name" ]; then
    cp "$WORK_TMP/download/$name" "$attached_copy"
  elif ! gh release download "$VERSION" --repo "$CRIER_REPO" --pattern "$name" --dir "$WORK_TMP/download/identity" >/dev/null 2>&1; then
    ADOPT_REASON="the attached ${name} could not be fetched to read its build identity (gh release download failed) — refusing on an unreadable identity"
    return 0
  fi
  if [ ! -s "$attached_copy" ]; then
    ADOPT_REASON="the attached ${name} fetched for the identity compare is empty or truncated — refusing on an unreadable identity"
    return 0
  fi
  identity_of "$attached_copy"
  I_ATTACH_COMMIT="$I_COMMIT" I_ATTACH_COMMIT_CANDIDATES="$I_COMMIT_CANDIDATES" \
    I_ATTACH_VERSION_STAMPED="$I_VERSION_STAMPED" I_ATTACH_VCSMOD="$I_VCSMOD"
  if [ "$I_ATTACH_COMMIT_CANDIDATES" -ne 1 ]; then
    ADOPT_REASON="build identity unreadable in the ATTACHED ${name} (commit stamp: ${I_ATTACH_COMMIT_CANDIDATES} exactly-40-hex candidate(s) after the mantissa filter, want exactly 1) — refusing on an unreadable identity"
    return 0
  fi
  if [ "$I_ATTACH_VERSION_STAMPED" -ne 1 ]; then
    ADOPT_REASON="build identity unreadable in the ATTACHED ${name} (the VERSION token ${VERSION} is not in the bytes) — refusing on an unreadable identity"
    return 0
  fi
  if [ "$I_ATTACH_VCSMOD" = "true" ]; then
    ADOPT_REASON="the attached ${name} reports vcs.modified=true — it was built from a modified working tree, so the commit it names does not describe its bytes"
    return 0
  fi
  if [ "$I_ATTACH_COMMIT" != "$I_LOCAL_COMMIT" ]; then
    ADOPT_REASON="build identity differs — the attached ${name} was built from commit ${I_ATTACH_COMMIT}, this build from commit ${I_LOCAL_COMMIT}"
    return 0
  fi
  # ── The metadata bound (INT-CI-034) ────────────────────────────────────────
  # Every identity ground above is answered, but on go1.26.10 they are not
  # enough: the toolchain stamps no vcs.revision / vcs.modified, so the
  # clean-tree gate above is VACUOUS on it and a rebuild from a DIRTY tree at
  # the same commit would pass every check so far. Measure the remaining byte
  # difference — both copies are on disk — and adopt only within the metadata
  # bound (the knobs and their measured derivation are declared at the top of
  # this script): anything beyond it is a different body wearing a copied
  # identity stamp, and keeps the RELEASE-012 refusal with the measured
  # numbers quoted.
  byte_divergence "$asset" "$attached_copy"
  # ADOPT-BOUND-ARM:
  if [ "$D_SIZE_DELTA" -gt "$RELEASE_ADOPT_MAX_SIZE_DELTA" ] || [ "$D_DIFF_OFFSETS" -gt "$RELEASE_ADOPT_MAX_DIFF_OFFSETS" ]; then
    ADOPT_REASON="same build identity but the bytes diverge beyond the metadata bound (size delta ${D_SIZE_DELTA} byte(s), ${D_DIFF_OFFSETS} differing byte offset(s); bound: size <= ${RELEASE_ADOPT_MAX_SIZE_DELTA}, offsets <= ${RELEASE_ADOPT_MAX_DIFF_OFFSETS}) — a rebuild from a modified tree is not the published build"
    return 0
  fi
  # Every refusal ground is answered: same single commit candidate on both
  # sides, this publish's VERSION token in both, the attached tree was clean
  # (or the toolchain stamps no VCS status), and the remaining byte difference
  # is within the metadata bound — the build metadata (BUILD_TIME) a rebuild
  # always stamps in. -> adoptable, with the divergence measured above.
  return 0
}

if [ "$EXISTS" = "1" ]; then
  REMOTE_TSV="$WORK_TMP/attached.tsv"
  # ONE read, through the REST endpoint rather than `gh release view --json
  # assets`: the release-asset `digest` field is what makes content identity
  # cheap, and older gh builds do not project it from --json (gh 2.46 answers
  # `null` for every asset while the same API response carries `sha256:…`).
  gh api "repos/${CRIER_REPO}/releases/tags/${VERSION}" \
    --jq '.assets[] | [.name, (.digest // ""), (.size | tostring)] | @tsv' > "$REMOTE_TSV" \
    || fail "could not read the assets already attached to ${VERSION} in ${CRIER_REPO} — refusing to guess what is published (gh api failed)"
  note "Release object ${VERSION} exists — reconciling the set against what is attached ($(wc -l < "$REMOTE_TSV" | tr -d '[:space:]') asset(s))"

  for asset in $ASSETS; do
    name="$(basename "$asset")"
    lsha="$(local_sha "$asset")"
    lsize="$(wc -c < "$asset" | tr -d '[:space:]')"
    rline="$(awk -F'\t' -v n="$name" '$1 == n { print; exit }' "$REMOTE_TSV")"
    if [ -z "$rline" ]; then
      note "${name}: not attached — it will be uploaded"
      TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${asset}"
      continue
    fi
    rsha="$(printf '%s\n' "$rline" | cut -f2)"
    rsize="$(printf '%s\n' "$rline" | cut -f3)"
    # GitHub reports a digest as `sha256:<hex>`; compare against the bare hex.
    case "$rsha" in
      sha256:*) rsha="${rsha#sha256:}" ;;
    esac
    rsha="$(printf '%s' "$rsha" | tr '[:upper:]' '[:lower:]')"
    if [ -z "$rsha" ]; then
      # No digest on the attached asset (published before GitHub recorded one, or
      # projected away by the client). A size mismatch already proves the content
      # differs; equal sizes prove nothing, so the asset is DOWNLOADED and hashed.
      # "Identical" is measured here, never assumed from a missing field.
      if [ "$rsize" != "$lsize" ]; then
        note "${name}: attached with a different size (${rsize} vs ${lsize}) — DIFFERENT"
        DIFFERENT_REASONS="${DIFFERENT_REASONS}${DIFFERENT_REASONS:+$'\n'}${name}: size differs (attached ${rsize} vs local ${lsize} bytes)"
        DIFFERENT="${DIFFERENT}${DIFFERENT:+ }${name}"
        TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${asset}"
        continue
      fi
      mkdir -p "$WORK_TMP/download"
      gh release download "$VERSION" --repo "$CRIER_REPO" --pattern "$name" --dir "$WORK_TMP/download" >/dev/null 2>&1 \
        || fail "could not download the attached ${name} from ${VERSION} in ${CRIER_REPO} to compare its content — refusing to publish blind (gh release download failed)"
      rsha="$(local_sha "$WORK_TMP/download/$name")"
      note "${name}: no digest on the Release — downloaded and hashed to compare"
    fi
    # CONVERGENCE-ARM: same content under the same name is already published.
    if [ "$rsha" = "$lsha" ]; then
      note "${name}: attached with identical content — skipped"
      CONVERGED="${CONVERGED}${CONVERGED:+ }${name}"
    else
      # MANIFEST DEFERRAL: SHA256SUMS is the manifest OF the set, so its own
      # content always changes when any covered binary's bytes change. Its
      # decision waits until every binary in this run is classified — see the
      # manifest arm below. (Under CLOBBER=1 it simply rides with the rest of
      # the replaced set, exactly as before the identity rule existed.)
      if [ "$name" = "SHA256SUMS" ]; then
        note "${name}: attached with DIFFERENT content — decision deferred until every binary in this run is classified (it is the set's manifest)"
        MANIFEST_DIFFERENT=1
      else
        note "${name}: attached with DIFFERENT content (release sha256 ${rsha})"
        # A differing binary gets one second chance before the refusal: classify
        # WHY the content differs — when BOTH sides provably carry the same
        # build identity, the difference is the BUILD_TIME metadata a rebuild
        # always stamps in. The decision itself is the IDENTITY-ARM below.
        identity_refusal_reason "$asset" "$name"   # sets ADOPT_REASON ("" = adoptable) + I_LOCAL_*/I_ATTACH_*
        # IDENTITY-ARM:
        if [ -z "$ADOPT_REASON" ]; then
          # Empty reason = a same-build rebuild: same VERSION token, same
          # single commit candidate, no attached vcs.modified=true, and the
          # measured byte divergence within the metadata bound (INT-CI-034).
          # The remaining byte difference is the BUILD_TIME metadata a rebuild
          # always stamps in, so the asset is ADOPTED — skipped, nothing
          # uploaded, the difference reported as expected metadata WITH its
          # measured numbers (a converged run carries its own evidence), never
          # as a conflict. Anything else keeps the RELEASE-012 refusal.
          # Neutering this decision (selftest arm [11/15]) re-creates the
          # historical refusal on a same-commit rebuild (run 42422236749, tag
          # v0.4.0).
          note "${name}: ADOPTED by build identity — both sides carry Version=${VERSION} Commit=${I_LOCAL_COMMIT} (the remaining byte difference is build metadata — BUILD_TIME — expected on a rebuild; measured divergence: size delta ${D_SIZE_DELTA} byte(s), ${D_DIFF_OFFSETS} differing byte offset(s), within the metadata bound size <= ${RELEASE_ADOPT_MAX_SIZE_DELTA}, offsets <= ${RELEASE_ADOPT_MAX_DIFF_OFFSETS}); nothing is uploaded"
          ADOPTED="${ADOPTED}${ADOPTED:+ }${name}"
          if is_release_binary "$name"; then
            ADOPTED_BINARIES="${ADOPTED_BINARIES}${ADOPTED_BINARIES:+ }${name}"
          fi
        else
          ADOPT_REASON="${ADOPT_REASON:-identity adoption was not evaluated}"
          note "${name}: REFUSED — ${ADOPT_REASON}"
          DIFFERENT_REASONS="${DIFFERENT_REASONS}${DIFFERENT_REASONS:+$'\n'}${name}: ${ADOPT_REASON}"
          DIFFERENT="${DIFFERENT}${DIFFERENT:+ }${name}"
          TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${asset}"
        fi
      fi
    fi
  done

  # MANIFEST-ARM: the deferred SHA256SUMS decision. It may be ADOPTED only when
  # every differing binary in this run was adopted by identity — its hashes
  # changed because those builds' bytes changed, so it IS the manifest of the
  # adopted set. If any binary was refused, the manifest is refused with them
  # (a manifest pointing at a set that was never published). Under CLOBBER=1 it
  # is replaced with the rest of the differing set, exactly as before. And a
  # manifest that differs while NOTHING else does (and no binary was adopted)
  # describes an internally inconsistent Release — refused, fail closed.
  if [ "$MANIFEST_DIFFERENT" = "1" ]; then
    if [ "$CLOBBER" = "1" ]; then
      note "SHA256SUMS: CLOBBER=1 — replacing the manifest with the rest of the differing set"
      DIFFERENT="${DIFFERENT}${DIFFERENT:+ }SHA256SUMS"
      TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${RELEASE_OUTDIR}/SHA256SUMS"
    elif [ -z "$DIFFERENT" ] && [ -n "$ADOPTED_BINARIES" ]; then
      note "SHA256SUMS: adopted — it is the manifest of the adopted binaries (every differing binary in this run was adopted by build identity); nothing is uploaded"
      ADOPTED="${ADOPTED}${ADOPTED:+ }SHA256SUMS"
    else
      if [ -n "$DIFFERENT" ]; then
        MANIFEST_REASON="it is the manifest of a REFUSED set — its hashes describe binaries whose identity was refused above"
      else
        MANIFEST_REASON="it differs while every asset it covers is byte-identical and no binary was adopted — the attached manifest is inconsistent with its own set"
      fi
      note "SHA256SUMS: REFUSED — ${MANIFEST_REASON}"
      DIFFERENT_REASONS="${DIFFERENT_REASONS}${DIFFERENT_REASONS:+$'\n'}SHA256SUMS: ${MANIFEST_REASON}"
      DIFFERENT="${DIFFERENT}${DIFFERENT:+ }SHA256SUMS"
      TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${RELEASE_OUTDIR}/SHA256SUMS"
    fi
  fi

  # DIVERGENCE-ARM:
  if [ -n "$DIFFERENT" ] && [ "$CLOBBER" != "1" ]; then
    # Different content under a published name is a refusal, never a silent
    # overwrite (RELEASE-012). Same-commit rebuilds whose binaries carry the
    # same Version+Commit were already ADOPTED above, so a refusal here means
    # the content genuinely differs — and the reasons above say which fact made
    # each name a refusal, so the next red run is diagnosable from the log alone.
    if [ -n "$DIFFERENT_REASONS" ]; then
      printf '%s\n' "$DIFFERENT_REASONS" | sed 's/^/release-upload:     - /' >&2
    fi
    fail "${VERSION} already carries asset(s) whose content differs from this build: ${DIFFERENT} — a published asset is never replaced silently (somebody may already have downloaded it, and its SHA256SUMS entry would stop matching). A same-commit rebuild is ADOPTED by build identity, so this refusal means the identity genuinely differs (see the reasons above). Re-publish the build that produced them, or set CLOBBER=1 to replace them deliberately"
  fi
  if [ -n "$DIFFERENT" ] && [ "$CLOBBER" = "1" ]; then
    note "CLOBBER=1 — replacing deliberately: ${DIFFERENT}"
  fi
fi

# shellcheck disable=SC2086
if [ "$EXISTS" = "1" ] && [ -z "$TO_UPLOAD" ]; then
  CONVERGED_COUNT="$(printf '%s\n' $CONVERGED | grep -c . || true)"
  # shellcheck disable=SC2086
  ADOPTED_COUNT="$(printf '%s\n' $ADOPTED | grep -c . || true)"
  echo
  if [ -n "$ADOPTED" ]; then
    echo "release-upload: CONVERGED — ${VERSION} already carries the full set: ${CONVERGED_COUNT} asset(s) with IDENTICAL content + ${ADOPTED_COUNT} adopted by build identity (BUILD_TIME differs — expected on a rebuild):"
    for name in $CONVERGED; do note "${name}: attached with identical content"; done
    for name in $ADOPTED; do note "${name}: adopted by build identity — same Version+Commit, metadata (BUILD_TIME) differs, nothing uploaded"; done
    if [ "$DRY_RUN" = "1" ]; then
      echo "release-upload: DRY RUN — a real run would upload nothing (it would converge with the adoptions above, exit 0)."
    else
      echo "release-upload: nothing was uploaded — a same-commit rebuild is adopted by build identity, not treated as a conflict."
    fi
  else
    echo "release-upload: CONVERGED — ${VERSION} already carries all ${CONVERGED_COUNT} asset(s) with IDENTICAL content:"
    for name in $CONVERGED; do note "$name"; done
    if [ "$DRY_RUN" = "1" ]; then
      echo "release-upload: DRY RUN — a real run would upload nothing (it would converge, exit 0)."
    else
      echo "release-upload: nothing was uploaded — re-publishing an identical asset set is a no-op, not a name collision."
    fi
  fi
elif [ "$EXISTS" = "1" ]; then
  # shellcheck disable=SC2086
  TO_UPLOAD_COUNT="$(printf '%s\n' $TO_UPLOAD | grep -c . || true)"
  # shellcheck disable=SC2086
  CONVERGED_COUNT="$(printf '%s\n' $CONVERGED | grep -c . || true)"
  # shellcheck disable=SC2086
  ADOPTED_COUNT="$(printf '%s\n' $ADOPTED | grep -c . || true)"
  note "uploading ${TO_UPLOAD_COUNT} asset(s) onto the existing Release (${CONVERGED_COUNT} already attached with identical content, ${ADOPTED_COUNT} adopted by build identity, skipped):"
  for asset in $TO_UPLOAD; do note "  $(basename "$asset")"; done
  # shellcheck disable=SC2086
  if [ "$CLOBBER" = "1" ]; then
    run gh release upload "$VERSION" $TO_UPLOAD --repo "$CRIER_REPO" --clobber
  else
    run gh release upload "$VERSION" $TO_UPLOAD --repo "$CRIER_REPO"
  fi
else
  if [ "$DRY_RUN" = "1" ]; then
    note "no Release object ${VERSION} yet — it would be CREATED with these assets"
  fi
  # shellcheck disable=SC2086
  if [ "${PRERELEASE:-0}" = "1" ]; then
    run gh release create "$VERSION" $ASSETS --repo "$CRIER_REPO" --title "$VERSION" --notes-file "$NOTES_FOR_UPLOAD" --prerelease
  else
    run gh release create "$VERSION" $ASSETS --repo "$CRIER_REPO" --title "$VERSION" --notes-file "$NOTES_FOR_UPLOAD"
  fi
fi

[ -z "$NOTES_TMP" ] || rm -f "$NOTES_TMP"
echo
if [ "$DRY_RUN" = "1" ]; then
  echo "release-upload: DRY RUN — nothing was published. Re-run without --dry-run once CI is green on the tagged commit."
else
  echo "release-upload: verify what landed —"
  echo "    gh release view ${VERSION} --repo ${CRIER_REPO} --json assets --jq '.assets[].name'"
  echo "release-upload: the front door is then live:"
  echo "    curl -fsSL https://raw.githubusercontent.com/${CRIER_REPO}/main/scripts/install.sh | sh"
fi
