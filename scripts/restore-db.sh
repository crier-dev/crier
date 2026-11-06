#!/usr/bin/env bash
# crier restore — put a dump back, with the guards that make it safe to run.
#
#   CR_RESTORE_CONFIRM=yes CR_DATABASE_URL=... ./scripts/restore-db.sh <dump>
#   ./scripts/restore-db.sh --sqlite <live-db-path> <backup-file>
#
# Guards (each exits non-zero with the reason):
#   1. CR_RESTORE_CONFIRM must be `yes` — a restore is destructive by definition.
#   2. No crier server may be running (pgrep -x crier). Stop the unit first: a
#      restore under a live process leaves it reading a database that changed.
#   3. The dump must parse (`pg_restore --list` / sqlite open) BEFORE anything is
#      replaced, so a truncated file cannot destroy the current store.
#
# PostgreSQL: `pg_restore --clean --if-exists` replaces the objects the dump
# defines. SQLite: the backup is copied over the live path, and the pre-restore
# file is kept beside it as <db>.pre-restore-<stamp>.
#
# After either mode, re-check /status (docs/operations.md §5): registry_backend
# must NOT read `memory`, registry_durable=true, agents_total is the expected
# count.
set -uo pipefail
stamp="$(date +%Y%m%dT%H%M%S)"

die() {
  local msg="$1"
  local code="${2:-1}"
  echo "REFUSED: $msg" >&2
  exit "$code"
}

# Guard 2, with the one documented exception: a DRILL restores into a scratch
# database/store while the live server keeps running. Overriding it is only
# legitimate when the target is not the store the server has open — the script
# cannot tell, so it prints the hazard and the caller owns the claim.
still_running() {
  pgrep -x crier >/dev/null 2>&1 || return 0
  if [ "${CR_RESTORE_ALLOW_RUNNING:-no}" = "yes" ]; then
    echo "WARN: crier is running (pid $(pgrep -x crier | tr '\n' ' ')) — CR_RESTORE_ALLOW_RUNNING=yes set." >&2
    echo "WARN: this is only safe when the target is NOT the store that process holds open." >&2
    return 0
  fi
  die "a crier server is running (pid $(pgrep -x crier | tr '\n' ' ')) — stop it first" 3
}

if [ "${1:-}" = "--sqlite" ]; then
  live="${2:-}"; backup="${3:-}"
  [ -n "$live" ] && [ -n "$backup" ] || die "usage: $0 --sqlite <live-db-path> <backup-file>" 2
  [ "${CR_RESTORE_CONFIRM:-}" = "yes" ] || die "set CR_RESTORE_CONFIRM=yes to confirm" 2
  [ -f "$backup" ] || die "backup file $backup does not exist" 2
  [ -f "$live" ] || die "live store $live does not exist (refusing to create one blind)" 2
  chk="$(sqlite3 "$backup" 'pragma integrity_check;' 2>/dev/null | head -1)"
  [ "$chk" = "ok" ] || die "backup $backup fails integrity_check (${chk:-<no output>}) — not restoring" 1
  still_running
  keep="$live.pre-restore-$stamp"
  cp -p "$live" "$keep" || die "could not preserve the current store at $keep" 1
  cp -p "$backup" "$live" || die "copy failed; current store preserved at $keep" 1
  echo "ok sqlite restore: $backup -> $live"
  echo "ok pre-restore copy: $keep"
  echo "   rows now: $(sqlite3 "$live" "select count(*) from sqlite_master where type='table';") tables"
  exit 0
fi

dump="${1:-}"
[ -n "$dump" ] || die "usage: $0 <dump>   (postgres; needs CR_DATABASE_URL + CR_RESTORE_CONFIRM=yes)" 2
[ "${CR_RESTORE_CONFIRM:-}" = "yes" ] || die "set CR_RESTORE_CONFIRM=yes to confirm" 2
[ -f "$dump" ] || die "dump $dump does not exist" 2
[ -n "${CR_DATABASE_URL:-}" ] || die "set CR_DATABASE_URL to the database you intend to overwrite" 2
command -v pg_restore >/dev/null 2>&1 || die "pg_restore not installed" 1
pg_restore --list "$dump" >/dev/null 2>&1 || die "dump $dump does not parse with pg_restore --list — not restoring" 1
still_running

echo "restoring $(basename "$dump") into ${CR_DATABASE_URL##*/} …"
pg_restore --no-owner --no-privileges --clean --if-exists -d "$CR_DATABASE_URL" "$dump" \
  || die "pg_restore rc!=0 — inspect before starting the server" 1
echo "ok postgres restore"
echo "   public tables now: $(psql "$CR_DATABASE_URL" -tAc "select count(*) from information_schema.tables where table_schema='public'")"
echo "start the unit, then check /status: registry_backend=postgres, registry_durable=true (docs/operations.md §5)"
