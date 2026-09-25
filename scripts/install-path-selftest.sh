#!/usr/bin/env bash
#
# scripts/install-path-selftest.sh — CR-FEAT-028: the acceptance drive for the
# release front door, run against the REAL asset set the release publishes.
#
# WHAT THIS PROVES, end to end, on one host, with no Go toolchain in reach:
#
#   [1/7] two scratch ports are CHOSEN by the shared port selector (never
#         hard-coded), before anything is built
#   [2/7] the release asset set is BUILT for every release target by the same
#         script `make release-artifacts` runs — crier + crier-mcp per target,
#         the installer copied in, SHA256SUMS written and re-verified. (The
#         whole set is built, so a target that stops compiling fails here, not
#         at release time; only the HOST pair is installed below, because a
#         cross-compiled artifact cannot be executed on this box.)
#   [3/7] that set is published over HTTP on a scratch port — the same
#         /download/<tag>/<asset> and /latest/download/<asset> URL shapes
#         GitHub serves — laid out as `download/<tag>/`, `latest/download/`,
#         `tamper/download/<tag>/` and `nomanifest/download/<tag>/`, with the
#         pid holding the port asserted to be the pid this script started
#   [4/7] `install.sh` installs from it TWICE into a CLEAN BOX: a pinned tag
#         (`--version <tag>` -> /download/<tag>/) and the default latest
#         (/latest/download/). The clean box is `env -i` with its OWN HOME and
#         a PATH whose `go` is a SHIM that exits 127 and logs the call: the run
#         therefore proves the no-toolchain claim POSITIVELY (the shim was
#         never invoked) instead of assuming that a `go` merely absent from
#         PATH is what kept the installer working
#   [5/7] the INSTALLED crier RUNS: `-version` reports the tag's stamped
#         version, the server starts on a scratch port (own pid, asserted
#         owner of that port), answers /health and /version, registers an
#         agent and accepts a delivery into that agent's inbox
#   [6/7] the TWO negative controls: a TAMPERED artifact (one byte flipped in
#         the served crier binary) must be REFUSED naming the checksum, and an
#         artifact set whose SHA256SUMS has no entry for the binary must be
#         REFUSED as unverifiable. A front door that installs an unverified
#         download is worse than no front door, so both arms are load-bearing
#   [7/7] the PUBLISH wiring: `release-upload` (the command docs/releases.md now
#         points the release manager at) takes THIS asset set, verifies it, and
#         calls `gh release create` with every asset named — driven against a
#         `gh` PATH shim that records its argv, so the arm proves the wiring
#         without a network and without touching a published release. Its
#         `--dry-run` arm runs first and proves a dry run publishes NOTHING
#
# Requirements: go (to build the asset set), curl, sha256sum/shasum, python3
# (stdlib http.server), ss (iproute2, for the port guards), awk.
#
# Env:
#   INSTALL_SELFTEST_PORT_BASE        first candidate of the artifact-server rotation (default 18891)
#   INSTALL_SELFTEST_RELAY_PORT_BASE  first candidate of the relay rotation (default 18901)
#   INSTALL_SELFTEST_PORT_CANDIDATES  how many candidates may be tried (default 5)
#   INSTALL_SELFTEST_PORT             use THIS port for the artifact server; checked, never rotated
#   INSTALL_SELFTEST_RELAY_PORT       use THIS port for the installed server; checked, never rotated
#   INSTALL_SELFTEST_TARGETS          release targets to build (default: the three the release ships)
#   INSTALL_SELFTEST_VERSION          version stamped into the built artifacts (default v0.0.0-installtest)
#
# Exit status: 0 only when every arm passed. Fixtures live under one mktemp -d
# and every spawned process is killed on exit (see cleanup).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# The shared "the server we measure is the server we started" guards (QA-CRIER-9
# / QA-CRIER-10) — sourced, never re-implemented, because a local copy of a
# guard is a guard that drifts.
. "$REPO_ROOT/scripts/lib/port-guard.sh"
guard_loopback_off_proxy

INSTALL_SELFTEST_PORT_BASE="${INSTALL_SELFTEST_PORT_BASE:-18891}"
INSTALL_SELFTEST_PORT_CANDIDATES="${INSTALL_SELFTEST_PORT_CANDIDATES:-5}"
INSTALL_SELFTEST_RELAY_PORT_BASE="${INSTALL_SELFTEST_RELAY_PORT_BASE:-18901}"
INSTALL_SELFTEST_PORT="${INSTALL_SELFTEST_PORT:-}"
INSTALL_SELFTEST_RELAY_PORT="${INSTALL_SELFTEST_RELAY_PORT:-}"
INSTALL_SELFTEST_TARGETS="${INSTALL_SELFTEST_TARGETS:-linux/amd64 linux/arm64 darwin/arm64}"
SELFTEST_VERSION="${INSTALL_SELFTEST_VERSION:-v0.0.0-installtest}"

WORKDIR="$(mktemp -d)"
ARTIFACT_SERVER_PID=""
RELAY_PID=""

# Cleanup contract (scripts/check-demo-cleanup.sh): nothing this script spawns
# can outlive it, and the workdir (with the clean-box HOME and the built
# assets) goes with them.
cleanup() {
  [ -n "$ARTIFACT_SERVER_PID" ] && kill "$ARTIFACT_SERVER_PID" 2>/dev/null || true
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

usage() {
  cat <<'EOF'
scripts/install-path-selftest.sh — the CR-FEAT-028 acceptance drive.

Usage: bash scripts/install-path-selftest.sh

Builds the release asset set, serves it over a scratch port as a release tree,
and installs from it into a clean box with a `go` shim on PATH that would fail
loudly if the installer ever needed a toolchain — plus the tampered-artifact and
missing-manifest negative controls. See the header of this file for the six
steps.

Env: INSTALL_SELFTEST_PORT, INSTALL_SELFTEST_PORT_BASE, INSTALL_SELFTEST_PORT_CANDIDATES,
INSTALL_SELFTEST_RELAY_PORT, INSTALL_SELFTEST_RELAY_PORT_BASE, INSTALL_SELFTEST_TARGETS,
INSTALL_SELFTEST_VERSION=1.
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) echo "install-path-selftest.sh: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
esac

echo "==> crier release front door (CR-FEAT-028): asset set + curl-to-install, no Go toolchain"

# ── Pre-flight ──────────────────────────────────────────────────────────────
for tool in go curl python3 ss awk; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done
if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL="shasum -a 256"
else
  echo "FAIL: neither sha256sum nor shasum is on PATH" >&2
  exit 1
fi
echo "    python3 : $(python3 -V 2>&1)"
echo "    sha256  : ${SHA_TOOL}"

# ── [1/6] choose the scratch ports before anything is built ─────────────────
echo
echo "==> [1/7] select scratch ports"
select_scratch_port "${INSTALL_SELFTEST_PORT:-}" "$INSTALL_SELFTEST_PORT_BASE" "the artifact server" "$INSTALL_SELFTEST_PORT_CANDIDATES" "INSTALL_SELFTEST_PORT"
ARTIFACT_PORT="$PORT_GUARD_SELECTED"
select_scratch_port "${INSTALL_SELFTEST_RELAY_PORT:-}" "$INSTALL_SELFTEST_RELAY_PORT_BASE" "the installed crier server" "$INSTALL_SELFTEST_PORT_CANDIDATES" "INSTALL_SELFTEST_RELAY_PORT"
RELAY_PORT="$PORT_GUARD_SELECTED"
ARTIFACT_BASE="http://127.0.0.1:${ARTIFACT_PORT}"
RELAY_BASE="http://127.0.0.1:${RELAY_PORT}"
echo "    artifacts: ${ARTIFACT_BASE}   (the release tree, served locally)"
echo "    relay    : ${RELAY_BASE}   (the INSTALLED binary, once step 5 starts it)"

# ── [2/6] build the release asset set ───────────────────────────────────────
echo
echo "==> [2/7] build the release asset set (the same script make release runs)"
VERSION="$SELFTEST_VERSION" RELEASE_TARGETS="$INSTALL_SELFTEST_TARGETS" RELEASE_OUTDIR="$WORKDIR/dist" \
  bash "$SCRIPT_DIR/release-artifacts.sh" 2>&1 | sed 's/^/    /'
[ -f "$WORKDIR/dist/SHA256SUMS" ] || { echo "FAIL: no SHA256SUMS in the asset set" >&2; exit 1; }
echo "    SHA256SUMS:"; sed 's/^/      /' "$WORKDIR/dist/SHA256SUMS"

# Only the HOST pair can be EXECUTED here; the install arms use it.
HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
esac
HOST_SERVER_ASSET="crier_${HOST_OS}_${HOST_ARCH}"
HOST_MCP_ASSET="crier-mcp_${HOST_OS}_${HOST_ARCH}"
[ -s "$WORKDIR/dist/$HOST_SERVER_ASSET" ] || { echo "FAIL: the asset set has no $HOST_SERVER_ASSET (host target ${HOST_OS}/${HOST_ARCH} was not built)" >&2; exit 1; }
[ -s "$WORKDIR/dist/$HOST_MCP_ASSET" ] || { echo "FAIL: the asset set has no $HOST_MCP_ASSET" >&2; exit 1; }

# ── [3/6] serve it as a release tree ────────────────────────────────────────
echo
echo "==> [3/7] publish the asset set on :${ARTIFACT_PORT} as a release tree"
SERVE_ROOT="$WORKDIR/serve"
mkdir -p "$SERVE_ROOT/download/$SELFTEST_VERSION" "$SERVE_ROOT/latest/download" \
         "$SERVE_ROOT/tamper/download/$SELFTEST_VERSION" \
         "$SERVE_ROOT/nomanifest/download/$SELFTEST_VERSION"
cp "$WORKDIR/dist"/* "$SERVE_ROOT/download/$SELFTEST_VERSION/"
# GitHub's /releases/latest/download/<asset> is a redirect to the newest release's
# asset; a symlinked file reproduces the shape the installer's default path asks
# for, without a second copy of ~30 MB of binaries.
for f in "$SERVE_ROOT/download/$SELFTEST_VERSION"/*; do
  ln -s "$f" "$SERVE_ROOT/latest/download/$(basename "$f")"
done
cp "$WORKDIR/dist"/* "$SERVE_ROOT/tamper/download/$SELFTEST_VERSION/"
cp "$WORKDIR/dist"/* "$SERVE_ROOT/nomanifest/download/$SELFTEST_VERSION/"
# The tamper: ONE byte of the served server binary is flipped, so the artifact no
# longer matches the manifest the same directory publishes. The flip targets a
# byte near the end (inside the Go build info is fine — nothing executes it) and
# the manifest is deliberately left UNCHANGED, which is exactly the mirror-lying
# case the checksum exists for.
python3 - "$SERVE_ROOT/tamper/download/$SELFTEST_VERSION/$HOST_SERVER_ASSET" <<'PY'
import sys
path = sys.argv[1]
with open(path, 'r+b') as fh:
    fh.seek(-64, 2)
    b = fh.read(1)
    fh.seek(-64, 2)
    fh.write(bytes([b[0] ^ 0xFF]))
PY
# The no-manifest control: the manifest is present but has no line for the
# server binary, so verification has nothing to compare against.
grep -v " ${HOST_SERVER_ASSET}\$" "$WORKDIR/dist/SHA256SUMS" > "$SERVE_ROOT/nomanifest/download/$SELFTEST_VERSION/SHA256SUMS"
TAMPER_SUM="$("$SHA_TOOL" "$SERVE_ROOT/tamper/download/$SELFTEST_VERSION/$HOST_SERVER_ASSET" | awk '{print $1}')"
CLEAN_SUM="$("$SHA_TOOL" "$WORKDIR/dist/$HOST_SERVER_ASSET" | awk '{print $1}')"
if [ "$TAMPER_SUM" = "$CLEAN_SUM" ]; then
  echo "FAIL: the tamper control did not change the served binary's digest (${TAMPER_SUM})" >&2
  exit 1
fi
if grep -q " ${HOST_SERVER_ASSET}\$" "$SERVE_ROOT/nomanifest/download/$SELFTEST_VERSION/SHA256SUMS"; then
  echo "FAIL: the no-manifest control still carries a $HOST_SERVER_ASSET entry" >&2
  exit 1
fi
echo "    tamper   : $HOST_SERVER_ASSET digest ${CLEAN_SUM:0:16}… -> ${TAMPER_SUM:0:16}… (manifest unchanged)"
echo "    served   : download/$SELFTEST_VERSION/ + latest/download/ (symlinked assets)"

python3 -m http.server "$ARTIFACT_PORT" --bind 127.0.0.1 --directory "$SERVE_ROOT" > "$WORKDIR/artifact-server.log" 2>&1 &
ARTIFACT_SERVER_PID=$!
wait_http_or_die "${ARTIFACT_BASE}/download/$SELFTEST_VERSION/SHA256SUMS" "$ARTIFACT_SERVER_PID" "$WORKDIR/artifact-server.log" "the artifact server"
assert_port_owned "$ARTIFACT_PORT" "$ARTIFACT_SERVER_PID" "the artifact server"
echo "    the pid holding :${ARTIFACT_PORT} is the pid this script started (${ARTIFACT_SERVER_PID})"

# ── [4/6] install into a clean box, with a go shim on PATH ──────────────────
echo
echo "==> [4/7] install from the served release tree into a clean box"
CLEAN_HOME="$WORKDIR/clean-home"
SHIM_DIR="$WORKDIR/clean-path"
SHIM_LOG="$SHIM_DIR/go-invocations.log"
mkdir -p "$CLEAN_HOME" "$SHIM_DIR"
# The shim is the positive form of "no Go toolchain": the installer runs with a
# `go` on PATH, and it is a program that FAILS. If any step of the install path
# reaches for a toolchain, the shim records the call and this script fails with
# the log — an absent `go` would have proven only that PATH did not carry one.
cat > "$SHIM_DIR/go" <<'SHIM'
#!/bin/sh
# CR-FEAT-028: a failing `go` shim — the install path must never invoke it.
log="$(dirname "$0")/go-invocations.log"
echo "go $*" >> "$log"
echo "go: the release install path must not need a Go toolchain (invoked as: go $*)" >&2
exit 127
SHIM
chmod 0755 "$SHIM_DIR/go"
CLEAN_PATH="$SHIM_DIR:/usr/bin:/bin"
CLEAN_BIN="$CLEAN_HOME/.local/bin"
echo "    HOME     : ${CLEAN_HOME}"
echo "    PATH     : ${CLEAN_PATH}   (go resolves to the failing shim)"
echo "    go on the clean PATH: $(env -i PATH="$CLEAN_PATH" sh -c 'command -v go')   <- the shim, not a toolchain"

INSTALL_SH="$WORKDIR/dist/install.sh"

# clean_install <label> <base-url> <installer args…> — run the installer in the
# clean box, print its transcript indented, abort the drive when it exits nonzero.
clean_install() {
  local label="$1" base="$2"
  shift 2
  local out rc
  set +e
  out="$(env -i PATH="$CLEAN_PATH" HOME="$CLEAN_HOME" CRIER_BASE_URL="$base" sh "$INSTALL_SH" "$@" 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "$out" | sed 's/^/    | /'
  if [ "$rc" -ne 0 ]; then
    echo "    FAIL: ${label} exited ${rc}" >&2
    exit 1
  fi
  echo "    PASS: ${label}"
}

# clean_install_expect_failure <label> <needle> <base-url> <installer args…> —
# the negative arm: the installer MUST exit nonzero AND say why, so "it failed
# for the wrong reason" is never counted as a refusal.
clean_install_expect_failure() {
  local label="$1" needle="$2" base="$3"
  shift 3
  local out rc
  set +e
  out="$(env -i PATH="$CLEAN_PATH" HOME="$CLEAN_HOME" CRIER_BASE_URL="$base" sh "$INSTALL_SH" "$@" 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "$out" | sed 's/^/    | /'
  if [ "$rc" -eq 0 ]; then
    echo "    FAIL: ${label} SUCCEEDED — it must be refused" >&2
    exit 1
  fi
  if ! printf '%s' "$out" | grep -q -- "$needle"; then
    echo "    FAIL: ${label} was refused, but the output does not name '${needle}'" >&2
    exit 1
  fi
  echo "    PASS: ${label} refused (exit ${rc}) naming '${needle}'"
}

clean_install "pinned install (--version ${SELFTEST_VERSION})" "$ARTIFACT_BASE" --version "$SELFTEST_VERSION" --dir "$CLEAN_BIN"
[ -x "$CLEAN_BIN/crier" ] || { echo "FAIL: no executable at ${CLEAN_BIN}/crier" >&2; exit 1; }
[ -x "$CLEAN_BIN/crier-mcp" ] || { echo "FAIL: no executable at ${CLEAN_BIN}/crier-mcp" >&2; exit 1; }

# The default path — no --version at all — must resolve /latest/download/.
rm -f "$CLEAN_BIN/crier" "$CLEAN_BIN/crier-mcp"
clean_install "default install (latest, no --version)" "$ARTIFACT_BASE" --dir "$CLEAN_BIN"
[ -x "$CLEAN_BIN/crier" ] || { echo "FAIL: latest install left no ${CLEAN_BIN}/crier" >&2; exit 1; }

# ── [5/6] the installed artifact runs ───────────────────────────────────────
echo
echo "==> [5/7] run the INSTALLED crier (no toolchain, no build)"
VERSION_LINE="$(env -i PATH="$CLEAN_PATH" HOME="$CLEAN_HOME" "$CLEAN_BIN/crier" -version 2>&1 | head -1)"
echo "    ${CLEAN_BIN}/crier -version -> ${VERSION_LINE}"
case "$VERSION_LINE" in
  *"$SELFTEST_VERSION"*) ;;
  *) echo "FAIL: the installed binary does not report the stamped version '${SELFTEST_VERSION}': '${VERSION_LINE}'" >&2; exit 1 ;;
esac

# The relay is started from the FULL installed path literal, on ONE line (rather
# than through a variable or a backslash continuation), so the repo's server-spawn
# arm (scripts/check-demo-cleanup.sh) classifies this script and checks its cleanup
# contract — a trap that kills the pid it started, and an assertion that the
# port's holder IS that pid.
env -i PATH="$CLEAN_PATH" HOME="$CLEAN_HOME" CRIER_PORT="$RELAY_PORT" CR_GUARD_ENABLED=false CR_LOG_LEVEL=warn "$CLEAN_HOME/.local/bin/crier" > "$WORKDIR/relay.log" 2>&1 &
RELAY_PID=$!
wait_http_or_die "${RELAY_BASE}/health" "$RELAY_PID" "$WORKDIR/relay.log" "the installed crier server"
assert_port_owned "$RELAY_PORT" "$RELAY_PID" "the installed crier server"
echo "    the pid holding :${RELAY_PORT} is the pid this script started (${RELAY_PID})"

HEALTH="$(curl -fsS "${RELAY_BASE}/health")"
echo "    GET /health  -> ${HEALTH}"
printf '%s' "$HEALTH" | grep -q '"status":"ok"' || { echo "FAIL: /health did not answer status ok" >&2; exit 1; }

RUN_VERSION="$(curl -fsS "${RELAY_BASE}/version")"
echo "    GET /version -> ${RUN_VERSION}"
# `/version` reports the version SEGMENT (the `v` is a rendering detail of the
# CLI's `v<version>-<commit>` line, not part of the stored version).
VERSION_SEGMENT="${SELFTEST_VERSION#v}"
printf '%s' "$RUN_VERSION" | grep -q "$VERSION_SEGMENT" || { echo "FAIL: the running server does not report the stamped version '${VERSION_SEGMENT}': ${RUN_VERSION}" >&2; exit 1; }

REG_STATUS="$(curl -s -o "$WORKDIR/register.json" -w '%{http_code}' -X POST "${RELAY_BASE}/agents" \
  -H 'Content-Type: application/json' \
  -d '{"id":"installed-probe","public_key":"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d5e6f708192a3b4c5d6e7f809"}')"
echo "    POST /agents -> ${REG_STATUS} $(cat "$WORKDIR/register.json")"
[ "$REG_STATUS" = "201" ] || { echo "FAIL: register answered ${REG_STATUS}, wanted 201" >&2; exit 1; }

DELIVER_STATUS="$(curl -s -o "$WORKDIR/deliver.json" -w '%{http_code}' -X POST "${RELAY_BASE}/agents/installed-probe/inbox" \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"from-the-installed-binary"}}')"
echo "    POST /agents/installed-probe/inbox -> ${DELIVER_STATUS} $(cat "$WORKDIR/deliver.json")"
[ "$DELIVER_STATUS" = "201" ] || { echo "FAIL: delivery answered ${DELIVER_STATUS}, wanted 201 (durable inbox)" >&2; exit 1; }

# ── [6/6] the negative controls ─────────────────────────────────────────────
echo
echo "==> [6/7] negative controls (an unverified download must never install)"
clean_install_expect_failure "tampered artifact (one byte flipped in $HOST_SERVER_ASSET)" \
  "checksum mismatch for $HOST_SERVER_ASSET" "$ARTIFACT_BASE/tamper" --version "$SELFTEST_VERSION" --dir "$CLEAN_HOME/tamper-bin"
# The clean-box HOME just received a verified install in [4/7]; the refusal must
# not have replaced it with the tampered file.
[ -z "$(ls -A "$CLEAN_HOME/tamper-bin" 2>/dev/null)" ] || { echo "FAIL: the tampered install left files in ${CLEAN_HOME}/tamper-bin" >&2; exit 1; }

clean_install_expect_failure "manifest with no entry for $HOST_SERVER_ASSET" \
  "carries no entry for '$HOST_SERVER_ASSET'" "$ARTIFACT_BASE/nomanifest" --version "$SELFTEST_VERSION" --dir "$CLEAN_HOME/nomanifest-bin"

# ── [7/7] the publish wiring ────────────────────────────────────────────────
# The other half of CR-FEAT-028 is that the assets REACH the Release object. That
# is a `gh` call, and the honest way to prove it here is a `gh` PATH shim that
# records its argv: no network, no authentication, and no published release is
# touched. The shim answers `release view` with "no such release" so the upload
# takes the CREATE path — the shape that carries the assets on the command line.
echo
echo "==> [7/7] publish wiring: release-upload attaches THIS asset set (gh is a shim)"
GH_SHIM_DIR="$WORKDIR/gh-path"
GH_LOG="$GH_SHIM_DIR/gh-invocations.log"
mkdir -p "$GH_SHIM_DIR"
cat > "$GH_SHIM_DIR/gh" <<'SHIM'
#!/bin/sh
# CR-FEAT-028 test double: records every invocation, claims no Release object
# exists (so the upload takes the create path), and succeeds at everything else.
log="$(dirname "$0")/gh-invocations.log"
echo "gh $*" >> "$log"
if [ "${1:-} ${2:-}" = "release view" ]; then
  exit 1
fi
exit 0
SHIM
chmod 0755 "$GH_SHIM_DIR/gh"
printf 'CR-FEAT-028 selftest release notes\n' > "$WORKDIR/notes.md"

run_release_upload() { # <label> <args…>
  local label="$1"
  shift
  local out rc
  set +e
  out="$(PATH="$GH_SHIM_DIR:$PATH" VERSION="$SELFTEST_VERSION" RELEASE_OUTDIR="$WORKDIR/dist" \
    bash "$SCRIPT_DIR/release-upload.sh" "$@" 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "$out" | sed 's/^/    | /'
  [ "$rc" -eq 0 ] || { echo "    FAIL: ${label} exited ${rc}" >&2; exit 1; }
  echo "    PASS: ${label}"
}

run_release_upload "dry run (must publish nothing)" --dry-run --notes-file "$WORKDIR/notes.md"
# The dry run may only READ: the probe that decides create-vs-upload is a `gh
# release view`, so nothing else may appear in the shim's log.
if grep -qv '^gh release view ' "$GH_LOG" 2>/dev/null; then
  echo "    FAIL: --dry-run made a mutating gh call:" >&2
  sed 's/^/      /' "$GH_LOG" >&2
  exit 1
fi
echo "    PASS: --dry-run ran no gh call that changes anything ($(wc -l < "$GH_LOG") read-only call(s))"

run_release_upload "publish (gh shim records the command)" --notes-file "$WORKDIR/notes.md"
grep -q -- "release create $SELFTEST_VERSION" "$GH_LOG" \
  || { echo "FAIL: release-upload never asked gh to create the Release for $SELFTEST_VERSION:" >&2; sed 's/^/      /' "$GH_LOG" >&2; exit 1; }
grep -q -- "--repo crier-dev/crier" "$GH_LOG" \
  || { echo "FAIL: the gh invocation names no --repo" >&2; sed 's/^/      /' "$GH_LOG" >&2; exit 1; }
# EVERY asset named by the set must be on the command line: the release that
# attaches the server but not the MCP bridge (or vice versa) is the exact defect
# this task exists for, and nothing else would notice a partial set.
UPLOAD_LINE="$(grep -- "release create $SELFTEST_VERSION" "$GH_LOG")"
echo "    gh argv : ${UPLOAD_LINE}"
for asset in $(awk '{print $2}' "$WORKDIR/dist/SHA256SUMS"); do
  case "$UPLOAD_LINE" in
    *"$asset"*) ;;
    *) echo "FAIL: the publish command does not carry the asset '${asset}'" >&2; exit 1 ;;
  esac
done
echo "    PASS: every asset in SHA256SUMS ($(wc -l < "$WORKDIR/dist/SHA256SUMS") file(s)) is on the publish command line"

# The no-toolchain proof, positively: the shim fails loudly and logs, and every
# arm above ran with it on PATH.
if [ -f "$SHIM_LOG" ]; then
  echo "    FAIL: the install path invoked a Go toolchain:" >&2
  sed 's/^/      /' "$SHIM_LOG" >&2
  exit 1
fi
echo "    PASS: the go shim (a program that exits 127) was never invoked by any arm"

echo
echo "install-path-selftest: PASS — the release asset set installs and runs on a box with no Go toolchain,"
echo "                       both unverified-download shapes are refused by name, and the publish command"
echo "                       carries every asset of the set"
