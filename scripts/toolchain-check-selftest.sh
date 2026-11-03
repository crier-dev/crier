#!/usr/bin/env bash
#
# scripts/toolchain-check-selftest.sh — the regression for
# scripts/toolchain-check.sh (DF-CRIER-496).
#
# WHY THIS EXISTS
# ---------------
# The clean-machine failure this row records was SILENT on every networked box:
# with GOTOOLCHAIN=auto, Go downloads the toolchain go.mod asks for, so a
# provisioning bootstrap that installs a toolchain BELOW the module floor looks
# completely healthy until a laptop, a pinned host or a chaos-disconnect cell
# (GOPROXY=off, GOTOOLCHAIN=local) tries to build with no network. A floor check
# that has only ever been seen to pass is decoration, so this drives the real
# script in BOTH directions on PATH shims:
#
#   * PASS — a toolchain exactly at the floor and one above it,
#   * FAIL — a toolchain below the floor (go1.26.5 against a 1.26.10 floor, the
#     measured bunker-las-03 shape), asserting the check's OWN exit code (1, not
#     a build/refusal code) and that the message names both versions, says
#     "bootstrap" and quotes the GOTOOLCHAIN=local refusal,
#   * MASKING — the trap that makes the clean-machine failure invisible: a `go`
#     that reports the AUTO-selected (downloaded, healthy) version for a plain
#     `go version` but the INSTALLED below-floor version under GOTOOLCHAIN=local
#     must still be REJECTED. A gate that judged the auto answer would print OK
#     here — measured on the row's own host.
#   * derivation — a module file whose floor is ABOVE the running toolchain must
#     be rejected naming that floor, which proves the floor is READ from go.mod
#     and not a constant that could drift,
#   * fail-closed — a missing module file and an unparseable version are exit 2,
#     never a green over input the check could not read,
#   * NEUTER — a copy whose below-floor verdict is forced to success must FAIL
#     the below-floor arm, so the rejection cannot be vacuous.
#
# The shims are `go` wrappers that answer `version` with a chosen string and
# delegate EVERY other subcommand to the real toolchain, which is the exact
# shape a distro/pinned `go` presents.
#
# No network, no downloads, no toolchain switch, nothing written outside a
# scratch dir; ~2s. Exit 0 = every arm behaved, 1 = an arm regressed, 2 = the
# selftest refused to run (no go, no checker).
set -euo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
check="$script_dir/toolchain-check.sh"

[ -f "$check" ] || { printf 'selftest: REFUSED — missing %s\n' "$check" >&2; exit 2; }
real_go=$(command -v go 2>/dev/null || true)
[ -n "$real_go" ] || { printf 'selftest: REFUSED — no `go` on PATH\n' >&2; exit 2; }

gomod="$repo_root/go.mod"
[ -f "$gomod" ] || { printf 'selftest: REFUSED — missing %s\n' "$gomod" >&2; exit 2; }
floor=$(awk '$1 == "go" && NF >= 2 { print $2; exit }' "$gomod")
[ -n "$floor" ] || { printf 'selftest: REFUSED — no `go` directive in %s\n' "$gomod" >&2; exit 2; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/toolchain-check-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

# The simulated old toolchain is the version the bunker-las-03 bootstrap really
# installed, so the regression reproduces the measured shape. If the module floor
# ever drops to 1.26.5 or below, that premise is gone: refuse loudly instead of
# asserting a rejection that can no longer happen.
OLD_VERSION=1.26.5
if [ "$(printf '%s\n%s\n' "$floor" "$OLD_VERSION" | sort -V | tail -n 1)" = "$OLD_VERSION" ]; then
    printf 'selftest: REFUSED — the module floor (%s) is not above the simulated old toolchain (%s);\n' \
        "$floor" "$OLD_VERSION" >&2
    printf 'selftest: pick a simulated-old version below the floor before trusting this regression.\n' >&2
    exit 2
fi

failures=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# make_shim FILE REPORTED — a `go` wrapper that answers `version` with REPORTED
# (verbatim, so both the real shape and a garbage one can be simulated) and
# delegates EVERY other subcommand to the real toolchain.
make_shim() {
    mkdir -p "$(dirname -- "$1")"
    {
        printf '%s\n' '#!/usr/bin/env bash'
        printf 'if [ "${1-}" = version ]; then\n'
        printf '  echo "%s"\n' "$2"
        printf '  exit 0\n'
        printf 'fi\n'
        printf 'exec %q "$@"\n' "$real_go"
    } >"$1"
    chmod +x "$1"
}

# make_shim_masking FILE INSTALLED AUTO — mimics the real toolchain's
# GOTOOLCHAIN=auto behaviour: `go version` reports AUTO (the version Go would
# select/download, i.e. a healthy-looking one) while GOTOOLCHAIN=local reports
# INSTALLED (what is really on the machine). `go env GOTOOLCHAIN` reports "auto".
make_shim_masking() {
    mkdir -p "$(dirname -- "$1")"
    {
        printf '%s\n' '#!/usr/bin/env bash'
        printf 'if [ "${1-}" = version ]; then\n'
        printf '  if [ "${GOTOOLCHAIN-}" = local ]; then\n'
        printf '    echo "go version go%s linux/amd64"\n' "$2"
        printf '  else\n'
        printf '    echo "go version go%s linux/amd64"\n' "$3"
        printf '  fi\n'
        printf '  exit 0\n'
        printf 'fi\n'
        printf 'if [ "${1-}" = env ]; then\n'
        printf '  echo "auto"\n'
        printf '  exit 0\n'
        printf 'fi\n'
        printf 'exec %q "$@"\n' "$real_go"
    } >"$1"
    chmod +x "$1"
}

make_shim "$tmp/shim-old/go" "go version go$OLD_VERSION linux/amd64"
make_shim "$tmp/shim-at-floor/go" "go version go$floor linux/amd64"
make_shim "$tmp/shim-newer/go" "go version go1.99.0 linux/amd64"
make_shim "$tmp/shim-garbage/go" "go version devel"
make_shim_masking "$tmp/shim-masking/go" "$OLD_VERSION" "$floor"

# run_check GO_BIN GOMOD -> sets OUT and RC for the real checker.
run_check() {
    set +e
    OUT=$(TOOLCHAIN_CHECK_GO="$1" TOOLCHAIN_CHECK_GOMOD="$2" bash "$check" 2>&1)
    RC=$?
    set -e
}

# expect_rc NAME WANT_RC — grades the last run_check.
expect_rc() {
    if [ "$RC" = "$2" ]; then
        pass "$1 (rc $RC)"
    else
        fail "$1 (rc $RC, wanted $2)"
        printf '      --- output ---\n%s\n      --------------\n' "$OUT"
    fi
}

# expect_out NAME NEEDLE — grades that the last run_check output names NEEDLE.
expect_out() {
    case "$OUT" in
        *"$2"*) pass "$1 mentions '$2'" ;;
        *)
            fail "$1 does not mention '$2'"
            printf '      --- output ---\n%s\n      --------------\n' "$OUT"
            ;;
    esac
}

printf 'toolchain-check selftest: floor read from go.mod = go %s\n' "$floor"

# --- 1. the real toolchain on THIS host: the verdict follows the INSTALLED one
# (derived independently here, with GOTOOLCHAIN=local — never from what a plain
# `go version` prints, which under GOTOOLCHAIN=auto may name a downloaded one).
base_raw=$(GOTOOLCHAIN=local "$real_go" version 2>/dev/null || true)
base_ver=$(printf '%s\n' "$base_raw" | grep -oE 'go[0-9][^[:space:]]*' | head -n 1 || true)
base_ver=${base_ver#go}
base_ver=${base_ver%%[!0-9.]*}
if [ -n "$base_ver" ] && [ "$(printf '%s\n%s\n' "$base_ver" "$floor" | sort -V | tail -n 1)" = "$base_ver" ]; then
    want_real=0
else
    want_real=1
fi
run_check "$real_go" "$gomod"
expect_rc "real host: verdict agrees with the INSTALLED toolchain (${base_ver:-unknown} vs floor $floor)" "$want_real"

# --- 2. simulated OLD toolchain must be REJECTED by the check itself --------
run_check "$tmp/shim-old/go" "$gomod"
expect_rc "simulated go$OLD_VERSION is rejected" 1
expect_out "the failure names the required version" "$floor"
expect_out "the failure names the running version" "go$OLD_VERSION"
expect_out "the failure says the bootstrap must install at or above the floor" "bootstrap"
expect_out "the failure quotes the GOTOOLCHAIN=local refusal" "GOTOOLCHAIN=local"
expect_out "the failure is the check's own FAIL verdict" "toolchain-check: FAIL"

# --- 2b. MASKING: an auto-selected "healthy" answer must not fool the gate ---
# This shim reports a HEALTHY version for a plain `go version` and the
# below-floor INSTALLED version under GOTOOLCHAIN=local — the real toolchain's
# behaviour, measured on the row's own host. A gate that judged the auto answer
# would print OK here; this one must reject.
run_check "$tmp/shim-masking/go" "$gomod"
expect_rc "a toolchain that only LOOKS healthy under GOTOOLCHAIN=auto is rejected" 1
expect_out "the masking rejection names the INSTALLED version" "go$OLD_VERSION"
expect_out "the masking rejection names the required version" "$floor"

# --- 3. exactly at the floor, and above it, must PASS -----------------------
run_check "$tmp/shim-at-floor/go" "$gomod"
expect_rc "a toolchain exactly at the floor passes" 0
expect_out "the OK line names the floor" "go $floor"
expect_out "the OK verdict is a single OK line" "toolchain-check: OK"

run_check "$tmp/shim-newer/go" "$gomod"
expect_rc "a toolchain above the floor passes" 0

# --- 4. the floor is DERIVED from go.mod (a raised floor changes the verdict)
printf 'module example.com/drift\n\ngo 1.99.0\n' >"$tmp/drift-go.mod"
run_check "$real_go" "$tmp/drift-go.mod"
expect_rc "a floor above the running toolchain is rejected" 1
expect_out "the rejection names the raised floor (not a constant)" "1.99.0"

# --- 5. fail closed, never a green over unreadable input --------------------
run_check "$real_go" "$tmp/absent-go.mod"
expect_rc "a missing module file is REFUSED (2), not passed" 2

run_check "$tmp/shim-garbage/go" "$gomod"
expect_rc "an unparseable version is REFUSED (2), not passed" 2

# --- 6. NEUTER: force the below-floor verdict to success --------------------
mkdir -p "$tmp/neuter"
if sed 's/^BELOW_FLOOR_RC=1$/BELOW_FLOOR_RC=0/' "$check" >"$tmp/neuter/toolchain-check.sh"; then
    if cmp -s "$check" "$tmp/neuter/toolchain-check.sh"; then
        fail "NEUTER  the neutered copy is byte-identical (the sed target is gone)"
    else
        pass "NEUTER  the neutered copy differs from the shipped check"
        set +e
        OUT=$(TOOLCHAIN_CHECK_GO="$tmp/shim-old/go" TOOLCHAIN_CHECK_GOMOD="$gomod" \
            bash "$tmp/neuter/toolchain-check.sh" 2>&1)
        RC=$?
        set -e
        if [ "$RC" = 0 ]; then
            pass "NEUTER  with the verdict forced to success the old toolchain passes (rc 0)"
        else
            fail "NEUTER  the forced-success copy still exited $RC — the sed no longer targets the verdict"
            printf '      --- output ---\n%s\n      --------------\n' "$OUT"
        fi
    fi
else
    fail "NEUTER  could not build the neutered copy"
fi

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'toolchain-check selftest: FAIL (%d arm(s) failed)\n' "$failures" >&2
    exit 1
fi
printf 'toolchain-check selftest: PASS (every arm behaved; nothing downloaded)\n'
exit 0
