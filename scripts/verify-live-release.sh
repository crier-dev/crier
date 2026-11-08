#!/usr/bin/env bash
# Verify the running Crier process against its served build identity and recorded hash.
set -euo pipefail

PROG=verify-live-release
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  cat <<'EOF'
verify-live-release.sh — verify live Crier release ancestry and binary hash

Usage:
  bash scripts/verify-live-release.sh --env-file PATH --pidfile PATH --hash-file PATH
  bash scripts/verify-live-release.sh --fixture DIR

The env file supplies CRIER_PORT. The hash file contains one 64-hex SHA-256
recorded by the host-owned deployment/restart leg. The pidfile is Crier's JSON
pidfile; its pid selects /proc/<pid>/exe. --fixture runs offline from fixture
files: fleet.env, crier.pid, version.json, recorded.sha256, and repo/.
EOF
}

fail() { printf 'FAIL  %s\n' "$*" >&2; return 1; }
info() { printf '%s: %s\n' "$PROG" "$*"; }

ENV_FILE=""
PID_FILE=""
HASH_FILE=""
FIXTURE=""
while (($#)); do
  case "$1" in
    --env-file) (($# >= 2)) || { usage >&2; exit 2; }; ENV_FILE="$2"; shift 2 ;;
    --pidfile) (($# >= 2)) || { usage >&2; exit 2; }; PID_FILE="$2"; shift 2 ;;
    --hash-file) (($# >= 2)) || { usage >&2; exit 2; }; HASH_FILE="$2"; shift 2 ;;
    --fixture) (($# >= 2)) || { usage >&2; exit 2; }; FIXTURE="$2"; shift 2 ;;
    -h|--help|help) usage; exit 0 ;;
    *) printf '%s: ERROR: unknown argument: %s\n' "$PROG" "$1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -n "$FIXTURE" ]]; then
  [[ -z "$ENV_FILE$PID_FILE$HASH_FILE" ]] || { printf '%s: ERROR: --fixture cannot be combined with live paths\n' "$PROG" >&2; exit 2; }
  ENV_FILE="$FIXTURE/fleet.env"
  PID_FILE="$FIXTURE/crier.pid"
  HASH_FILE="$FIXTURE/recorded.sha256"
  VERSION_FILE="$FIXTURE/version.json"
  PROC_ROOT="$FIXTURE/proc"
  REPO="$FIXTURE/repo"
  CURL_MODE=fixture
else
  [[ -n "$ENV_FILE" && -n "$PID_FILE" && -n "$HASH_FILE" ]] || { printf '%s: ERROR: live mode requires --env-file, --pidfile, and --hash-file\n' "$PROG" >&2; exit 2; }
  VERSION_FILE=""
  PROC_ROOT=/proc
  REPO="$REPO_ROOT"
  CURL_MODE=live
fi

for required in "$ENV_FILE" "$PID_FILE" "$HASH_FILE"; do
  [[ -r "$required" ]] || { printf '%s: ERROR: required input is missing or unreadable: %s\n' "$PROG" "$required" >&2; exit 2; }
done
[[ -d "$REPO/.git" || -f "$REPO/.git" ]] || { printf '%s: ERROR: repository metadata missing: %s\n' "$PROG" "$REPO" >&2; exit 2; }
[[ -r "$REPO/README.md" ]] || { printf '%s: ERROR: README.md is missing or unreadable in %s\n' "$PROG" "$REPO" >&2; exit 2; }

# Read only the two allowed values; never source/evaluate an operator env file.
PORT="$(awk -F= '$1 == "CRIER_PORT" { gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2); gsub(/^\047|\047$/, "", $2); gsub(/^"|"$/, "", $2); print $2; found=1 } END { if (!found) exit 1 }' "$ENV_FILE")" || { printf '%s: ERROR: CRIER_PORT missing from %s\n' "$PROG" "$ENV_FILE" >&2; exit 2; }
[[ "$PORT" =~ ^[0-9]+$ ]] && ((10#$PORT > 0 && 10#$PORT <= 65535)) || { printf '%s: ERROR: invalid CRIER_PORT in %s\n' "$PROG" "$ENV_FILE" >&2; exit 2; }

command -v jq >/dev/null 2>&1 || { printf '%s: ERROR: jq is required to parse pidfile/version JSON\n' "$PROG" >&2; exit 2; }
PID="$(jq -er '.pid | select(type == "number" and . > 0 and floor == .)' "$PID_FILE" 2>/dev/null)" || { printf '%s: ERROR: pidfile has no valid pid: %s\n' "$PROG" "$PID_FILE" >&2; exit 2; }
PID_PORT="$(jq -er '.port | select(type == "number" and . > 0 and floor == .)' "$PID_FILE" 2>/dev/null)" || { printf '%s: ERROR: pidfile has no valid port: %s\n' "$PROG" "$PID_FILE" >&2; exit 2; }
[[ "$PID_PORT" == "$PORT" ]] || { printf '%s: ERROR: pidfile port %s does not match fleet env CRIER_PORT %s\n' "$PROG" "$PID_PORT" "$PORT" >&2; exit 2; }

if [[ "$CURL_MODE" == fixture ]]; then
  [[ -r "$VERSION_FILE" ]] || { printf '%s: ERROR: fixture version response missing: %s\n' "$PROG" "$VERSION_FILE" >&2; exit 2; }
  VERSION_JSON="$(<"$VERSION_FILE")"
else
  command -v curl >/dev/null 2>&1 || { printf '%s: ERROR: curl is required to read /version\n' "$PROG" >&2; exit 2; }
  VERSION_JSON="$(curl --fail --silent --show-error --max-time 5 "http://127.0.0.1:${PORT}/version")" || { printf '%s: ERROR: GET /version failed at port %s\n' "$PROG" "$PORT" >&2; exit 2; }
fi

command -v jq >/dev/null 2>&1 || { printf '%s: ERROR: jq is required to parse /version JSON\n' "$PROG" >&2; exit 2; }
COMMIT="$(jq -er '.commit | select(type == "string" and length > 0)' <<<"$VERSION_JSON")" || { printf '%s: ERROR: /version has missing or invalid commit data\n' "$PROG" >&2; exit 2; }

EXE="$PROC_ROOT/$PID/exe"
[[ -e "$EXE" || -L "$EXE" ]] || { printf '%s: ERROR: process executable link is missing: %s\n' "$PROG" "$EXE" >&2; exit 2; }
[[ -r "$EXE" ]] || { printf '%s: ERROR: process executable is unreadable: %s\n' "$PROG" "$EXE" >&2; exit 2; }
ACTUAL_HASH="$(sha256sum "$EXE" | awk '{print $1}')" || { printf '%s: ERROR: cannot hash process executable: %s\n' "$PROG" "$EXE" >&2; exit 2; }
RECORDED_HASH="$(tr -d '[:space:]' < "$HASH_FILE")"
[[ "$RECORDED_HASH" =~ ^[[:xdigit:]]{64}$ ]] || { printf '%s: ERROR: recorded hash is missing or not a 64-hex SHA-256: %s\n' "$PROG" "$HASH_FILE" >&2; exit 2; }

LATEST_TAG="$(git -C "$REPO" tag --list 'v*' --sort=-version:refname | awk 'NR == 1 {print; exit}')"
[[ -n "$LATEST_TAG" ]] || { printf '%s: ERROR: no v* release tag exists in %s\n' "$PROG" "$REPO" >&2; exit 2; }
TAG_COMMIT="$(git -C "$REPO" rev-parse --verify "refs/tags/${LATEST_TAG}^{commit}" 2>/dev/null)" || { printf '%s: ERROR: newest tag does not resolve to a commit: %s\n' "$PROG" "$LATEST_TAG" >&2; exit 2; }
SERVED_COMMIT="$(git -C "$REPO" rev-parse --verify "${COMMIT}^{commit}" 2>/dev/null)" || { printf '%s: ERROR: served commit is not present in %s: %s\n' "$PROG" "$REPO" "$COMMIT" >&2; exit 1; }
if ! git -C "$REPO" merge-base --is-ancestor "$TAG_COMMIT" "$SERVED_COMMIT"; then
  printf 'FAIL  ancestry  served commit %s is not a descendant of newest tag %s (%s)\n' "$COMMIT" "$LATEST_TAG" "$TAG_COMMIT" >&2
  exit 1
fi
printf 'PASS  ancestry  served commit %s descends from newest tag %s\n' "$COMMIT" "$LATEST_TAG"
if [[ "$ACTUAL_HASH" != "$RECORDED_HASH" ]]; then
  printf 'FAIL  binary-hash running %s sha256=%s differs from recorded sha256=%s\n' "$EXE" "$ACTUAL_HASH" "$RECORDED_HASH" >&2
  exit 1
fi
printf 'PASS  binary-hash running process executable matches recorded sha256=%s\n' "$ACTUAL_HASH"
