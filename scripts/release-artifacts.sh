#!/usr/bin/env bash
#
# scripts/release-artifacts.sh — CR-FEAT-028: the release front door, BUILD half.
#
# WHY THIS EXISTS
# ---------------
# `gh release view v0.1.0-rc2` returned `assets: []` — the Release object carried
# notes only, so the only way into this project was `git clone` + a Go toolchain
# + `make build`. The external hands-on review (DISPATCH · CRI-001, Carter,
# 2026-09-25) filed that as a tester-funnel problem: a prebuilt binary and a
# curl-to-install path widen the funnel, and a tester who cannot install is a
# tester who cannot report anything.
#
# This script is the build half of the fix. It cross-compiles the two shipped
# binaries for every release target, writes the SHA256SUMS the installer verifies
# against, and verifies its own output before returning (checksums re-checked,
# file(1) type per artifact, and the HOST artifact actually executed to prove it
# carries the stamped identity).
#
# The install half is scripts/install.sh, which is copied into the artifact set
# so a pinned version can fetch its own installer.
#
# BUILD SHAPE
# -----------
# The same shape the Dockerfile ships: `CGO_ENABLED=0` (a static binary with the
# pure-Go resolver — no libc to match on the target box), `-trimpath` (no build
# paths of this checkout leak into the artifact), `-s -w` (stripped). The
# identity is stamped from internal/buildinfo exactly as `make build` does it, so
# `crier -version` on a release artifact and on a locally built binary agree.
#
# DF-CRIER-171 / the docs-claims gate: the two `go build` lines below carry their
# `-ldflags` stamp INLINE, spelled out, instead of through a variable. That is
# deliberate, not verbose for its own sake — cmd/server/docsclaims_test.go reads
# this file (via COUNT-BUILD-PATHS-STAMPED) and counts every `go build` line that
# produces a crier binary, FAILING the build when one of them does not stamp
# internal/buildinfo. A variable indirected stamp would not be seen, so the
# release artifacts would be the one build path in the repo that could silently
# ship without an identity. Keep the stamp on the `go build` line itself.
#
# Invoked by `make release-artifacts`, and by `make release` itself, which builds
# the asset set for the version it is about to tag.
#
# Env:
#   VERSION           version stamped into the binaries and reported by -version.
#                     Defaults to `git describe --tags --always --dirty`, the same
#                     default `make build` uses. `make release VERSION=x` passes
#                     the tag explicitly.
#   COMMIT            commit stamped into the binaries (default: git rev-parse HEAD)
#   BUILD_TIME        RFC 3339 build time (default: now, UTC)
#   RELEASE_TARGETS   space-separated GOOS/GOARCH pairs (default: the three the
#                     release publishes — linux/amd64 linux/arm64 darwin/arm64).
#                     Validated against a known-good set: an unbuildable target
#                     fails here, not halfway through a release.
#   RELEASE_OUTDIR    output directory (default: dist/ — gitignored)
#
# Exit status: 0 only when every target built, every checksum re-verified, and
# the host artifact ran and reported the stamped version.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
RELEASE_TARGETS="${RELEASE_TARGETS:-linux/amd64 linux/arm64 darwin/arm64}"
RELEASE_OUTDIR="${RELEASE_OUTDIR:-dist}"

# The install-half of CR-FEAT-028 ships as an artifact too, so `--version vX`
# can fetch the installer that belongs to that version instead of trusting the
# copy on a branch that has moved on.
INSTALL_SH="$SCRIPT_DIR/install.sh"

usage() {
  cat <<'EOF'
scripts/release-artifacts.sh — build the release asset set (CR-FEAT-028).

Usage: bash scripts/release-artifacts.sh

Cross-compiles crier and crier-mcp for every release target into RELEASE_OUTDIR
(default dist/), copies the installer in, writes SHA256SUMS and re-verifies it.

Env: VERSION, COMMIT, BUILD_TIME, RELEASE_TARGETS, RELEASE_OUTDIR.
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) echo "release-artifacts.sh: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
esac

fail() { echo "release-artifacts: FAIL: $*" >&2; exit 1; }

# ── Pre-flight ──────────────────────────────────────────────────────────────
command -v go >/dev/null 2>&1 || fail "'go' is required on PATH (this is the one step of the release flow that needs a toolchain)"
[ -f "$INSTALL_SH" ] || fail "$INSTALL_SH is missing — it ships inside the asset set, so the set cannot be built without it"

# The checksum file is part of the contract with scripts/install.sh, which
# FAILS CLOSED when it cannot verify a download. A build without a checksum tool
# would therefore produce an asset set nobody can install — refuse here instead.
if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL="shasum -a 256"
else
  fail "neither 'sha256sum' nor 'shasum' is on PATH — SHA256SUMS cannot be produced or re-verified (the installer refuses unverified downloads, so an asset set without it is unusable)"
fi

echo "==> crier release artifacts (CR-FEAT-028)"
echo "    version   : ${VERSION}"
echo "    commit    : ${COMMIT}"
echo "    build time: ${BUILD_TIME}"
echo "    targets   : ${RELEASE_TARGETS}"
echo "    outdir    : ${RELEASE_OUTDIR}"
echo "    go        : $(go version)"
echo "    sha256    : ${SHA_TOOL}"

mkdir -p "$RELEASE_OUTDIR"

# ── Cross-compile ───────────────────────────────────────────────────────────
# GOOS/GOARCH are validated against a known-good set FIRST so a typo
# ("linux/amd65") fails with the supported list instead of a Go error halfway
# through a release.
HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
esac
HOST_TARGET="${HOST_OS}/${HOST_ARCH}"
HOST_SERVER="$RELEASE_OUTDIR/crier_${HOST_OS}_${HOST_ARCH}"

BUILT_TARGETS=""
for target in $RELEASE_TARGETS; do
  os="${target%%/*}"
  arch="${target##*/}"
  case "$os" in
    linux|darwin) ;;
    *) fail "unsupported OS in RELEASE_TARGETS: '$os' (supported: linux darwin)" ;;
  esac
  case "$arch" in
    amd64|arm64) ;;
    *) fail "unsupported architecture in RELEASE_TARGETS: '$arch' (supported: amd64 arm64)" ;;
  esac

  SERVER_OUT="$RELEASE_OUTDIR/crier_${os}_${arch}"
  MCP_OUT="$RELEASE_OUTDIR/crier-mcp_${os}_${arch}"
  echo
  echo "==> ${target} -> $(basename "$SERVER_OUT"), $(basename "$MCP_OUT")"
  # The stamp is inline on purpose — see the DF-CRIER-171 note in the header.
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X github.com/crier-dev/crier/internal/buildinfo.Version=${VERSION} -X github.com/crier-dev/crier/internal/buildinfo.Commit=${COMMIT} -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=${BUILD_TIME}" -o "$SERVER_OUT" ./cmd/server
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X github.com/crier-dev/crier/internal/buildinfo.Version=${VERSION} -X github.com/crier-dev/crier/internal/buildinfo.Commit=${COMMIT} -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=${BUILD_TIME}" -o "$MCP_OUT" ./cmd/crier-mcp
  [ -s "$SERVER_OUT" ] || fail "$SERVER_OUT is empty"
  [ -s "$MCP_OUT" ] || fail "$MCP_OUT is empty"
  BUILT_TARGETS="${BUILT_TARGETS}${BUILT_TARGETS:+ }${target}"
done

# ── The install half ships with the binaries ────────────────────────────────
install -m 0755 "$INSTALL_SH" "$RELEASE_OUTDIR/install.sh"

# ── Checksums ───────────────────────────────────────────────────────────────
echo
echo "==> SHA256SUMS"
# A stable, sorted manifest: the installer looks a name up by its own filename,
# so only the names matter, but a deterministic file keeps a re-run diffable.
(
  cd "$RELEASE_OUTDIR"
  # A glob rather than `find -printf`, which is GNU-only: the toolchain that runs
  # this can be asked for a darwin target from anywhere.
  for name in *; do
    [ -f "$name" ] || continue
    [ "$name" = "SHA256SUMS" ] && continue
    # shellcheck disable=SC2086
    printf '%s  %s\n' "$($SHA_TOOL "$name" | awk '{print $1}')" "$name"
  done | LC_ALL=C sort > SHA256SUMS
)
# Verify what we just wrote, from the directory the checksums are relative to.
(
  cd "$RELEASE_OUTDIR"
  # shellcheck disable=SC2086
  $SHA_TOOL -c --status SHA256SUMS || fail "the SHA256SUMS file this script just wrote does not verify — refusing to leave a bad asset set on disk"
)
echo "    $(wc -l < "$RELEASE_OUTDIR/SHA256SUMS") file(s) checksummed and re-verified"

# ── Verify the artifacts ────────────────────────────────────────────────────
echo
echo "==> artifacts"
if command -v file >/dev/null 2>&1; then
  for f in "$RELEASE_OUTDIR"/crier_* "$RELEASE_OUTDIR"/crier-mcp_*; do
    [ -f "$f" ] || continue
    echo "    $(basename "$f"): $(wc -c < "$f" | awk '{printf "%.1f MB", $1/1048576}') — $(file -b "$f")"
  done
else
  echo "    (file(1) is not on PATH — per-artifact type check skipped, sizes still printed)"
  for f in "$RELEASE_OUTDIR"/crier_* "$RELEASE_OUTDIR"/crier-mcp_*; do
    [ -f "$f" ] || continue
    echo "    $(basename "$f"): $(wc -c < "$f" | awk '{printf "%.1f MB", $1/1048576}')"
  done
fi

# A static check, when the host is Linux and ldd can read it: the whole point of
# CGO_ENABLED=0 is that the artifact does not need the builder's libc.
if [ "$HOST_OS" = "linux" ] && command -v ldd >/dev/null 2>&1; then
  if [ -f "$HOST_SERVER" ]; then
    LDD_OUT="$(ldd "$HOST_SERVER" 2>&1 || true)"
    case "$LDD_OUT" in
      *"not a dynamic executable"*|*"statically linked"*) echo "    static   : $(basename "$HOST_SERVER") -> ${LDD_OUT}" ;;
      *) echo "    WARNING  : $(basename "$HOST_SERVER") is dynamically linked (${LDD_OUT}) — it will not run on a box without this builder's libc" >&2 ;;
    esac
  fi
fi

# ── Prove the host artifact runs and reports the stamped identity ───────────
# A cross-compiled artifact for another OS/arch cannot be executed here; the
# host artifact can, and `-version` is the cheapest way to prove the stamp
# landed AND that the binary runs at all. Skipped (LOUDLY) only when the host
# target is not part of the requested set.
echo
echo "==> identity of the host artifact"
HOST_SERVER="$RELEASE_OUTDIR/crier_${HOST_OS}_${HOST_ARCH}"
if [ -f "$HOST_SERVER" ]; then
  VERSION_LINE="$("$HOST_SERVER" -version 2>&1 | head -1)"
  echo "    ${VERSION_LINE}"
  case "$VERSION_LINE" in
    *"$VERSION"*) ;;
    *) fail "the host artifact does not report the stamped version: wanted '$VERSION', -version printed '${VERSION_LINE}'" ;;
  esac
else
  echo "    SKIP: the host target (${HOST_TARGET}) is not in RELEASE_TARGETS [${RELEASE_TARGETS}] — no artifact of this run can be executed here"
  [ -n "$BUILT_TARGETS" ] || fail "no target was built"
fi

echo
echo "release-artifacts: OK — $(wc -l < "$RELEASE_OUTDIR/SHA256SUMS") checksummed file(s) in ${RELEASE_OUTDIR}/ for ${BUILT_TARGETS}"
echo "release-artifacts: publish with 'make release-upload VERSION=${VERSION}' (never pushes a tag; see docs/releases.md)"
