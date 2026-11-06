# Operating crier — backup, restore, and post-restore health

This is the operations half of the story. **Installing and starting crier is
`INSTALL.md`** (§6 runs it as a service, §7 verifies the install, §8 is
troubleshooting) — that path is not duplicated here.

What is here is the part nothing else in the tree covers: crier owns a durable
registry/inbox store **and** a session store, and neither has a recovery path
unless one is written down and executed. Everything below is a command you can
run, and every output quoted below was measured, not asserted.

---

## 1. What you are actually backing up

Two independent stores, selected by different variables. Backing up the wrong
one loses the agents and their undelivered inboxes. `scripts/backup-db.sh` (§2.1)
walks every store the environment configures; the commands below are what it
runs, spelled out so you can do any single step by hand.

| store | selected by | lives in | durable? |
| --- | --- | --- | --- |
| **registry + inboxes** | `CR_DATABASE_URL` (PostgreSQL) | a PostgreSQL server: the compose `postgres` service (`pgdata` volume), or any PostgreSQL 14+ instance | yes — the durable path the README documents |
| **sessions / chat** | `CR_SESSION_BACKEND=sqlite` + `CR_SESSION_SQLITE_PATH` (legacy alias `CR_SQLITE_PATH`) | a single SQLite file | yes, for that store only |
| **registry, SQLite flavour** | `CR_REGISTRY_BACKEND=sqlite` + `CR_REGISTRY_SQLITE_PATH` | a single SQLite file | yes, for that store only |
| neither set | — | process memory | **no** — agents and inboxes are lost on exit |

Relative SQLite paths resolve against the unit's `WorkingDirectory`:
`$XDG_STATE_HOME/crier` for the user unit, `/var/lib/crier` for the system unit
(measured — `crier.db` appeared exactly there). Back up the **absolute** path
`/status` reports, not the value you typed: `session_store_path` and
`registry_store_path` echo the resolved location.

---

## 2. PostgreSQL — the durable store

### 2.1 Back up

The whole job, over every configured store, verified as it goes:

```sh
CR_DATABASE_URL=postgres:///crier_fleet?host=/var/run/postgresql \
  ./scripts/backup-db.sh ~/crier-backups --drill
```

It dumps (PostgreSQL custom format, and/or each SQLite file named by
`CR_SESSION_SQLITE_PATH` / `CR_REGISTRY_SQLITE_PATH`), proves each artifact
(`pg_restore --list`, `pragma integrity_check`), and with `--drill` restores the
PostgreSQL dump into a throwaway database and compares public-table counts.
Exit 0 means every configured store dumped **and** verified; the failing store
is named on stderr otherwise. By hand, the PostgreSQL half is:

Compose stack (the `postgres` service from this repo's `docker-compose.yml`):

```sh
docker compose exec -T postgres \
  pg_dump -U crier -d crier -Fc > "crier-$(date +%Y%m%dT%H%M%S).dump"
```

`-Fc` (custom format) is the form `pg_restore` wants, and it compresses. Plain SQL
works too: drop `-Fc` and use `psql` on the way back in. Write the dump
somewhere outside the `pgdata` volume — a dump that shares a disk with the
database it protects is not a backup.

Client form, for a server crier does not own (any `CR_DATABASE_URL`, including a
unix-socket database). This is exactly the shape the fleet instance uses —
`postgres:///crier_fleet?host=/var/run/postgresql`:

```sh
pg_dump --no-owner --no-privileges "$CR_DATABASE_URL" -Fc > "crier-$(date +%Y%m%dT%H%M%S).dump"
```

Schema-only, when you only want the shape:

```sh
pg_dump --schema-only --no-owner --no-privileges "$CR_DATABASE_URL" > crier-schema.sql
```

### 2.2 Restore

`scripts/restore-db.sh` performs the PostgreSQL restore below with three guards:
it refuses without `CR_RESTORE_CONFIRM=yes`, refuses while a `crier` process is
running (exit 3), and refuses a dump that does not parse — so a truncated file
cannot destroy the store it was meant to recover. The commands are:

Stop the server first — a restore into a store a live process holds leaves the
process reading a database that changed under it:

```sh
systemctl --user stop crier.service          # or: systemctl stop crier

docker compose exec -T postgres \
  pg_restore -U crier -d crier --clean --if-exists < crier-20261105T191500.dump
```

`--clean --if-exists` replaces the objects the dump defines and leaves the rest
alone; on a brand-new empty database drop both flags. Client form:

```sh
pg_restore --no-owner --no-privileges -d "$CR_DATABASE_URL" --clean --if-exists < crier-….dump
```

Then start the unit again and run §5.

### 2.3 Prove the dump restores — a drill, into a scratch database

A backup nobody has restored is a file whose validity is unknown. The drill
dumps the live store, restores it into a **throwaway database**, compares table
counts, and drops the throwaway. Nothing writes to the live database.

```sh
SCRATCH_DB=crier_restore_drill_$(date +%H%M%S)
pg_dump --schema-only --no-owner --no-privileges "$CR_DATABASE_URL" > /tmp/crier-drill.sql
createdb -h /var/run/postgresql "$SCRATCH_DB"
psql "postgres:///$SCRATCH_DB?host=/var/run/postgresql" -q -f /tmp/crier-drill.sql
psql "postgres:///$SCRATCH_DB?host=/var/run/postgresql" -tAc \
  "select count(*) from information_schema.tables where table_schema='public'"
dropdb -h /var/run/postgresql "$SCRATCH_DB"
```

Measured on the fleet instance (2026-11-05):

```
live tables:  22
pg_dump rc=0 bytes=22648 lines=769
scratch db created: crier_restore_drill_191909
restore rc=0
restored tables: 22
scratch db dropped: crier_restore_drill_191909
```

Equal counts, `pg_dump rc=0`, `psql -f` rc=0 — the dump is restorable and the
schema is complete. A drill is only green when the restored count equals the
live count **and** both commands exited 0; quote all of it.

---

## 3. SQLite — the session store (and the SQLite registry flavour)

Both are single files, so the backup is a copy that cannot catch the store
mid-write. Use SQLite's own online backup, not `cp`, while the server runs.

```sh
sqlite3 "$CR_SESSION_SQLITE_PATH" ".backup '$CR_SESSION_SQLITE_PATH.$(date +%Y%m%dT%H%M%S).bak'"
# equivalent, and also fine on a quiesced store:
sqlite3 "$CR_SESSION_SQLITE_PATH" "VACUUM INTO '$CR_SESSION_SQLITE_PATH.vacuum.$(date +%Y%m%dT%H%M%S).db'"
```

`scripts/restore-db.sh --sqlite <live-db-path> <backup-file>` does this with the
same guards and keeps the replaced file as `<db>.pre-restore-<stamp>`. By hand,
restore is a copy back to the original path, with the server stopped:

```sh
systemctl --user stop crier.service
cp -p "$CR_SESSION_SQLITE_PATH.20261105T191500.bak" "$CR_SESSION_SQLITE_PATH"
systemctl --user start crier.service
```

Use `$CR_REGISTRY_SQLITE_PATH` in the same two commands for the SQLite registry
flavour. Keep the `-wal` and `-shm` siblings out of the copy: `.backup` and
`VACUUM INTO` produce a single consistent file, which is the point of using them.

### 3.1 Prove the SQLite backup restores

```sh
sqlite3 store.db ".backup 'store.backup.db'"
cp -p store.backup.db store.restored.db
echo "src:      $(sqlite3 store.db        'select count(*) from agents;')"
echo "restored: $(sqlite3 store.restored.db 'select count(*) from agents;')"
sqlite3 store.restored.db 'pragma integrity_check;'
```

Measured on a scratch store seeded with three agents and three inbox rows
(2026-11-05):

```
src agents:        3
src inbox_entries: 3
arm1 backup file:  present
arm2 vacuum file:  present
restored agents:        3
restored inbox_entries: 3
restored names:         relay-a,relay-b,relay-c
vacuum agents:          3
backup integrity:   ok
restored integrity: ok
```

Both arms (`.backup` and `VACUUM INTO`) produce a file that restores with equal
counts and `pragma integrity_check` = `ok`.

---

## 4. Cadence and retention

- **PostgreSQL:** a dump at least daily (`scripts/backup-db.sh ~/crier-backups`),
  retained off-host; keep one weekly dump
  for a month. The store is small (the schema alone dumps to ~22 KB; a full dump
  of a busy instance stays in the low megabytes), so retention is cheap.
- **SQLite session store:** the same daily cadence. Sessions are chattier than
  registry rows — dump before any upgrade, and always before a migration.
- **Before every upgrade or `docker compose down`:** take a dump and run the §2.3
  or §3.1 arm. A migration that cannot be rolled back is a restore you did not
  test.
- **Monthly:** run one full drill restore against the real dump and record the
  counts. Delete the throwaway database afterwards either way.

A dump that has never been restored does not count as a backup.

---

## 5. Post-restore health check

The server publishes its own verdict about the store it opened — read it rather
than inferring from the process being up.

```sh
curl -s localhost:8767/health      # {"status":"ok"}
curl -s localhost:8767/version     # build identity actually running
curl -s -H "Authorization: Bearer $CR_AUTH_TOKEN" localhost:8767/status
```

Fields in `/status` that answer "did the restore work" (`cmd/server/status.go`):

| field | what a healthy restore shows |
| --- | --- |
| `health` | `ok` (graded from the store's own probe) |
| `registry_backend` | `postgres` — **not** `memory`; `memory` means the server booted non-durable and your restore is invisible to it |
| `registry_durable` | `true` |
| `registry_store_path` / `session_store_path` | the path the server actually opened — compare it with what you restored |
| `agents_total` | the row count you expect after the restore (0 on an empty store, `null`/unchanged is a red flag) |
| `queue_depth` | an object, not `null` — `null` means the store cannot report a queue |

`/metrics` answers the same question over time; it is **not** auth-exempt
(`internal/middleware/auth.go`), so a scrape carries the bearer or answers 401 —
see `deploy/prometheus-scrape.example.yml`. `CR_ENABLE_METRICS=false` leaves the
path 404 entirely.

If `registry_backend` reads `memory` after a restart, the server never saw
`CR_DATABASE_URL`: it is running the demo store and logging the DEMO-ONLY
warning quoted in `INSTALL.md` §6.3. Fix the unit's environment before trusting
anything else in `/status`.

---

## 6. The scheduled drill, and what is still not automated

The scripts exist and are the supported path — `scripts/backup-db.sh` dumps and
verifies every configured store, `scripts/restore-db.sh` restores with the
guards above. The **cadence** used to be an operator obligation with nothing
behind it: a day with no dump was silent, because a job nobody scheduled cannot
fail. Two unit files ship in [`deploy/`](deploy/) now, and they are the whole
difference between "we dump daily" and a dump that exists:

| unit | what it is |
| --- | --- |
| `deploy/crier-backup.service` | `Type=oneshot`. Runs the backed-up **drill** arm: `backup-db.sh /var/backups/crier --drill` — dump every configured store, verify each artifact, then restore the PostgreSQL dump into a throwaway database and compare public-table counts with live. Same `User=crier` and the same `EnvironmentFile=-/etc/crier/crier.env` as `deploy/crier.service`, so it reads the stores the server actually opened. |
| `deploy/crier-backup.timer` | Fires the service **daily at 03:20** system-local, `Persistent=true` (a run missed while the box was off happens at the next boot instead of being skipped), `RandomizedDelaySec=30min` (a fleet does not all drill at once), `WantedBy=timers.target`. |

**Dump destination:** `/var/backups/crier`, owned `crier:crier`, mode `0750`,
artifacts named `crier-postgres-<stamp>.dump`, `crier-session-<stamp>.db`,
`crier-registry-<stamp>.db`. The unit names that path in `ReadWritePaths=`
because `ProtectSystem=strict` makes `/var/backups` read-only to the job; it
cannot create the directory itself, so the operator creates it once (step 1
below), and a missing directory fails the unit instead of dumping somewhere
else. A per-user install that takes dumps by hand (§4, `~/crier-backups`) is
unaffected — this unit is for the system-service flavour.

### 6.1 Enable it

```sh
# 1. the dump directory — must exist BEFORE the first run (ReadWritePaths=)
sudo install -d -m 0750 -o crier -g crier /var/backups/crier

# 2. the script the unit executes. ExecStart points at an INSTALLED COPY, not at
#    the checkout: a timer must not depend on where the repo lives, and a
#    `git pull` that moves or deletes the tree must not disarm the backup.
sudo install -d -m 0755 /usr/local/lib/crier/scripts
sudo install -m 0755 scripts/backup-db.sh /usr/local/lib/crier/scripts/backup-db.sh
#    RE-RUN THIS after any change to scripts/backup-db.sh — the timer runs the
#    copy, so an edited checkout is not what fires at 03:20.

# 3. the units
sudo install -m 0644 deploy/crier-backup.service /etc/systemd/system/crier-backup.service
sudo install -m 0644 deploy/crier-backup.timer   /etc/systemd/system/crier-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now crier-backup.timer
systemctl list-timers crier-backup.timer --no-pager   # proves the next fire

# 4. prove it now, by hand, instead of waiting for the clock
sudo systemctl start crier-backup.service
systemctl status crier-backup.service --no-pager
journalctl -u crier-backup.service -n 40 --no-pager
```

Step 4 is not ceremony: it is the only way to know the account can reach the
stores **before** the first unattended fire.

### 6.2 What failure looks like

There is no `|| true`, no `SuccessExitStatus=` and no wrapper that swallows the
exit code. `backup-db.sh` exits non-zero when a store could not be dumped, when
an artifact does not parse, or when the drill's restored table count disagrees
with live, and it names the failing store on stderr. That leaves
`crier-backup.service` **failed**, so the miss has two surfaces:

```sh
systemctl --failed | grep crier-backup
journalctl -u crier-backup.service -n 50 --no-pager
```

The unit ships **no `OnFailure=`** on purpose: a target named there would be
untested boilerplate, and an alarm unit that itself fails is a second silent
hole. The failed state plus the journal line is the alarm — read it after a
reboot that follows a `Persistent=true` catch-up run, too.

Two expected non-failures that still deserve a look in that journal:

- `SKIP postgres drill: cannot CREATE DATABASE as crier` — the dump was made and
  verified (`pg_restore --list`), but the restore comparison did not run because
  the service account cannot create databases. Exit stays 0. Grant `CREATEDB` to
  the account if the drill is to be real.
- `skip postgres: CR_DATABASE_URL unset …` — no PostgreSQL store configured on
  this host. With no store at all the script exits 1 (`no store configured`) and
  the unit fails, which is correct: a backup job that backs up nothing is a
  failure, not a green.

### 6.3 Retention is still yours

The timer **never deletes**. §4's retention rule stands unchanged and stays an
operator task: keep enough dumps to cover the restore window, retain one weekly
dump for a month, and keep copies **off-host** — a drill that proves a dump is
restorable says nothing about a disk that also held the only copy. Nothing in
this repository prunes: a pruner that deletes the wrong backup is a worse
failure than a full `/var/backups`, so it was deliberately not shipped here.

**What is still not automated:** pruning, off-host replication, and any alerting
beyond the failed unit and its journal line. That residual — and the retention
automation that would close it — is tracked on the board as `CRIER-OPS-002`.
