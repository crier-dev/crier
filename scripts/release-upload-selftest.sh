#!/usr/bin/env bash
#
# scripts/release-upload-selftest.sh — the tag publisher's proof.
#
# WHY THIS EXISTS
# ---------------
# Two independent rules decide whether a tag publish is correct, and both are
# proven here by EXECUTION (no network, no keys, nothing published):
#
# (A) RELEASE-010 — the asset-set check. GitHub's Release carries the asset set
#     and the workflow verifies it against dist/SHA256SUMS. Both sides of that
#     comparison are legitimate: SHA256SUMS is uploaded WITH the assets (a release
#     without a manifest cannot be verified by scripts/install.sh), and the
#     manifest never names itself (a manifest that hashed itself could never be
#     verified — scripts/release-artifacts.sh writes it from the output directory
#     and skips the file itself).
#     So a set-comparison that demands EQUALITY rejects a perfectly correct
#     release. Measured: run 39539975561 (tag v0.2.0, commit c0d3142) exited 1 on
#       ::error::Release v0.2.0 carries asset(s) SHA256SUMS does not know:SHA256SUMS
#     with all 8 assets attached and a fresh download verifying `sha256sum -c`
#     cleanly. The rule the check must implement is:
#       the set must carry every asset named by SHA256SUMS (MISSING is fatal),
#       it may — and does — also carry SHA256SUMS itself (exempt from UNKNOWN),
#       and any OTHER asset the manifest does not name is unknown (fatal).
#
# (B) RELEASE-012 — the publish is IDEMPOTENT. GitHub refuses an upload whose name
#     is already attached ("asset under the same name already exists"), so the
#     pre-fix publisher could only ever run once per tag: whichever of the two
#     publishers ran second (the documented manual `make release-upload`, or the
#     tag-triggered release.yml) died on the collision, and the red run could
#     never be repaired by re-running it. Measured: run 42246232196 (tag v0.3.0,
#     e950fdb1) failed exactly that way with all 8 assets attached and verified.
#     The rule the publish must implement is:
#       identical content under the same name -> SKIPPED (a whole identical set
#                                                exits 0 reporting convergence),
#       an absent name                        -> uploaded (repairs a half-publish),
#       different content under that name     -> REFUSED unless CLOBBER=1.
#
# (C) INT-CI-033 — a same-commit REBUILD is ADOPTED by build identity, not
#     refused on byte drift. The two publishers of one release both build the
#     tagged commit, and release-artifacts.sh stamps the wall-clock BUILD_TIME
#     into every binary, so the second publisher's build is never byte-identical
#     to the first: run 42422236749 (tag v0.4.0) red-ed with every asset
#     "attached with DIFFERENT content" — a release that had shipped correctly
#     wore a red run on its own tag. The rule the publish must implement on top
#     of (B) is:
#       a differing BINARY whose bytes carry the same identity as the local
#       build — the same ${VERSION} token AND the same single 40-hex commit
#       candidate, with no `vcs.modified=true` on the attached side — is
#       ADOPTED: skipped, nothing uploaded, the difference reported as the
#       BUILD_TIME metadata a rebuild always stamps in;
#       SHA256SUMS is adopted only as the manifest of a fully-adopted set;
#       install.sh carries no stamp and must still converge byte-identically;
#       anything else (identity differs, identity unreadable/ambiguous,
#       non-binary drift) keeps the (B) refusal, with the deciding fact named.
#     The identity is READ FROM THE BYTES (never executed, never assumed from
#     the tag name): go1.26.10 records no -ldflags build setting at all, so
#     the reader greps the stamped VALUE shapes — the ${VERSION} token and the
#     exactly-40-hex commit run — which is exactly what the stamped fixtures
#     below carry. A NEUTER proof removes the adoption decision and shows the
#     same fixture red-ing again.
#
# (D) INT-CI-034 — the adoption is BOUNDED by a metadata-only byte-divergence
#     bound. The (C) identity stamps cannot do the whole job on the toolchain
#     crier builds with: go1.26.10 records no vcs.revision / vcs.modified at
#     all, so the clean-tree gate is vacuous there and a rebuild from a DIRTY
#     tree at the same commit carries the same Version token, the same single
#     commit candidate and no vcs.modified=true. What separates a rebuild from
#     a different body is SCALE: two builds of the SAME commit differing only
#     in BUILD_TIME measure size-identical with exactly 63 (linux/amd64), 63
#     (linux/arm64) and 153 (darwin/arm64) differing byte offsets — a
#     materially different body diverges far beyond that. The publisher
#     therefore measures the divergence on the adoption path and refuses
#     beyond RELEASE_ADOPT_MAX_SIZE_DELTA (default 0) /
#     RELEASE_ADOPT_MAX_DIFF_OFFSETS (default 4096), quoting the measured
#     numbers in the refusal; the bound is a metadata-only heuristic, not a
#     proof, and is documented as such.
#
# WHAT THIS PROVES
# ----------------
#   [1/15] scripts/release-upload.sh ACCEPTS a correct fixture — every required
#         release asset for every target, install.sh, and SHA256SUMS — exiting 0
#         and printing the set/manifest match. (`--dry-run`, so nothing is
#         published; `gh` is a PATH shim, so no network and no auth.)
#   [2/15] scripts/release-upload.sh REJECTS a fixture whose set carries an asset
#         SHA256SUMS does not know (a manifest written before that asset was
#         added): nonzero exit, FAIL naming the asset.
#   [3/15] the WORKFLOW's own verification block — extracted from
#         .github/workflows/release.yml and executed with `bash -e`, exactly as
#         Actions runs it, against a `gh` shim reporting the attached names —
#         accepts a Release carrying every manifest asset plus SHA256SUMS, and
#         rejects BOTH a Release carrying an unknown asset and a Release missing
#         a manifest asset. This is the arm that actually red-ed v0.2.0, so it is
#         proven by execution, not by reading the YAML.
#   [4/15] NEUTER proof (RELEASE-010): the same extracted block with the SHA256SUMS
#         exemption removed REJECTS the correct fixture, reproducing the
#         historical error text. The exemption is therefore load-bearing.
#   [5/15] CONVERGENCE (RELEASE-012): against a Release that already carries the
#         EXACT asset set, the publisher exits 0, says it converged, and issues
#         NO upload at all (the shim log is empty of `release upload`) — the
#         v0.3.0 failure cannot happen. Proven twice: with the assets carrying
#         GitHub digests, and with NO digests, where the publisher must download
#         each attached asset and hash it rather than assume "equal".
#   [6/15] DIVERGENCE (RELEASE-012): the same names with DIFFERENT content still
#         exit nonzero by name, with no upload attempted; with CLOBBER=1 the run
#         exits 0 and uploads ONLY the differing assets, replacing them
#         deliberately (the identical ones are still skipped, and the double's
#         stored digest changes). These fixtures carry NO build stamp, so the
#         differing binary is identity-UNREADABLE — refused, the fail-closed
#         default.
#   [7/15] REPAIR (RELEASE-012): a Release carrying only part of the set (a publish
#         that half-failed) converges the rest — exit 0, ONE upload carrying
#         exactly the missing assets and no `--clobber`, so no release is left
#         half-published and nothing is replaced.
#   [8/15] NEUTER proof (RELEASE-012): a copy of the publisher with the convergence
#         and divergence arms removed (the pre-fix shape) fails [5/15]'s fixture
#         again with the historical `asset under the same name already exists`.
#         The convergence is therefore load-bearing, not decoration.
#   [9/15] IDENTITY ADOPTION (INT-CI-033): against a Release whose binaries were
#         built from the SAME Version+Commit but stamped with a LATER BUILD_TIME,
#         the publisher exits 0, reports the adoption (BUILD_TIME named as
#         expected metadata, never as a conflict), downloads the attached bytes
#         to read the identity — and issues ZERO uploads: the published set is
#         kept exactly as it is. SHA256SUMS (the manifest of the adopted set) is
#         adopted with the binaries.
#   [10/15] IDENTITY REFUSAL (RELEASE-012 kept, INT-CI-033's AC3): the same
#         fixture shape with the attached binaries carrying a DIFFERENT commit
#         exits nonzero naming the asset and the two commits; a fixture whose
#         binaries report `vcs.modified=true` is refused too. No upload is
#         attempted in either shape.
#   [11/15] NEUTER proof (INT-CI-033): a copy of the publisher with ONLY the
#         adoption decision removed reds [9/15]'s same-commit-rebuild fixture
#         with the RELEASE-012 refusal — the adoption arm is load-bearing, not
#         decoration.
#   [12/15] NON-BINARY DRIFT: install.sh differs between the two builds (it
#         carries no stamp and cannot be adopted) — the run refuses, naming
#         install.sh AND the SHA256SUMS that covers it, with no upload.
#   [13/15] `bash -n` on the scripts and a YAML parse of the workflow, so the
#         fixtures above are driving scripts that are syntactically sound.
#   [14/15] METADATA BOUND (INT-CI-034): a fixture whose binaries carry the
#         SAME Version token and the SAME single 40-hex commit but a
#         materially different BODY (a 16 KiB filler run that differs between
#         the two builds, plus an appended line — thousands of differing byte
#         offsets and a real size delta, far beyond the measured metadata-only
#         spread) is REFUSED as a different build: the refusal quotes the
#         MEASURED size delta and differing-offset count. The metadata-only
#         [9/15] adoption arm still passes on a BUILD_TIME-only diff.
#   [15/15] NEUTER proof (INT-CI-034): a copy of the publisher with ONLY the
#         metadata bound removed ADOPTS [14/15]'s material-divergence fixture
#         — the bound is what separates that refusal from the [9/15]
#         adoption, so it is load-bearing.
#
# The two implementations of rule (A) are the same rule in two places (a workflow
# cannot source a shell function); this drive runs BOTH against the same fixtures,
# which is what keeps them from drifting apart.
#
# Requirements: bash, awk, sort, comm, sed, grep, wc, cp, head, tr, cmp,
# python3 (to extract the workflow step and to build the neutered copies —
# stdlib only), sha256sum/shasum.
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
command -v python3 >/dev/null 2>&1 || fail "'python3' is required to extract the verification step and build the neutered copies"
[ -f "$WORKFLOW" ] || fail "${WORKFLOW} is missing — the workflow arm has nothing to drive"

echo "==> release-upload-selftest (RELEASE-010 + RELEASE-012)"
echo "    version : ${SELFTEST_VERSION}"
echo "    targets : ${SELFTEST_TARGETS}"
echo "    workflow: ${WORKFLOW#"$REPO_ROOT"/}"

# ── The gh double ───────────────────────────────────────────────────────────
# Both halves under test are `gh` callers, so neither needs a network, a token or
# a published Release: the shim below is a PATH `gh` that MODELS a Release object.
#
#   GH_STATE_DIR — directory holding the simulated Release:
#                    release.tsv   one line per attached asset,
#                                  <name>\t<sha256:…|empty>\t<size>
#                                  (absent file = the Release does not exist)
#                    files/<name>  the attached bytes, served to
#                                  `gh release download`
#   GH_ASSETS_FILE — optional pre-rendered name list that `gh release view`
#                    prints, which is the shape the workflow's verification step
#                    consumes.
#
# `gh release upload` reproduces GitHub's OWN behaviour, including the error the
# v0.3.0 run died on: a name that is already attached is refused with
# `asset under the same name already exists: [...]` unless --clobber is passed.
# A double that quietly accepted a duplicate upload would make arm [8/15] unable
# to fail, i.e. it would make the neuter proof vacuous.
GH_SHIM_DIR="$WORKDIR/gh-path"
GH_LOG="$GH_SHIM_DIR/gh-invocations.log"
SHIM_STATE="$WORKDIR/release-state"
mkdir -p "$GH_SHIM_DIR"
cat > "$GH_SHIM_DIR/gh" <<'SHIM'
#!/bin/sh
# RELEASE-010 / RELEASE-012 test double: records every invocation and models a
# Release object (GH_STATE_DIR) closely enough that a duplicate upload fails the
# way GitHub fails it. No network, no auth.
log="$(dirname "$0")/gh-invocations.log"
printf 'gh %s\n' "$*" >> "$log"

state="${GH_STATE_DIR:-}"
remote="${state:+$state/release.tsv}"

cmd="${1:-}"
[ $# -gt 0 ] && shift

case "$cmd" in
  api)
    # `gh api repos/<repo>/releases/tags/<tag> --jq …` — the caller renders what
    # it needs from the response, so the double answers with the rendered rows.
    [ -n "$remote" ] && [ -f "$remote" ] || exit 1
    cat "$remote"
    exit 0
    ;;
  release)
    sub="${1:-}"
    [ $# -gt 0 ] && shift
    case "$sub" in
      view)
        if [ -n "${GH_ASSETS_FILE:-}" ] && [ -f "${GH_ASSETS_FILE}" ]; then
          cat "$GH_ASSETS_FILE"
          exit 0
        fi
        [ -n "$remote" ] && [ -f "$remote" ] || exit 1
        awk -F'\t' '{print $1}' "$remote"
        exit 0
        ;;
      download)
        pattern=""; dir=""; expect=""
        for a in "$@"; do
          case "$a" in
            --pattern) expect="pattern" ;;
            --dir) expect="dir" ;;
            --repo) expect="skip" ;;
            --*) expect="" ;;
            *)
              case "$expect" in
                pattern) pattern="$a" ;;
                dir) dir="$a" ;;
              esac
              expect=""
              ;;
          esac
        done
        [ -n "$pattern" ] && [ -n "$dir" ] || exit 1
        [ -f "$state/files/$pattern" ] || exit 1
        cp "$state/files/$pattern" "$dir/$pattern" || exit 1
        exit 0
        ;;
      upload)
        clobber=0; tag=""; files=""; expect=""
        for a in "$@"; do
          if [ "$expect" = "skip" ]; then expect=""; continue; fi
          case "$a" in
            --clobber) clobber=1; expect="" ;;
            --repo) expect="skip" ;;
            --*) expect="" ;;
            *)
              if [ -z "$tag" ]; then tag="$a"; else files="${files}${files:+ }$a"; fi
              ;;
          esac
        done
        [ -n "$remote" ] && [ -f "$remote" ] || exit 1
        exists=""
        for f in $files; do
          n="$(basename "$f")"
          if awk -F'\t' -v n="$n" '$1 == n { found = 1 } END { exit !found }' "$remote"; then
            exists="${exists}${exists:+ }${n}"
          fi
        done
        if [ -n "$exists" ] && [ "$clobber" != "1" ]; then
          # GitHub's own message, verbatim — the one run 42246232196 died on.
          printf 'asset under the same name already exists: [%s]\n' "$exists" >&2
          exit 1
        fi
        for f in $files; do
          n="$(basename "$f")"
          [ -f "$f" ] || {
            printf 'no such file: %s\n' "$f" >&2
            exit 1
          }
          if command -v sha256sum >/dev/null 2>&1; then
            hex="$(sha256sum "$f" | awk '{print $1}')"
          else
            hex="$(shasum -a 256 "$f" | awk '{print $1}')"
          fi
          d="sha256:${hex}"
          s="$(wc -c < "$f" | tr -d '[:space:]')"
          awk -F'\t' -v n="$n" '$1 != n' "$remote" > "$remote.tmp"
          printf '%s\t%s\t%s\n' "$n" "$d" "$s" >> "$remote.tmp"
          LC_ALL=C sort -o "$remote" "$remote.tmp"
          rm -f "$remote.tmp"
        done
        exit 0
        ;;
      create)
        [ -n "$remote" ] || exit 1
        : > "$remote"
        exit 0
        ;;
    esac
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
write_manifest() { # <dir> [asset the manifest must not know]
  local dir="$1" omit="${2:-}" name
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
  write_manifest "$dir" "$omit"
}

# The stamped fixture: the same dist/ shape, but every binary payload carries
# the IDENTITY SHAPES the publisher's reader greps out of real release
# binaries — the ${SELFTEST_VERSION} token and one exactly-40-hex commit run —
# plus the BUILD_TIME string a rebuild stamps in. Real go1.26.10 binaries carry
# these as bare rodata VALUES (no -ldflags build setting is recorded, measured),
# so the fixture mimics exactly that: value tokens among plain text, no
# name=value blob the reader could lean on. install.sh carries NO stamp — it is
# not a Go binary — and SHA256SUMS covers the set but not itself, as everywhere
# else. The optional vcs-modified argument embeds `vcs.modified=true`, the
# shape a toolchain that records VCS status would stamp for a dirty tree.
make_stamped_fixture() { # <dir> <commit> <build-time> [vcs-modified]
  local dir="$1" com="$2" ts="$3" vcsmod="${4:-}" name target os arch names=""
  mkdir -p "$dir"
  for target in $SELFTEST_TARGETS; do
    os="${target%%/*}"
    arch="${target##*/}"
    names="${names} crier_${os}_${arch} crier-mcp_${os}_${arch}"
  done
  for name in $names; do
    {
      printf 'release-upload-selftest stamped fixture payload: %s\n' "$name"
      printf 'stamped-version: %s\n' "$SELFTEST_VERSION"
      printf 'stamped-commit: %s\n' "$com"
      printf 'stamped-build-time: %s (build metadata — differs on a rebuild)\n' "$ts"
      if [ -n "$vcsmod" ]; then
        printf 'vcs.modified=%s\n' "$vcsmod"
      fi
    } > "$dir/$name"
  done
  printf 'release-upload-selftest fixture payload: install.sh (carries no build stamp)\n' > "$dir/install.sh"
  write_manifest "$dir"
}

# The MATERIAL-DIVERGENCE fixture (INT-CI-034): the same identity stamps as
# make_stamped_fixture — same ${SELFTEST_VERSION} token, same single 40-hex
# commit, no vcs.modified — but the BODY is a different build's: a 16 KiB
# filler run whose CONTENT differs between the two builds (thousands of
# differing byte offsets, orders of magnitude beyond the measured 63/63/153 a
# metadata-only rebuild stamps), plus, when <extra> is given, an appended line
# the other build does not carry (a real size delta, beyond the
# size-identical metadata-only spread). This is the shape a dirty-tree
# rebuild at the same commit could produce on go1.26.10, where no
# vcs.modified is stamped to catch it — exactly what the publisher's metadata
# bound must refuse.
make_material_fixture() { # <dir> <commit> <build-time> <filler-char> [extra line]
  local dir="$1" com="$2" ts="$3" fill="$4" extra="${5:-}" name target os arch names=""
  mkdir -p "$dir"
  for target in $SELFTEST_TARGETS; do
    os="${target%%/*}"
    arch="${target##*/}"
    names="${names} crier_${os}_${arch} crier-mcp_${os}_${arch}"
  done
  for name in $names; do
    {
      printf 'release-upload-selftest stamped fixture payload: %s\n' "$name"
      printf 'stamped-version: %s\n' "$SELFTEST_VERSION"
      printf 'stamped-commit: %s\n' "$com"
      printf 'stamped-build-time: %s (build metadata — differs on a rebuild)\n' "$ts"
      head -c 16384 /dev/zero | tr '\0' "$fill"
      printf '\n'
      if [ -n "$extra" ]; then printf '%s\n' "$extra"; fi
    } > "$dir/$name"
  done
  printf 'release-upload-selftest fixture payload: install.sh (carries no build stamp)\n' > "$dir/install.sh"
  write_manifest "$dir"
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

# ── The simulated Release object ────────────────────────────────────────────
# publish_to_shim <dist-dir> [digest-mode]
#   real (default) — every attached asset carries its `sha256:…` digest, which is
#                    what GitHub reports for an asset it hashed at upload time.
#   none           — every attached asset reports NO digest, the shape of a
#                    Release published before GitHub recorded digests. The
#                    publisher may not assume "identical" from a missing field,
#                    so this mode is what forces the download-and-hash arm.
publish_to_shim() { # <dist-dir> [real|none]
  local dist="$1" mode="${2:-real}" name digest size f
  rm -rf "$SHIM_STATE"
  mkdir -p "$SHIM_STATE/files"
  : > "$SHIM_STATE/release.tsv"
  for f in "$dist"/*; do
    [ -f "$f" ] || continue
    name="$(basename "$f")"
    cp "$f" "$SHIM_STATE/files/$name"
    if [ "$mode" = "none" ]; then
      digest=""
    else
      # shellcheck disable=SC2086
      digest="sha256:$($SHA_TOOL "$f" | awk '{print $1}')"
    fi
    size="$(wc -c < "$f" | tr -d '[:space:]')"
    printf '%s\t%s\t%s\n' "$name" "$digest" "$size" >> "$SHIM_STATE/release.tsv"
  done
}

# Keep only the named assets attached — the shape of a publish that half-failed.
keep_only_attached() { # <name>…
  local keep="$*" tmp="$SHIM_STATE/release.tsv.new" want
  : > "$tmp"
  for want in $keep; do
    awk -F'\t' -v n="$want" '$1 == n' "$SHIM_STATE/release.tsv" >> "$tmp"
  done
  mv "$tmp" "$SHIM_STATE/release.tsv"
}

shim_log_reset() { : > "$GH_LOG"; }
shim_calls() { grep -c -- "$1" "$GH_LOG" 2>/dev/null || true; }
shim_upload_line() { grep -- 'release upload' "$GH_LOG" 2>/dev/null | head -1 || true; }
shim_attached_count() { wc -l < "$SHIM_STATE/release.tsv" | tr -d '[:space:]'; }
shim_attached_digest() { # <name>
  awk -F'\t' -v n="$1" '$1 == n { print $2 }' "$SHIM_STATE/release.tsv"
}

# The last target's server asset is the one the "unknown" fixtures leave out of
# the manifest, so the omitted name always belongs to the shipped set.
LAST_TARGET="${SELFTEST_TARGETS##* }"
OMITTED_ASSET="crier_${LAST_TARGET%%/*}_${LAST_TARGET##*/}"

# Identity constants for the INT-CI-033 arms. Both commits are exactly 40 hex
# chars and carry letters beyond digits/e, i.e. the shape the reader accepts as
# a commit stamp (a digits-only-with-e run would be refused as a float
# mantissa). The two BUILD_TIMEs are what a hand publish and a tag-workflow
# rebuild ~2 minutes apart would stamp.
IDENT_COMMIT_A="0f1e2d3c4b5a69788796a5b4c3d2e1f00918a7b6"
IDENT_COMMIT_B="fedcba9876543210fedcba9876543210abcdef01"
IDENT_TS_LOCAL="2026-11-07T10:00:00Z"
IDENT_TS_ATTACHED="2026-11-07T10:02:30Z"

CORRECT="$WORKDIR/fixture-correct"
UNKNOWN="$WORKDIR/fixture-unknown-in-set"
make_fixture "$CORRECT"
make_fixture "$UNKNOWN" "$OMITTED_ASSET"
printf 'RELEASE-010 selftest release notes\n' > "$WORKDIR/notes.md"

# ── [1/15] release-upload.sh accepts the correct set ────────────────────────
echo
echo "==> [1/15] release-upload.sh accepts a CORRECT set (every manifest asset + SHA256SUMS)"
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

# ── [2/15] release-upload.sh rejects a genuinely unknown asset ──────────────
echo
echo "==> [2/15] release-upload.sh rejects an asset SHA256SUMS does not know (${OMITTED_ASSET})"
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

# ── [3/15] the WORKFLOW's verification block, executed ──────────────────────
# The v0.2.0 defect lived in the workflow step, so the workflow step is what gets
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
echo "==> [3/15] the workflow's own verification block, executed (bash -e, gh is a shim)"
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

# ── [4/15] NEUTER proof ─────────────────────────────────────────────────────
echo
echo "==> [4/15] NEUTER proof: without the SHA256SUMS exemption the correct fixture reds"
if run_workflow_block "$NEUTERED_BLOCK" "$WORKDIR/attached-correct.txt"; then
  fail "the NEUTERED workflow block (exemption removed) ACCEPTED the correct Release — the exemption is not load-bearing and the green above is vacuous"
fi
printf '%s\n' "$WF_OUT" | grep -q 'carries asset(s) SHA256SUMS does not know:SHA256SUMS' \
  || fail "the neutered block failed, but not with the historical SHA256SUMS error — the reproduction does not match the v0.2.0 red"
echo "    FAIL (expected): the neutered block reproduced the v0.2.0 error verbatim"
echo "    PASS: the SHA256SUMS exemption is load-bearing"

# ── [5/15] CONVERGENCE: an already-attached identical set is a NO-OP ───────
# The v0.3.0 defect, driven for real (no --dry-run): the Release already carries
# every asset this publish would upload, byte for byte. GitHub would refuse all
# eight uploads; the publisher must therefore NOT attempt any of them.
echo
echo "==> [5/15] CONVERGED: the Release already carries the EXACT asset set (no upload may be attempted)"
publish_to_shim "$CORRECT" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$CORRECT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh exited ${RC} against a Release that already carries the identical set — this is the v0.3.0 defect (RELEASE-012)"
printf '%s\n' "$OUT" | grep -qi 'converged' \
  || fail "release-upload.sh exited 0 but never reported convergence — the arm is not proven"
[ "$(shim_calls 'release upload')" = "0" ] \
  || fail "release-upload.sh CONVERGED but still issued an upload (log: $(shim_upload_line)) — the collision would still happen"
[ "$(shim_attached_count)" = "$((CORRECT_COUNT + 1))" ] \
  || fail "the simulated Release no longer carries $((CORRECT_COUNT + 1)) asset(s) — the arm mutated state it should not have"
echo "    PASS: exit 0, reported converged, zero uploads attempted ($(shim_attached_count) asset(s) untouched)"

# 5b — the same, with NO digests on the Release. The publisher may not read
# "no digest" as "identical": it must download and hash each attached asset.
echo
echo "==> [5b/15] CONVERGED without GitHub digests: content is measured, not assumed"
publish_to_shim "$CORRECT" none
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$CORRECT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh exited ${RC} against a digest-less Release carrying the identical set"
printf '%s\n' "$OUT" | grep -qi 'converged' || fail "release-upload.sh exited 0 without reporting convergence"
[ "$(shim_calls 'release download')" -gt 0 ] \
  || fail "the release carries no digests, yet release-upload.sh never downloaded an asset to compare — it assumed identity it never measured"
[ "$(shim_calls 'release upload')" = "0" ] || fail "release-upload.sh uploaded against a digest-less but identical Release"
echo "    PASS: $(shim_calls 'release download') asset(s) downloaded and hashed, zero uploads, converged"

# ── [6/15] DIVERGENCE: different content under the same name is REFUSED ─────
# The same names as the attached set, but one binary and therefore the manifest
# carry different bytes: a re-publish that would swap a published binary.
echo
echo "==> [6/15] DIFFERENT content under an attached name is refused unless CLOBBER=1"
DIVERGENT="$WORKDIR/fixture-divergent"
make_fixture "$DIVERGENT"
printf 'this build is NOT the one the Release already carries\n' >> "$DIVERGENT/crier_linux_amd64"
write_manifest "$DIVERGENT"
publish_to_shim "$CORRECT" real
ORIGINAL_DIGEST="$(shim_attached_digest crier_linux_amd64)"
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$DIVERGENT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ACCEPTED a set whose crier_linux_amd64 differs from the attached one (exit 0) — a published binary would be replaced silently"
printf '%s\n' "$OUT" | grep -q 'crier_linux_amd64' || fail "release-upload.sh refused the set but never named the differing asset"
printf '%s\n' "$OUT" | grep -q 'SHA256SUMS' || fail "release-upload.sh refused the set but never named the manifest that differs with it"
[ "$(shim_calls 'release upload')" = "0" ] || fail "release-upload.sh refused the set AND still issued an upload"
[ "$(shim_attached_digest crier_linux_amd64)" = "$ORIGINAL_DIGEST" ] || fail "the refused publish mutated the attached asset anyway"
echo "    FAIL (expected): the differing asset was refused by name — exit ${RC}"
echo "    PASS: different content under a published name is fatal without CLOBBER=1"

# 6b — CLOBBER=1 replaces ONLY the differing assets; identical ones stay skipped.
echo
echo "==> [6b/15] CLOBBER=1 replaces the differing assets deliberately (identical ones still skipped)"
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$DIVERGENT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 CLOBBER=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh exited ${RC} with CLOBBER=1 — the deliberate replacement path is broken"
UPLOAD_LINE="$(shim_upload_line)"
[ -n "$UPLOAD_LINE" ] || fail "CLOBBER=1 exited 0 but issued no upload — nothing was replaced"
printf '%s\n' "$UPLOAD_LINE" | grep -q -- '--clobber' || fail "CLOBBER=1 uploaded without --clobber (log: ${UPLOAD_LINE})"
printf '%s\n' "$UPLOAD_LINE" | grep -q 'crier_linux_amd64' || fail "CLOBBER=1 did not upload the asset that differs (log: ${UPLOAD_LINE})"
printf '%s\n' "$UPLOAD_LINE" | grep -q 'install.sh' \
  && fail "CLOBBER=1 re-uploaded install.sh, which is byte-identical — identical assets must still be skipped (log: ${UPLOAD_LINE})"
NEW_DIGEST="$(shim_attached_digest crier_linux_amd64)"
[ "$NEW_DIGEST" != "$ORIGINAL_DIGEST" ] || fail "the simulated Release still carries the old digest — the upload never landed"
[ "$(shim_attached_count)" = "$((CORRECT_COUNT + 1))" ] || fail "CLOBBER=1 changed the number of attached assets"
echo "    PASS: only the differing assets were uploaded with --clobber and the attached digest changed"

# ── [7/15] REPAIR: a half-failed publish converges the rest ─────────────────
# A publish that died after two uploads leaves a Release carrying part of the
# set. Re-running must upload exactly the rest — no collision, no --clobber, and
# no re-upload of what already landed.
echo
echo "==> [7/15] REPAIR: a half-published Release gets exactly the missing assets"
publish_to_shim "$CORRECT" real
keep_only_attached install.sh SHA256SUMS
ATTACHED_BEFORE="$(shim_attached_count)"
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$CORRECT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh exited ${RC} repairing a half-published Release carrying ${ATTACHED_BEFORE} of $((CORRECT_COUNT + 1)) asset(s)"
UPLOAD_LINE="$(shim_upload_line)"
[ -n "$UPLOAD_LINE" ] || fail "the repair exited 0 without uploading the missing assets"
printf '%s\n' "$UPLOAD_LINE" | grep -q -- '--clobber' && fail "the repair used --clobber, which would replace assets that are already correct (log: ${UPLOAD_LINE})"
printf '%s\n' "$UPLOAD_LINE" | grep -q 'install.sh' && fail "the repair re-uploaded install.sh, which was already attached (log: ${UPLOAD_LINE})"
printf '%s\n' "$UPLOAD_LINE" | grep -q 'crier_linux_amd64' || fail "the repair never uploaded the missing crier_linux_amd64 (log: ${UPLOAD_LINE})"
[ "$(shim_attached_count)" = "$((CORRECT_COUNT + 1))" ] \
  || fail "after the repair the Release carries $(shim_attached_count) asset(s), want $((CORRECT_COUNT + 1))"
echo "    PASS: ${ATTACHED_BEFORE} attached + the missing ones uploaded = $(shim_attached_count) asset(s), no --clobber"

# ── [8/15] NEUTER proof: without the reconciliation the collision returns ───
# The neutered copy is the PRE-FIX shape of the publisher: it treats every asset
# as needing an upload and refuses nothing, so the shim's GitHub behaviour (a
# duplicate name is an error) rejects it exactly as run 42246232196 was rejected.
# Both arms under test here are new in RELEASE-012, so removing BOTH restores the
# script that failed on v0.3.0.
cat > "$WORKDIR/neuter-upload.py" <<'PY'
import sys

src, out = sys.argv[1], sys.argv[2]
sentinels = tuple(sys.argv[3:]) or ("# CONVERGENCE-ARM:", "# DIVERGENCE-ARM:")
lines = open(src, encoding="utf-8").read().split("\n")

# Each arm is anchored on its own sentinel comment, and the DECISION LINE that
# follows it is neutralised. A sentinel that disappears (a refactor) must break
# this proof loudly: silently proving nothing is worse than a red selftest.
found = {}
for i, line in enumerate(lines):
    for s in sentinels:
        if s not in line:
            continue
        nxt = i + 1
        while nxt < len(lines) and lines[nxt].strip() == "":
            nxt += 1
        if nxt >= len(lines) or not lines[nxt].strip().startswith("if "):
            sys.exit(f"neuter: the line after {s!r} is not an `if` — cannot neutralise it")
        found[s] = nxt

missing = [s for s in sentinels if s not in found]
if missing:
    sys.exit("neuter: sentinel(s) not found: " + ", ".join(missing) + " — the proof would be vacuous")

for s, nxt in found.items():
    indent = lines[nxt][: len(lines[nxt]) - len(lines[nxt].lstrip())]
    lines[nxt] = indent + "if false; then   # NEUTERED: " + s.lstrip("# ")

open(out, "w", encoding="utf-8").write("\n".join(lines))
PY
NEUTERED_UPLOAD="$WORKDIR/release-upload-neutered.sh"
python3 "$WORKDIR/neuter-upload.py" "$SCRIPT_DIR/release-upload.sh" "$NEUTERED_UPLOAD" \
  "# CONVERGENCE-ARM:" "# DIVERGENCE-ARM:"
chmod 0755 "$NEUTERED_UPLOAD"
echo
echo "==> [8/15] NEUTER proof: without the reconciliation, [5/15]'s fixture fails on the name collision"
publish_to_shim "$CORRECT" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$CORRECT" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$NEUTERED_UPLOAD" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "the NEUTERED publisher (reconciliation removed) ACCEPTED the identical set — the convergence is not load-bearing and [5/15] proves nothing"
printf '%s\n' "$OUT" | grep -q 'asset under the same name already exists' \
  || fail "the neutered publisher failed, but not with GitHub's collision error — the reproduction does not match the v0.3.0 red"
[ "$(shim_calls 'release upload')" -gt 0 ] || fail "the neutered publisher never attempted an upload — the double is not exercising GitHub's behaviour"
echo "    FAIL (expected): the pre-fix shape reproduced the v0.3.0 collision verbatim"
echo "    PASS: the same-set convergence is load-bearing"

# ── [9/15] IDENTITY ADOPTION: a same-commit rebuild is adopted, not refused ─
# The v0.4.0 shape (run 42422236749): the Release already carries every asset,
# but each binary was stamped with a LATER BUILD_TIME than this build's, so
# every byte differs. The publisher must download the attached bytes, read the
# identity from them, adopt all six binaries AND their manifest — and upload
# nothing: the published set is kept exactly as it is.
echo
echo "==> [9/15] IDENTITY ADOPTION: same Version+Commit, later BUILD_TIME — adopted, zero uploads"
STAMPED_LOCAL="$WORKDIR/fixture-stamped-local"
STAMPED_ATTACHED="$WORKDIR/fixture-stamped-attached"
make_stamped_fixture "$STAMPED_LOCAL" "$IDENT_COMMIT_A" "$IDENT_TS_LOCAL"
make_stamped_fixture "$STAMPED_ATTACHED" "$IDENT_COMMIT_A" "$IDENT_TS_ATTACHED"
publish_to_shim "$STAMPED_ATTACHED" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$STAMPED_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "release-upload.sh exited ${RC} against a same-commit rebuild — the v0.4.0 defect (INT-CI-033) is not fixed"
printf '%s\n' "$OUT" | grep -q 'ADOPTED by build identity' \
  || fail "release-upload.sh exited 0 but never reported an adoption — the arm is not proven"
printf '%s\n' "$OUT" | grep -q 'BUILD_TIME' \
  || fail "the adoption was reported but the BUILD_TIME difference was never named as expected metadata"
printf '%s\n' "$OUT" | grep -qi 'converged' \
  || fail "the adoption run did not report convergence"
[ "$(shim_calls 'release upload')" = "0" ] \
  || fail "the adopt run issued an upload (log: $(shim_upload_line)) — adoption must KEEP the published set, never replace it"
[ "$(shim_calls 'release download')" -gt 0 ] \
  || fail "the adopt run never downloaded an attached asset — the identity was not read from the bytes"
[ "$(shim_attached_count)" = "$((CORRECT_COUNT + 1))" ] \
  || fail "the adopt run mutated the Release ($(shim_attached_count) asset(s), want $((CORRECT_COUNT + 1)))"
# INT-CI-034: the ADOPTED line must report the MEASURED divergence (size delta
# + differing-offset count), computed here independently of the publisher, so
# a converged run carries its own evidence.
EXP9_LSIZE="$(wc -c < "$STAMPED_LOCAL/crier_linux_amd64" | tr -d '[:space:]')"
EXP9_ASIZE="$(wc -c < "$STAMPED_ATTACHED/crier_linux_amd64" | tr -d '[:space:]')"
EXP9_DELTA="$(( EXP9_LSIZE > EXP9_ASIZE ? EXP9_LSIZE - EXP9_ASIZE : EXP9_ASIZE - EXP9_LSIZE ))"
EXP9_OFFSETS="$(cmp -l "$STAMPED_LOCAL/crier_linux_amd64" "$STAMPED_ATTACHED/crier_linux_amd64" 2>/dev/null | wc -l | tr -d '[:space:]' || true)"
printf '%s\n' "$OUT" | grep -Fq "measured divergence: size delta ${EXP9_DELTA} byte(s), ${EXP9_OFFSETS} differing byte offset(s)" \
  || fail "the ADOPTED line did not report the measured divergence (want size delta ${EXP9_DELTA} byte(s), ${EXP9_OFFSETS} differing byte offset(s)) — a converged run must carry its own evidence (INT-CI-034)"
echo "    PASS: exit 0, adoption reported with BUILD_TIME as expected metadata, zero uploads ($(shim_attached_count) asset(s) untouched; measured divergence: size delta ${EXP9_DELTA} byte(s), ${EXP9_OFFSETS} differing byte offset(s))"

# ── [10/15] IDENTITY REFUSAL: a different commit / a dirty tree still refuses ─
# RELEASE-012 kept (INT-CI-033's AC3). Two shapes, both nonzero with no upload:
# 10a — the attached binaries carry a DIFFERENT commit (another tree's build):
#       refused, naming the asset, the manifest, and both commits.
# 10b — the attached binaries report vcs.modified=true: the commit they name
#       does not describe their bytes, so the identity cannot be trusted.
echo
echo "==> [10/15] IDENTITY REFUSAL: a different commit (10a) and vcs.modified=true (10b) still refuse"
STAMPED_FOREIGN="$WORKDIR/fixture-stamped-foreign-commit"
make_stamped_fixture "$STAMPED_FOREIGN" "$IDENT_COMMIT_B" "$IDENT_TS_ATTACHED"
publish_to_shim "$STAMPED_FOREIGN" real
ORIGINAL_DIGEST="$(shim_attached_digest crier_linux_amd64)"
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$STAMPED_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ACCEPTED a set whose binaries carry commit ${IDENT_COMMIT_B} (exit 0) — RELEASE-012 was weakened"
printf '%s\n' "$OUT" | grep -q 'crier_linux_amd64' || fail "the refusal never named the differing asset"
printf '%s\n' "$OUT" | grep -q 'SHA256SUMS' || fail "the refusal never named the manifest of the refused set"
printf '%s\n' "$OUT" | grep -q 'build identity differs' || fail "the refusal never said WHICH fact made it a refusal"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the refused run still issued an upload"
[ "$(shim_attached_digest crier_linux_amd64)" = "$ORIGINAL_DIGEST" ] || fail "the refused run mutated the attached asset anyway"
echo "    FAIL (expected): the foreign-commit build was refused by name, with both commits printed"
echo "    PASS: a genuinely different commit is still fatal without CLOBBER=1 (10a)"

STAMPED_MODIFIED="$WORKDIR/fixture-stamped-dirty-tree"
make_stamped_fixture "$STAMPED_MODIFIED" "$IDENT_COMMIT_A" "$IDENT_TS_ATTACHED" true
publish_to_shim "$STAMPED_MODIFIED" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$STAMPED_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ACCEPTED a set reporting vcs.modified=true (exit 0)"
printf '%s\n' "$OUT" | grep -q 'vcs.modified=true' || fail "the refusal never named the modified-tree stamp"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the vcs.modified refusal still issued an upload"
echo "    FAIL (expected): the modified-tree build was refused"
echo "    PASS: an attached vcs.modified=true is fatal (10b)"

# ── [11/15] NEUTER proof: without the adoption decision the rebuild reds ────
# A copy of the publisher with ONLY the IDENTITY-ARM decision neutralised must
# fail [9/15]'s same-commit-rebuild fixture with the RELEASE-012 refusal —
# i.e. the exact run 42422236749 red. If the neutered copy still exits 0, the
# adoption arm is decoration and [9/15] proves nothing.
echo
echo "==> [11/15] NEUTER proof: with ONLY the adoption decision removed, [9/15]'s fixture reds"
NEUTERED_IDENTITY="$WORKDIR/release-upload-neutered-identity.sh"
python3 "$WORKDIR/neuter-upload.py" "$SCRIPT_DIR/release-upload.sh" "$NEUTERED_IDENTITY" "# IDENTITY-ARM:"
chmod 0755 "$NEUTERED_IDENTITY"
publish_to_shim "$STAMPED_ATTACHED" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$STAMPED_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$NEUTERED_IDENTITY" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "the NEUTERED publisher (adoption removed) ACCEPTED the same-commit rebuild — the adoption arm is not load-bearing and [9/15] proves nothing"
printf '%s\n' "$OUT" | grep -q 'a published asset is never replaced silently' \
  || fail "the neutered publisher failed, but not with the RELEASE-012 refusal — the reproduction does not match the v0.4.0 red"
printf '%s\n' "$OUT" | grep -q 'ADOPTED by build identity — both sides carry' \
  && fail "the neutered publisher still reported an adoption — the neuter did not remove the adoption decision"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the neutered publisher uploaded while refusing"
echo "    FAIL (expected): the pre-INT-CI-033 shape reproduced the v0.4.0 refusal verbatim"
echo "    PASS: the adoption decision is load-bearing"

# ── [12/15] NON-BINARY DRIFT: a differing install.sh still refuses ──────────
# install.sh carries no build stamp, so it can never be adopted: it must
# converge byte-identically as it always has. Its SHA256SUMS entry changes with
# it, so the manifest is refused with it (the manifest of a refused set).
echo
echo "==> [12/15] NON-BINARY DRIFT: install.sh (no build stamp) differing still refuses"
STAMPED_ATTACHED_DRIFT="$WORKDIR/fixture-stamped-installdrift"
make_stamped_fixture "$STAMPED_ATTACHED_DRIFT" "$IDENT_COMMIT_A" "$IDENT_TS_ATTACHED"
printf 'this installer is NOT the one the Release already carries\n' >> "$STAMPED_ATTACHED_DRIFT/install.sh"
write_manifest "$STAMPED_ATTACHED_DRIFT"
publish_to_shim "$STAMPED_ATTACHED_DRIFT" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$STAMPED_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ACCEPTED a set whose install.sh differs from the attached one (exit 0)"
printf '%s\n' "$OUT" | grep -q 'install.sh' || fail "the refusal never named install.sh"
printf '%s\n' "$OUT" | grep -q 'non-binary' || fail "the refusal never said WHY (non-binary drift)"
printf '%s\n' "$OUT" | grep -q 'SHA256SUMS' || fail "the refusal never named the manifest of the refused set"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the refused run still issued an upload"
echo "    FAIL (expected): install.sh drift was refused as non-binary drift, manifest with it"
echo "    PASS: a non-binary asset is never adopted — content drift keeps the refusal"

# ── [13/15] the scripts under test are syntactically sound ───────────────────
echo
echo "==> [13/15] syntax: bash -n on the scripts, YAML parse of the workflow"
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

# ── [14/15] METADATA BOUND: a copied identity stamp on a different body refuses ─
# The INT-CI-034 shape: the attached binaries carry the SAME Version token and
# the SAME single 40-hex commit as this build (every identity ground passes)
# but their bodies are a different build's — a 16 KiB filler run that differs
# between the builds plus an appended line, so the divergence is thousands of
# offsets and a real size delta, far beyond the measured metadata-only spread
# (size-identical, 63/63/153 offsets). The publisher must refuse, quoting the
# MEASURED numbers, with no upload and nothing replaced.
echo
echo "==> [14/15] METADATA BOUND: same Version+Commit, materially different body — refused with measured numbers"
MATERIAL_LOCAL="$WORKDIR/fixture-material-local"
MATERIAL_ATTACHED="$WORKDIR/fixture-material-attached"
make_material_fixture "$MATERIAL_LOCAL" "$IDENT_COMMIT_A" "$IDENT_TS_LOCAL" L
make_material_fixture "$MATERIAL_ATTACHED" "$IDENT_COMMIT_A" "$IDENT_TS_ATTACHED" M \
  "material drift: appended body the published build does not carry"
# What the bound must see, measured HERE independently of the publisher:
MAT_LSIZE="$(wc -c < "$MATERIAL_LOCAL/crier_linux_amd64" | tr -d '[:space:]')"
MAT_ASIZE="$(wc -c < "$MATERIAL_ATTACHED/crier_linux_amd64" | tr -d '[:space:]')"
MAT_DELTA="$(( MAT_LSIZE > MAT_ASIZE ? MAT_LSIZE - MAT_ASIZE : MAT_ASIZE - MAT_LSIZE ))"
MAT_OFFSETS="$(cmp -l "$MATERIAL_LOCAL/crier_linux_amd64" "$MATERIAL_ATTACHED/crier_linux_amd64" 2>/dev/null | wc -l | tr -d '[:space:]' || true)"
[ "$MAT_DELTA" -gt 0 ] || fail "the material fixture has no size delta — the fixture is broken"
[ "$MAT_OFFSETS" -gt 4096 ] || fail "the material fixture diverges in only ${MAT_OFFSETS} offset(s) — it does not breach the default offsets bound and proves nothing"
publish_to_shim "$MATERIAL_ATTACHED" real
ORIGINAL_DIGEST="$(shim_attached_digest crier_linux_amd64)"
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$MATERIAL_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$SCRIPT_DIR/release-upload.sh" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -ne 0 ] || fail "release-upload.sh ADOPTED a materially different body wearing a copied identity stamp (exit 0) — the metadata bound is not enforced (INT-CI-034)"
printf '%s\n' "$OUT" | grep -q 'crier_linux_amd64' || fail "the refusal never named the diverging asset"
printf '%s\n' "$OUT" | grep -q 'SHA256SUMS' || fail "the refusal never named the manifest of the refused set"
printf '%s\n' "$OUT" | grep -q 'diverge beyond the metadata bound' || fail "the refusal never said WHY (beyond the metadata bound)"
printf '%s\n' "$OUT" | grep -Fq "size delta ${MAT_DELTA} byte(s), ${MAT_OFFSETS} differing byte offset(s)" \
  || fail "the refusal did not quote the MEASURED divergence (want size delta ${MAT_DELTA} byte(s), ${MAT_OFFSETS} differing byte offset(s))"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the bound-breach refusal still issued an upload"
[ "$(shim_attached_digest crier_linux_amd64)" = "$ORIGINAL_DIGEST" ] || fail "the bound-breach refusal mutated the attached asset anyway"
echo "    FAIL (expected): the copied-identity build was refused — measured size delta ${MAT_DELTA} byte(s), ${MAT_OFFSETS} differing byte offset(s)"
echo "    PASS: a body beyond the metadata bound is refused even with identical identity stamps"

# ── [15/15] NEUTER proof: without the metadata bound the material fixture is adopted ─
# A copy of the publisher with ONLY the ADOPT-BOUND-ARM decision neutralised
# must ADOPT [14/15]'s fixture — every identity ground passes for it, so the
# bound is the ONLY thing standing between [14/15]'s refusal and an adoption.
# If the neutered copy still refuses, the bound is not load-bearing and
# [14/15] proves nothing.
echo
echo "==> [15/15] NEUTER proof: with ONLY the metadata bound removed, [14/15]'s fixture is ADOPTED"
NEUTERED_BOUND="$WORKDIR/release-upload-neutered-bound.sh"
python3 "$WORKDIR/neuter-upload.py" "$SCRIPT_DIR/release-upload.sh" "$NEUTERED_BOUND" "# ADOPT-BOUND-ARM:"
chmod 0755 "$NEUTERED_BOUND"
publish_to_shim "$MATERIAL_ATTACHED" real
shim_log_reset
set +e
OUT="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$MATERIAL_LOCAL" \
  GH_STATE_DIR="$SHIM_STATE" ALLOW_TAG_DRIFT=1 \
  bash "$NEUTERED_BOUND" --notes-file "$WORKDIR/notes.md" 2>&1)"
RC=$?
set -e
printf '%s\n' "$OUT" | indent
[ "$RC" -eq 0 ] || fail "the NEUTERED publisher (bound removed) REFUSED the material-divergence fixture — the bound is not load-bearing and [14/15] proves nothing"
printf '%s\n' "$OUT" | grep -q 'ADOPTED by build identity' \
  || fail "the neutered publisher exited 0 but never reported an adoption — the neuter did not exercise the adoption path"
printf '%s\n' "$OUT" | grep -q 'diverge beyond the metadata bound' \
  && fail "the neutered publisher still reported the bound refusal — the neuter did not remove the bound"
[ "$(shim_calls 'release upload')" = "0" ] || fail "the neutered publisher uploaded while adopting"
echo "    FAIL (expected): the pre-INT-CI-034 shape ADOPTED the material-divergence fixture"
echo "    PASS: the metadata bound is load-bearing"

echo
echo "release-upload-selftest: PASS — a correct asset set (every manifest asset + SHA256SUMS)"
echo "                         is accepted by BOTH the publish script and the workflow's"
echo "                         self-check, a genuinely unknown asset is still refused by name,"
echo "                         the SHA256SUMS exemption is proven load-bearing,"
echo "                         re-publishing an IDENTICAL set converges with no upload,"
echo "                         a same-commit REBUILD is adopted by build identity with zero"
echo "                         uploads (INT-CI-033) and the adoption is proven load-bearing,"
echo "                         a materially different body wearing a copied identity stamp is"
echo "                         refused by the metadata bound with its measured numbers"
echo "                         (INT-CI-034) and the bound is proven load-bearing,"
echo "                         a different commit, a vcs.modified=true build and a differing"
echo "                         install.sh are still refused, CLOBBER=1 stays the only"
echo "                         replacement path, and a half-published Release is repaired."
