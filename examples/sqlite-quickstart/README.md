# sqlite-quickstart — a session with no PostgreSQL service

Runnable demonstration of CR-CHAT-006: the dual backend with **SQLite** as the
query view and **JSONL** as the ordered append log and transport form. Nothing
is installed beyond the Go toolchain — the SQLite driver is pure Go
(`modernc.org/sqlite`), so there is no CGO, no database server and no
PostgreSQL URL anywhere in the program.

```bash
go run ./examples/sqlite-quickstart
```

What it does, in order:

1. opens a JSONL log root and a SQLite database in a scratch directory;
2. creates a session, adds a member, posts a root message and a reply, and
   branches a thread — appending each record to the log and projecting the
   SAME record into the SQLite view (record → JSONL line → view is the only
   write direction);
3. prints the reduced session `State` read back from the view;
4. **reconciles** the log against the view, which reports a mismatch instead
   of healing one;
5. closes both, reopens the same paths, re-projects the log and prints the
   State again — identical, because the log is the log of record and the view
   is durable.

To run a server on the SQLite backend instead of PostgreSQL:

```bash
CR_SESSION_BACKEND=sqlite CR_SQLITE_PATH=crier-sessions.sqlite make run
```

That is the whole configuration: `CR_SESSION_BACKEND=sqlite` with no
`CR_DATABASE_URL` needs no service. Selecting `postgres` keeps the
server-backed view for multi-user and scale deployments, and `jsonl` runs on
the log alone.
