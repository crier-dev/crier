# Crier diagnostics — 2026-09-24 addendum: the restart that wasn't, and how to never be fooled by it

Context: this project's server lifecycle is already well tooled (`make stop`
verifies `/proc/<pid>/exe`, refuses foreign binaries, never SIGKILLs; a failed
bind names the port, the holder-check command and the build identity). This
addendum records the one sequence that still defeats a careful user, hit live
during the 2026-09-24 dogfood run, and the verification habit that catches it.

## The sequence: "restart" that silently keeps the old server

1. Server A runs on :8767 with a pidfile (`.crier.pid`, JSON).
2. You kill A **out-of-band, wrong** (or believe you did), then start B with the
   same port. B logs `server failed: another process already holds this port`
   — **to its own stdout/stderr** — and exits.
3. If B's output went into a redirected log or a subshell you did not read,
   everything after that point talks to **A**, while you believe you are
   talking to B. Config changes (e.g. pointing B at PostgreSQL) did not apply;
   A's old backend is answering.
4. `.crier.pid` still describes A — plausible, on disk, and (in the observed
   case) the pid was even alive.

How it was caught in the run: the "after restart" stats query returned an
`oldest_age_ms` that continued the pre-restart continuum instead of resetting —
data was still the old server's memory. Any durably-queued message that
"survived" a memory-backend restart is this exact bug in your own procedure,
not in crier.

## The right way (all verifiable in seconds)

- Restart through the repo's own tooling: `make stop` (reads the pidfile,
  verifies `/proc/<pid>/exe` is the same binary, SIGTERM, waits for the port)
  then start. Never `kill $(cat .crier.pid)` — the pidfile is JSON, so that
  line feeds bash `{`, `"pid":` … as pids (filed as DF-CRIER-283).
- After any restart, prove the world you are in:
  `curl -s :8767/status` → `build.commit` must be **your** checkout's commit
  (`git rev-parse --short HEAD`), and `registry_backend` must be the backend
  you configured. `/status` exists precisely for this.
- The failed-bind error is accurate and loud — the failure mode is only ever
  "nobody read B's output". When starting B non-interactively, tail its log or
  `grep -l "server failed"` the redirect target before continuing.

## Related honest-behaviour notes from the same run

- Memory backend loses inboxes on a REAL restart — `docs/architecture.md:57`
  says so plainly. The durable promise needs `CR_DATABASE_URL` (Postgres);
  the `docker compose up -d postgres` recipe in `docs/integration-guide.md` §6
  worked first-try on a rootless agent (override the host port with
  `CRIER_PG_HOST_PORT` when 5437 is taken), migrations auto-apply, and 21/21
  undelivered messages survived two restarts.
- Postgres recipe assumes the compose *service* port maps to
  `localhost:5437`; on a shared/agent host with a per-user docker socket,
  set `CRIER_PG_HOST_PORT` and use the same number in `CR_DATABASE_URL`.
