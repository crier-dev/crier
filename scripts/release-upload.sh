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
#   other content -> REFUSED unless CLOBBER=1 (a published binary is never
#                    silently replaced — docs/releases.md §4).
# Content identity is the only thing that converges, and it is always MEASURED: a
# missing remote digest is downloaded and hashed, never assumed equal.
#
# Exit status: 0 only when the asset set verified AND the Release then carries it —
# created, uploaded, or already converged (or printed, under --dry-run).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${VERSION:-}"
RELEASE_OUTDIR="${RELEASE_OUTDIR:-dist}"
CRIER_REPO="${CRIER_REPO:-crier-dev/crier}"
CRIER_TARGETS="${CRIER_TARGETS:-linux/amd64 linux/arm64 darwin/arm64}"
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
     PRERELEASE=1, CLOBBER=1, ALLOW_TAG_DRIFT=1.

Requires: gh (authenticated with write access to the repo), sha256sum/shasum.

Re-publishing an asset set that the Release already carries IDENTICAL content for
is a no-op (exit 0, nothing uploaded). Different content under an attached name is
refused unless CLOBBER=1. Never pushes or moves a git tag — see docs/releases.md
§3/§4.
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

CONVERGED=""   # names already attached with IDENTICAL content
DIFFERENT=""   # names attached with DIFFERENT content
TO_UPLOAD=""   # asset paths this publish still has to upload

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
      note "${name}: attached with DIFFERENT content (release sha256 ${rsha})"
      DIFFERENT="${DIFFERENT}${DIFFERENT:+ }${name}"
      TO_UPLOAD="${TO_UPLOAD}${TO_UPLOAD:+ }${asset}"
    fi
  done

  # DIVERGENCE-ARM: different content under a published name is a refusal, never a silent overwrite.
  if [ -n "$DIFFERENT" ] && [ "$CLOBBER" != "1" ]; then
    fail "${VERSION} already carries asset(s) whose content differs from this build: ${DIFFERENT} — a published asset is never replaced silently (somebody may already have downloaded it, and its SHA256SUMS entry would stop matching). Re-publish the build that produced them, or set CLOBBER=1 to replace them deliberately"
  fi
  if [ -n "$DIFFERENT" ] && [ "$CLOBBER" = "1" ]; then
    note "CLOBBER=1 — replacing deliberately: ${DIFFERENT}"
  fi
fi

# shellcheck disable=SC2086
if [ "$EXISTS" = "1" ] && [ -z "$TO_UPLOAD" ]; then
  CONVERGED_COUNT="$(printf '%s\n' $CONVERGED | grep -c . || true)"
  echo
  echo "release-upload: CONVERGED — ${VERSION} already carries all ${CONVERGED_COUNT} asset(s) with IDENTICAL content:"
  for name in $CONVERGED; do note "$name"; done
  if [ "$DRY_RUN" = "1" ]; then
    echo "release-upload: DRY RUN — a real run would upload nothing (it would converge, exit 0)."
  else
    echo "release-upload: nothing was uploaded — re-publishing an identical asset set is a no-op, not a name collision."
  fi
elif [ "$EXISTS" = "1" ]; then
  # shellcheck disable=SC2086
  TO_UPLOAD_COUNT="$(printf '%s\n' $TO_UPLOAD | grep -c . || true)"
  # shellcheck disable=SC2086
  CONVERGED_COUNT="$(printf '%s\n' $CONVERGED | grep -c . || true)"
  note "uploading ${TO_UPLOAD_COUNT} asset(s) onto the existing Release (${CONVERGED_COUNT} already attached with identical content, skipped):"
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
