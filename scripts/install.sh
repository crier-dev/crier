#!/bin/sh
#
# scripts/install.sh — CR-FEAT-028: the release front door, INSTALL half.
#
# WHY THIS EXISTS
# ---------------
# `gh release view v0.1.0-rc2` returned `assets: []`: the only way into crier was
# `git clone` + a Go toolchain + `make build`. An external hands-on review
# (DISPATCH · CRI-001, Carter, 2026-09-25) filed that as a tester-funnel problem
# — "a curl-to-install or published images would widen the tester funnel".
#
# This is the curl-to-install path. It fetches the two shipped binaries for THIS
# box's OS and architecture from the release assets, verifies each one against the
# release's own SHA256SUMS, and installs them into ~/.local/bin — no Go
# toolchain, no build, nothing to compile.
#
#     curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh
#
# The script is POSIX sh on purpose: it is piped into whatever `sh` the box has.
# It needs only uname, a checksum tool (sha256sum, or shasum on macOS) and curl
# or wget. Anything missing is a LOUD failure, never a silent skip — and there
# is deliberately NO flag to skip checksum verification: an unverified download
# installed as a binary is how a mirror becomes a supply chain.
#
# Asset names carry the PLATFORM but not the version (`crier_linux_amd64`), so
# `releases/latest/download/<name>` resolves without an API call and without a
# JSON parser; a pinned install swaps one URL segment for the tag. The version a
# binary carries is answered by the binary itself: `<dir>/crier -version`.
#
# Usage:
#   install.sh                       install the latest release into ~/.local/bin
#   install.sh --version v0.1.0-rc3  install that tag instead
#   install.sh --dir <path>          install somewhere else
#   install.sh --base-url <url>      fetch from a mirror / a local directory server
#   install.sh --help
#
# Env equivalents: CRIER_VERSION, CRIER_INSTALL_DIR, CRIER_BASE_URL.
#
# Exit status: 0 only when both binaries were downloaded, checksum-verified,
# installed and an installed binary ran and reported its identity.
set -eu

REPO="crier-dev/crier"
BASE_URL="${CRIER_BASE_URL:-https://github.com/${REPO}/releases}"
VERSION="${CRIER_VERSION:-latest}"
INSTALL_DIR="${CRIER_INSTALL_DIR:-${HOME:-}/.local/bin}"

usage() {
  cat <<'EOF'
scripts/install.sh — install prebuilt crier + crier-mcp binaries (CR-FEAT-028).

Usage: sh install.sh [--version <vX.Y.Z[-rcN]>] [--dir <path>] [--base-url <url>]

  --version <v>    release tag to install (default: latest)
  --dir <path>     install directory (default: $HOME/.local/bin)
  --base-url <url> release base URL (default: https://github.com/crier-dev/crier/releases)
  --help           this text

Env: CRIER_VERSION, CRIER_INSTALL_DIR, CRIER_BASE_URL.
Installs both `crier` (server + keygen) and `crier-mcp` (MCP bridge), verified
against the release's SHA256SUMS. No Go toolchain, no build, no root.
EOF
}

WHILE_ARGS=1
while [ "$WHILE_ARGS" = "1" ]; do
  case "${1:-}" in
    --version) [ "${2:-}" ] || { echo "install.sh: --version needs a value" >&2; exit 2; }; VERSION="$2"; shift 2 ;;
    --dir)     [ "${2:-}" ] || { echo "install.sh: --dir needs a value" >&2; exit 2; }; INSTALL_DIR="$2"; shift 2 ;;
    --base-url) [ "${2:-}" ] || { echo "install.sh: --base-url needs a value" >&2; exit 2; }; BASE_URL="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    "") WHILE_ARGS=0 ;;
    *) echo "install.sh: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "install.sh: FAIL: $*" >&2; exit 1; }
note() { echo "    $*"; }

# ── Platform detection ──────────────────────────────────────────────────────
[ -n "${HOME:-}" ] || fail "HOME is not set — pass --dir <path> to choose an install directory"

OS_RAW="$(uname -s 2>/dev/null || echo unknown)"
ARCH_RAW="$(uname -m 2>/dev/null || echo unknown)"
case "$OS_RAW" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) fail "unsupported operating system '$OS_RAW' — this release ships binaries for linux and darwin only (build from source instead: see README.md 'Build from source')" ;;
esac
case "$ARCH_RAW" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture '$ARCH_RAW' on $OS_RAW — this release ships amd64 and arm64 only (build from source instead: see README.md 'Build from source')" ;;
esac

# ── Fetch tooling ───────────────────────────────────────────────────────────
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
  FETCH_TOOL="curl $(curl --version 2>/dev/null | head -1 | awk '{print $2}')"
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  FETCH_TOOL="wget"
else
  fail "neither 'curl' nor 'wget' is on PATH — nothing can be downloaded"
fi

# Checksum tool. Both FOUND and MISSING are decided here: the verification below
# is not optional, so a box with neither tool is refused up front rather than
# installing an unverified binary.
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
  SHA_TOOL="shasum -a 256"
else
  fail "neither 'sha256sum' nor 'shasum' is on PATH — the download cannot be verified against the release's SHA256SUMS, and an unverified download is not installed"
fi

# ── URL construction ────────────────────────────────────────────────────────
# `latest` uses GitHub's /releases/latest/download/ redirect (no API call, no
# JSON parsing, no rate limit); a pinned version uses /releases/download/<tag>/.
# A release whose newest tag is marked PRERELEASE is not "latest" — pin it with
# --version in that case, which is why the failure message names the remedy.
asset_url() {
  if [ "$VERSION" = "latest" ]; then
    printf '%s/latest/download/%s' "$BASE_URL" "$1"
  else
    printf '%s/download/%s/%s' "$BASE_URL" "$VERSION" "$1"
  fi
}

SERVER_ASSET="crier_${OS}_${ARCH}"
MCP_ASSET="crier-mcp_${OS}_${ARCH}"
CHECKSUM_ASSET="SHA256SUMS"

echo "==> crier ${VERSION} installer (CR-FEAT-028) — no Go toolchain needed"
note "platform : ${OS}/${ARCH} (uname: ${OS_RAW}/${ARCH_RAW})"
note "source   : ${BASE_URL}"
note "fetch    : ${FETCH_TOOL}"
note "checksum : ${SHA_TOOL}"
note "install  : ${INSTALL_DIR}"

WORKDIR="$(mktemp -d 2>/dev/null || mktemp -d -t crier-install)" || fail "cannot create a temporary directory"
# The workdir goes away on every exit path, including a failed download: an
# interrupted install leaves no half-fetched binary behind for the next run (or
# a human) to mistake for a real one.
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT INT TERM

# ── Download ────────────────────────────────────────────────────────────────
echo
echo "==> download"
for asset in "$CHECKSUM_ASSET" "$SERVER_ASSET" "$MCP_ASSET"; do
  url="$(asset_url "$asset")"
  if ! fetch "$url" "$WORKDIR/$asset"; then
    fail "download failed: ${url}
       (a 404 here means the tag has no such asset, or the release is a
       prerelease and so is not 'latest' — pass --version <tag> explicitly;
       see https://github.com/${REPO}/releases)"
  fi
  [ -s "$WORKDIR/$asset" ] || fail "download produced an empty file: ${url}"
  note "$(printf '%-28s' "$asset") <- ${url}"
done

# ── Verify ──────────────────────────────────────────────────────────────────
echo
echo "==> verify against ${CHECKSUM_ASSET}"
for asset in "$SERVER_ASSET" "$MCP_ASSET"; do
  # The manifest is `sha256  name` (sha256sum -c format, with an optional `*`
  # binary marker). The expected digest is looked up BY NAME, and a manifest
  # with no line for this asset is a failure — never "nothing to compare, carry
  # on".
  expected="$(awk -v n="$asset" '{ f=$2; sub(/^\*/, "", f); if (f == n) { print $1; exit } }' "$WORKDIR/$CHECKSUM_ASSET")"
  [ -n "$expected" ] || fail "${CHECKSUM_ASSET} carries no entry for '${asset}' — refusing an unverifiable download"
  actual="$(sha256_of "$WORKDIR/$asset")"
  if [ "$expected" != "$actual" ]; then
    fail "checksum mismatch for ${asset}:
       expected ${expected}
       actual   ${actual}
       the download does not match the release manifest — nothing was installed"
  fi
  note "$(printf '%-28s' "$asset") sha256 ${actual} (matches ${CHECKSUM_ASSET})"
done

# ── Install ─────────────────────────────────────────────────────────────────
echo
echo "==> install"
mkdir -p "$INSTALL_DIR" || fail "cannot create ${INSTALL_DIR}"
for pair in "$SERVER_ASSET:crier" "$MCP_ASSET:crier-mcp"; do
  asset="${pair%%:*}"
  name="${pair##*:}"
  # install(1) is not guaranteed on every box (busybox has it, a minimal
  # container may not), so the fallback is an explicit copy + mode.
  if command -v install >/dev/null 2>&1; then
    install -m 0755 "$WORKDIR/$asset" "$INSTALL_DIR/$name"
  else
    cp "$WORKDIR/$asset" "$INSTALL_DIR/$name" || fail "cannot write ${INSTALL_DIR}/${name}"
    chmod 0755 "$INSTALL_DIR/$name"
  fi
  [ -x "$INSTALL_DIR/$name" ] || fail "${INSTALL_DIR}/${name} is not executable after install"
  note "$(printf '%-12s' "$name") -> ${INSTALL_DIR}/${name}"
done

# ── Prove the installed binary runs ─────────────────────────────────────────
# A cross-compiled binary for the wrong platform is the one failure a checksum
# cannot catch (the file is exactly the file the release published). Running
# `-version` is the artifact's own answer, and it also prints the version stamp
# the release was cut with.
echo
echo "==> run"
VERSION_LINE="$("$INSTALL_DIR/crier" -version 2>&1 | head -1)" || fail "the installed binary does not run: ${INSTALL_DIR}/crier -version failed"
[ -n "$VERSION_LINE" ] || fail "the installed binary printed nothing for -version"
note "${VERSION_LINE}"
"$INSTALL_DIR/crier-mcp" --version >/dev/null 2>&1 || fail "the installed MCP bridge does not run: ${INSTALL_DIR}/crier-mcp --version failed"
note "$("$INSTALL_DIR/crier-mcp" --version 2>&1 | head -1)"

echo
echo "install.sh: OK — crier and crier-mcp are installed in ${INSTALL_DIR}"
case ":${PATH:-}:" in
  *":${INSTALL_DIR}:"*) note "start it with: crier -port 8767" ;;
  *)
    note "NOTE: ${INSTALL_DIR} is not on your PATH. Add it, then start the server:"
    note "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    note "    crier -port 8767"
    ;;
esac
note "docs: https://github.com/${REPO}/blob/main/TESTERS.md (testers), README.md (reference)"
