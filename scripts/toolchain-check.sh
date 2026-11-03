#!/usr/bin/env bash
#
# scripts/toolchain-check.sh — this repository's TOOLCHAIN FLOOR gate (DF-CRIER-496).
#
# WHY THIS EXISTS
# ---------------
# go.mod's `go` directive is a HARD requirement: a toolchain older than it cannot
# build this module. With the default GOTOOLCHAIN=auto that is INVISIBLE on a
# networked box, because Go silently downloads the required toolchain — and it
# fails closed only where the silent download cannot happen: an offline host, a
# pinned GOTOOLCHAIN=local, a throwaway HOME. Measured on a fresh bunker-las-03
# agent (2026-11-03, tree aee2d9e2) the provisioning bootstrap installed go
# 1.26.5 while go.mod had already been raised to `go 1.26.10`, so the
# chaos-disconnect cell — which applies the documented cut (GOPROXY=off,
# GOTOOLCHAIN=local) — ended with the opaque
#
#     go: go.mod requires go >= 1.26.10 (running go 1.26.5; GOTOOLCHAIN=local)
#
# and nothing in the repo could say so BEFORE the build: there was no make
# target, no CI step and no documented floor. This script is that arm. It reads
# the floor, reads the toolchain that would actually run the build, and says
# which one is short, by how much, and what to install.
#
# THE FLOOR IS DERIVED, NEVER DUPLICATED
# --------------------------------------
# There is no second hand-maintained constant that can drift: the floor is READ
# from go.mod's `go <version>` directive (and, when the file also carries a
# `toolchain <version>` directive, from that one too — Go treats it as the exact
# toolchain to use, so the floor is whichever is higher). Raising the floor is a
# one-line edit in go.mod and this gate follows it.
#
# WHAT "RUNNING" MEANS — THE TRAP THIS GATE EXISTS FOR
# ---------------------------------------------------
# The version judged here is the INSTALLED toolchain, probed with
# GOTOOLCHAIN=local, NOT whatever `go version` prints in a module directory.
# Under the default GOTOOLCHAIN=auto, `go version` reports the toolchain Go
# SELECTED — possibly one it just downloaded — so on a machine whose bootstrap
# installed a toolchain below the floor it prints a healthy version and the
# shortfall only appears on an offline or pinned host. Measured on the very box
# this row was filed from: `go version` -> go1.26.10 while
# `GOTOOLCHAIN=local go version` -> go1.26.5, because go1.26.10 had been fetched
# into the module cache. Judging the auto answer would make this gate useless.
# An explicit `GOTOOLCHAIN=<version>`/path pin is honoured, since that is what
# the build would really use.
#
# USAGE
# -----
#   make toolchain-check                        # the operator/gate surface
#   bash scripts/toolchain-check.sh             # the same, without make
#   make toolchain-selftest                     # the regression that keeps it honest
#
# EXIT CODES
#   0  the running toolchain is at or above the go.mod floor (ONE OK line)
#   1  the running toolchain is BELOW the floor (actionable FAIL block)
#   2  REFUSED: no `go` on PATH, no/unreadable go.mod, no `go` directive in it, or
#      a version string this script cannot parse. It never reports OK over
#      something it could not read.
#
# TEST-ONLY OVERRIDES (used by scripts/toolchain-check-selftest.sh; not an
# operator surface — unset them for real runs):
#   TOOLCHAIN_CHECK_GOMOD=<path>   read that module file instead of the repo's go.mod
#   TOOLCHAIN_CHECK_GO=<path>      interrogate that binary instead of `go` on PATH
#
# This script downloads nothing, switches no toolchain and mutates nothing: its
# whole job is to be an honest, cheap verdict before a build that cannot work.
set -euo pipefail

# The below-floor verdict lives in ONE variable so the selftest can NEUTER it
# (force it to 0) and prove its own rejection arms are load-bearing — a
# forced-success copy must stop rejecting a short toolchain. Do not inline it.
BELOW_FLOOR_RC=1
REFUSED_RC=2

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

# parse_version RAW -> sets PV_MAJOR/PV_MINOR/PV_PATCH.
# Returns 1 when RAW carries no leading major version at all. Accepts the shapes
# the toolchain really produces: "go1.26.10", "1.26.10", "go1.27-rc1",
# "go1.28-1a2b3c4d" (a devel stamp) — everything from the first character that
# is neither a digit nor a dot is dropped, so a two-part floor ("go 1.26") is
# read as 1.26.0 rather than refused.
parse_version() {
    local raw=${1-} rest
    raw=${raw#go}
    rest=${raw%%[!0-9.]*}
    rest=${rest%.}
    if [ -z "$rest" ]; then
        return 1
    fi
    IFS=. read -r PV_MAJOR PV_MINOR PV_PATCH _ <<<"$rest"
    PV_MAJOR=${PV_MAJOR:-0}
    PV_MINOR=${PV_MINOR:-0}
    PV_PATCH=${PV_PATCH:-0}
    return 0
}

# cmp3 A B -> prints -1, 0 or 1 for the integer pair.
cmp3() {
    if [ "$1" -lt "$2" ]; then
        printf '%s' -1
    elif [ "$1" -gt "$2" ]; then
        printf '%s' 1
    else
        printf '%s' 0
    fi
}

# ver_cmp M1 m1 p1 M2 m2 p2 -> prints -1 (first is lower), 0 (equal) or 1.
ver_cmp() {
    local result
    result=$(cmp3 "$1" "$4")
    if [ "$result" != 0 ]; then
        printf '%s' "$result"
        return 0
    fi
    result=$(cmp3 "$2" "$5")
    if [ "$result" != 0 ]; then
        printf '%s' "$result"
        return 0
    fi
    cmp3 "$3" "$6"
}

# refuse MESSAGE — a check that could not read its inputs is never a PASS.
refuse() {
    printf 'toolchain-check: REFUSED — %s\n' "$1" >&2
    printf 'toolchain-check: refusing to report OK over input this check could not read.\n' >&2
    exit "$REFUSED_RC"
}

# ---------------------------------------------------------------------------
# 1. the floor, read out of go.mod
# ---------------------------------------------------------------------------

gomod=${TOOLCHAIN_CHECK_GOMOD:-$repo_root/go.mod}
go_bin=${TOOLCHAIN_CHECK_GO:-go}

[ -f "$gomod" ] || refuse "cannot read the module file: $gomod"

floor_raw=$(awk '$1 == "go" && NF >= 2 { print $2; exit }' "$gomod")
[ -n "$floor_raw" ] || refuse "no \`go\` directive found in $gomod"
parse_version "$floor_raw" || refuse "cannot parse the \`go\` directive in $gomod: '$floor_raw'"
FLOOR_MAJOR=$PV_MAJOR
FLOOR_MINOR=$PV_MINOR
FLOOR_PATCH=$PV_PATCH
floor_source="go.mod \`go $floor_raw\`"

# Go treats a `toolchain` directive as the exact toolchain to use; when the file
# has one it can only RAISE the requirement, never lower it.
toolchain_raw=$(awk '$1 == "toolchain" && NF >= 2 { print $2; exit }' "$gomod")
if [ -n "$toolchain_raw" ] && parse_version "$toolchain_raw"; then
    if [ "$(ver_cmp "$PV_MAJOR" "$PV_MINOR" "$PV_PATCH" "$FLOOR_MAJOR" "$FLOOR_MINOR" "$FLOOR_PATCH")" = 1 ]; then
        FLOOR_MAJOR=$PV_MAJOR
        FLOOR_MINOR=$PV_MINOR
        FLOOR_PATCH=$PV_PATCH
        floor_source="go.mod \`toolchain $toolchain_raw\`"
    fi
fi
floor_ver="$FLOOR_MAJOR.$FLOOR_MINOR.$FLOOR_PATCH"

# ---------------------------------------------------------------------------
# 2. the toolchain that would actually run the build
# ---------------------------------------------------------------------------
#
# MEASURED TRAP (this is the whole row): under GOTOOLCHAIN=auto, `go version`
# reports the toolchain Go SELECTED — which may be one it just downloaded — not
# the one installed on the machine. On this very host, in this repository:
#
#     $ go version                            -> go version go1.26.10 linux/amd64
#     $ GOTOOLCHAIN=local go version           -> go version go1.26.5  linux/amd64
#     $ go env GOROOT   -> /var/tmp/hermes-go-mod/golang.org/toolchain@...go1.26.10...
#
# The installed toolchain is go1.26.5 (~/.local/bin/go -> ~/sdk/go1.26.5); the
# 1.26.10 came out of the module cache. An offline, pinned or freshly
# provisioned host runs EXACTLY the `local` answer, so a gate that asked the
# plain `go version` question would have printed OK on the very machine this row
# recorded — a false negative. The probe therefore PINS the switch off, and only
# defers to GOTOOLCHAIN when the caller pinned it to something explicit (a
# `GOTOOLCHAIN=go1.26.10`/path setting is what the build would really use).

go_path=$(command -v "$go_bin" 2>/dev/null || true)
[ -n "$go_path" ] || refuse "no \`go\` binary on PATH (or at TOOLCHAIN_CHECK_GO='$go_bin')"

# Best-effort context, never part of the verdict.
ambient_gotoolchain=$("$go_bin" env GOTOOLCHAIN 2>/dev/null || true)
[ -n "$ambient_gotoolchain" ] || ambient_gotoolchain=unknown

case "$ambient_gotoolchain" in
    auto | local | unknown) probe_gotoolchain=local ;;
    *) probe_gotoolchain=$ambient_gotoolchain ;;
esac

go_version_raw=$(GOTOOLCHAIN="$probe_gotoolchain" "$go_bin" version 2>&1) \
    || refuse "GOTOOLCHAIN=$probe_gotoolchain '$go_bin version' failed: $go_version_raw"

# "go version go1.26.10 linux/amd64" / "go version devel go1.28-1a2b3c4d ..."
running_raw=$(printf '%s\n' "$go_version_raw" | grep -oE 'go[0-9][^[:space:]]*' | head -n 1 || true)
[ -n "$running_raw" ] || refuse "no version found in \`$go_bin version\` output: '$go_version_raw'"
parse_version "$running_raw" || refuse "cannot parse the running toolchain version '$running_raw'"
RUNNING_MAJOR=$PV_MAJOR
RUNNING_MINOR=$PV_MINOR
RUNNING_PATCH=$PV_PATCH
running_ver="$RUNNING_MAJOR.$RUNNING_MINOR.$RUNNING_PATCH"

# ---------------------------------------------------------------------------
# 3. the verdict
# ---------------------------------------------------------------------------

if [ "$(ver_cmp "$RUNNING_MAJOR" "$RUNNING_MINOR" "$RUNNING_PATCH" \
    "$FLOOR_MAJOR" "$FLOOR_MINOR" "$FLOOR_PATCH")" = -1 ]; then
    printf 'toolchain-check: FAIL — the INSTALLED Go toolchain is BELOW this module floor\n' >&2
    printf 'toolchain-check:   required : go %s   (%s, %s)\n' "$floor_ver" "$floor_source" "$gomod" >&2
    printf 'toolchain-check:   running  : go%s   (%s, `GOTOOLCHAIN=%s go version`: %s)\n' \
        "$running_ver" "$go_path" "$probe_gotoolchain" "$go_version_raw" >&2
    printf 'toolchain-check:   GOTOOLCHAIN in this environment: %s\n' "$ambient_gotoolchain" >&2
    case "$ambient_gotoolchain" in
        auto)
            printf 'toolchain-check: GOTOOLCHAIN=auto is why this machine may look healthy: Go silently\n' >&2
            printf 'toolchain-check: downloads go%s and the build then succeeds — while the toolchain that\n' "$floor_ver" >&2
            printf 'toolchain-check: is actually installed is the go%s above. That fetch cannot happen on an\n' "$running_ver" >&2
            printf 'toolchain-check: offline, pinned or freshly provisioned host, and with GOTOOLCHAIN=local the\n' >&2
            printf 'toolchain-check: build fails closed with exactly:\n' >&2
            ;;
        *)
            printf 'toolchain-check: an offline or pinned host cannot download a newer toolchain, and with\n' >&2
            printf 'toolchain-check: GOTOOLCHAIN=local the build fails closed with exactly:\n' >&2
            ;;
    esac
    printf 'toolchain-check:   go: go.mod requires go >= %s (running go %s; GOTOOLCHAIN=local)\n' "$floor_ver" "$running_ver" >&2
    printf 'toolchain-check: WHAT TO DO: the provisioning bootstrap (or you) must install a Go\n' >&2
    printf 'toolchain-check:   toolchain at or above go %s and put it first on PATH, then re-run\n' "$floor_ver" >&2
    printf 'toolchain-check:   `make toolchain-check`. This gate downloads nothing on purpose: an\n' >&2
    printf 'toolchain-check:   unattended bootstrap must not appear green by relying on a fetch.\n' >&2
    exit "$BELOW_FLOOR_RC"
fi

printf 'toolchain-check: OK — installed go%s satisfies the module floor go %s (%s, %s, probed with GOTOOLCHAIN=%s; environment GOTOOLCHAIN=%s)\n' \
    "$running_ver" "$floor_ver" "$floor_source" "$gomod" "$probe_gotoolchain" "$ambient_gotoolchain"
exit 0
