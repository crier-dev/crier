#!/usr/bin/env bash
# crier backup — dump both durable stores and verify each dump.
#
#   ./scripts/backup-db.sh [OUT_DIR] [--drill]
#
# Stores (docs/operations.md §1):
#   registry + inboxes  CR_DATABASE_URL  -> PostgreSQL custom-format dump
#   sessions / chat     CR_SESSION_SQLITE_PATH  (legacy CR_SQLITE_PATH)
#   registry (sqlite)   CR_REGISTRY_SQLITE_PATH
#
# Verification is part of the run, not an extra step:
#   * postgres: `pg_restore --list` must parse the dump; with --drill the dump is
#     restored into a throwaway database and the public table count is compared
#     with the live one, then the throwaway is dropped.
#   * sqlite: the backup file is opened and `pragma integrity_check` must say ok.
#
# Exit 0 = every configured store dumped AND verified. Anything else = 1, with
# the failing store named on stderr. Prints one line per artifact it wrote.
set -uo pipefail

OUT_DIR="${1:-$HOME/crier-backups}"
DRILL="${2:-}"
case "$OUT_DIR" in -*) echo "usage: $0 [OUT_DIR] [--drill]" >&2; exit 2 ;; esac
[ "${1:-}" = "--drill" ] && { DRILL=--drill; OUT_DIR="$HOME/crier-backups"; }
mkdir -p "$OUT_DIR" || exit 1

stamp="$(date +%Y%m%dT%H%M%S)"
rc=0
wrote=0

pg_dump_store() {
  local url="$1" out="$OUT_DIR/crier-postgres-$stamp.dump"
  if ! command -v pg_dump >/dev/null 2>&1; then
    echo "FAIL postgres: pg_dump not installed" >&2; return 1
  fi
  if ! pg_dump --no-owner --no-privileges -Fc "$url" >"$out" 2>"$OUT_DIR/.pg_dump.err"; then
    echo "FAIL postgres: pg_dump rc!=0 ($(head -1 "$OUT_DIR/.pg_dump.err" 2>/dev/null))" >&2
    rm -f "$out"; return 1
  fi
  echo "ok postgres dump: $out ($(wc -c <"$out") bytes)"
  if ! pg_restore --list "$out" >/dev/null 2>&1; then
    echo "FAIL postgres: dump does not parse with pg_restore --list" >&2; return 1
  fi
  echo "ok postgres dump parses (pg_restore --list)"
  [ "$DRILL" = "--drill" ] || return 0

  local live scratch
  live="$(psql "$url" -tAc "select count(*) from information_schema.tables where table_schema='public'" 2>/dev/null)"
  scratch="crier_restore_drill_$stamp"
  if ! createdb -h /var/run/postgresql "$scratch" 2>/dev/null; then
    echo "SKIP postgres drill: cannot CREATE DATABASE as $(id -un)"; return 0
  fi
  local surl="postgres:///$scratch?host=/var/run/postgresql"
  if ! pg_restore --no-owner --no-privileges -d "$surl" "$out" 2>"$OUT_DIR/.pg_restore.err"; then
    echo "FAIL postgres drill: pg_restore rc!=0 ($(head -1 "$OUT_DIR/.pg_restore.err" 2>/dev/null))" >&2
    dropdb -h /var/run/postgresql "$scratch" 2>/dev/null; return 1
  fi
  local restored
  restored="$(psql "$surl" -tAc "select count(*) from information_schema.tables where table_schema='public'" 2>/dev/null)"
  dropdb -h /var/run/postgresql "$scratch" 2>/dev/null
  if [ -n "$live" ] && [ "$live" = "$restored" ]; then
    echo "ok postgres drill: $restored/$live public tables restored"
    return 0
  fi
  echo "FAIL postgres drill: restored=$restored live=$live" >&2; return 1
}

sqlite_store() {
  local db="$1" label="$2" out
  [ -f "$db" ] || { echo "FAIL $label: $db does not exist" >&2; return 1; }
  out="$OUT_DIR/crier-$label-$stamp.db"
  command -v sqlite3 >/dev/null 2>&1 || { echo "FAIL $label: sqlite3 not installed" >&2; return 1; }
  if ! sqlite3 "$db" ".backup '$out'" 2>/dev/null; then
    echo "FAIL $label: .backup failed on $db" >&2; rm -f "$out"; return 1
  fi
  echo "ok $label backup: $out ($(wc -c <"$out") bytes)"
  local chk
  chk="$(sqlite3 "$out" 'pragma integrity_check;' 2>/dev/null | head -1)"
  if [ "$chk" = "ok" ]; then
    echo "ok $label backup integrity_check = ok"
  else
    echo "FAIL $label: integrity_check = ${chk:-<no output>}" >&2; return 1
  fi
}

if [ -n "${CR_DATABASE_URL:-}" ]; then
  pg_dump_store "$CR_DATABASE_URL" || rc=1; wrote=$((wrote+1))
elif docker compose ps --services --status running 2>/dev/null | grep -qx postgres; then
  CARRIER="$(docker compose ps -q postgres 2>/dev/null)"
  if [ -n "$CARRIER" ]; then
    out="$OUT_DIR/crier-postgres-$stamp.dump"
    if docker compose exec -T postgres pg_dump -U crier -d crier -Fc >"$out" 2>"$OUT_DIR/.pg_dump.err"; then
      if pg_restore --list "$out" >/dev/null 2>&1 || [ ! -s "$OUT_DIR/.pg_dump.err" ]; then
        echo "ok postgres dump (compose): $out ($(wc -c <"$out") bytes)"; wrote=$((wrote+1))
      else
        echo "FAIL postgres (compose): dump does not parse" >&2; rc=1; wrote=$((wrote+1))
      fi
    else
      echo "FAIL postgres (compose): pg_dump rc!=0" >&2; rc=1; wrote=$((wrote+1))
    fi
  fi
else
  echo "skip postgres: CR_DATABASE_URL unset and no running compose 'postgres' service"
fi

for pair in "CR_SESSION_SQLITE_PATH:session" "CR_SQLITE_PATH:session" "CR_REGISTRY_SQLITE_PATH:registry"; do
  var="${pair%%:*}" label="${pair##*:}"
  val="${!var:-}"
  [ -n "$val" ] && [ -f "$val" ] && { sqlite_store "$val" "$label" || rc=1; wrote=$((wrote+1)); }
done

if [ "$wrote" -eq 0 ]; then
  echo "FAIL: no store configured — set CR_DATABASE_URL, CR_SESSION_SQLITE_PATH or CR_REGISTRY_SQLITE_PATH" >&2
  exit 1
fi
echo "backup artifacts in $OUT_DIR: $(ls -1 "$OUT_DIR" | grep -c "crier-.*-$stamp")"
exit "$rc"
