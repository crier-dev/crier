#!/usr/bin/env bash
# Prove verify-live-release accepts valid offline evidence and rejects hash drift.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="$SCRIPT_DIR/verify-live-release.sh"
PROG=verify-live-release-selftest
# ch:trace row=REVIEW-001 spec=none:board-acceptance wave=crier-foreman-2026-11-08-21-33-49 test=make-verify-live-release-selftest doc=none:repo-verifier prompt=/home/kara/.hermes/cache/scratch/crier-foreman-2026-11-08-21-33-49/brief-REVIEW-001-rework.txt evidence=stdout:valid-fixture-and-altered-hash-arms witness=scripts/verify-live-release-selftest.sh verdict=none:foreman-judges-after-merge commit=none:worker-commit memory=none:foreman-closeout
# STATUS: partial
# TIER: T1
# EVIDENCE: make verify-live-release-selftest
command -v git >/dev/null 2>&1 || { echo "$PROG: ERROR: git is required" >&2; exit 2; }
command -v mktemp >/dev/null 2>&1 || { echo "$PROG: ERROR: mktemp is required" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "$PROG: ERROR: jq is required" >&2; exit 2; }
[[ -r "$CHECKER" ]] || { echo "$PROG: ERROR: checker missing: $CHECKER" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/verify-live-release-selftest.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
FIXTURE="$WORK/fixture"
REPO="$FIXTURE/repo"
mkdir -p "$REPO" "$FIXTURE/proc/4242"
git -C "$REPO" init -q -b main
git -C "$REPO" -c user.name=verify-selftest -c user.email=verify-selftest@example.invalid -c commit.gpgsign=false commit --allow-empty -qm base
git -C "$REPO" tag v0.1.0
git -C "$REPO" -c user.name=verify-selftest -c user.email=verify-selftest@example.invalid -c commit.gpgsign=false commit --allow-empty -qm descendant
COMMIT="$(git -C "$REPO" rev-parse HEAD)"
printf 'fixture README\n' > "$REPO/README.md"
printf 'CRIER_PORT=18767\n' > "$FIXTURE/fleet.env"
printf '{"pid":4242,"port":18767,"binary":"fixture"}\n' > "$FIXTURE/crier.pid"
printf '{"version":"fixture","commit":"%s","build_time":"fixture","modified":false}\n' "$COMMIT" > "$FIXTURE/version.json"
cp /bin/true "$FIXTURE/crier"
ln -s "$FIXTURE/crier" "$FIXTURE/proc/4242/exe"
sha256sum "$FIXTURE/proc/4242/exe" | awk '{print $1}' > "$FIXTURE/recorded.sha256"

OUT="$WORK/valid.out"
if "$CHECKER" --fixture "$FIXTURE" >"$OUT" 2>&1; then
  grep -q '^PASS  ancestry ' "$OUT" || { cat "$OUT" >&2; echo "$PROG: FAIL: valid fixture omitted ancestry PASS" >&2; exit 1; }
  grep -q '^PASS  binary-hash ' "$OUT" || { cat "$OUT" >&2; echo "$PROG: FAIL: valid fixture omitted hash PASS" >&2; exit 1; }
  echo 'PASS  valid-fixture accepts descendant commit and matching process hash'
else
  cat "$OUT" >&2
  echo 'FAIL  valid-fixture rejected valid ancestry/hash evidence' >&2
  exit 1
fi

printf '%064d\n' 0 > "$FIXTURE/recorded.sha256"
OUT="$WORK/altered.out"
if "$CHECKER" --fixture "$FIXTURE" >"$OUT" 2>&1; then
  cat "$OUT" >&2
  echo 'FAIL  altered-recorded-hash was accepted' >&2
  exit 1
fi
grep -q '^PASS  ancestry ' "$OUT" || { cat "$OUT" >&2; echo "$PROG: FAIL: altered-hash fixture failed before ancestry could be checked" >&2; exit 1; }
grep -q '^FAIL  binary-hash ' "$OUT" || { cat "$OUT" >&2; echo "$PROG: FAIL: altered-hash fixture lacked the named hash rejection" >&2; exit 1; }
echo 'FAIL  altered-recorded-hash rejected as required'
