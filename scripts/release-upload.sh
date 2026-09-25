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
#   CLOBBER=1          pass --clobber (re-upload over existing assets)
#   ALLOW_TAG_DRIFT=1  attach assets built from a commit other than the tag's
#   --notes-file <f>   use this release-notes file instead of the CHANGELOG section
#   --dry-run          verify everything and PRINT the gh commands, run none
#
# Exit status: 0 only when the asset set verified and the publish command ran (or
# was printed, under --dry-run).
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
Never pushes or moves a git tag — see docs/releases.md §3/§4.
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
EXISTS=0
if [ "$DRY_RUN" = "1" ]; then
  # The existence probe is a read; a dry run may run it, and prints which of the
  # two shapes it would take.
  if gh release view "$VERSION" --repo "$CRIER_REPO" >/dev/null 2>&1; then
    EXISTS=1
    note "Release object ${VERSION} exists — assets would be uploaded onto it"
  else
    note "no Release object ${VERSION} yet — it would be CREATED with these assets"
  fi
else
  if gh release view "$VERSION" --repo "$CRIER_REPO" >/dev/null 2>&1; then
    EXISTS=1
  fi
fi

# shellcheck disable=SC2086
set -- $ASSETS
if [ "$EXISTS" = "1" ]; then
  if [ "${CLOBBER:-0}" = "1" ]; then
    run gh release upload "$VERSION" "$@" --repo "$CRIER_REPO" --clobber
  else
    run gh release upload "$VERSION" "$@" --repo "$CRIER_REPO"
  fi
else
  if [ "${PRERELEASE:-0}" = "1" ]; then
    run gh release create "$VERSION" "$@" --repo "$CRIER_REPO" --title "$VERSION" --notes-file "$NOTES_FOR_UPLOAD" --prerelease
  else
    run gh release create "$VERSION" "$@" --repo "$CRIER_REPO" --title "$VERSION" --notes-file "$NOTES_FOR_UPLOAD"
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
