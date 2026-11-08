# DUCKBRAIN-ARCHIVE — DuckBrain as long-term cold storage for crier

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 ("registry storage should be using
duckbrain for storage stuff, long-term storage for sure, so it is cheap") · INT-008

## 1. Purpose

Crier's hot stores (SQLite/Postgres) hold every dead letter and task-output blob forever within
retention, then DELETE it — data dies at retention expiry. This spec offloads cold records to
DuckBrain (the fleet's git-backed memory ledger, storage of record) at retention-expiry time:
archive-then-delete. SQLite/Postgres stay the hot store; DuckBrain becomes the cheap long-term
tier. Zero change to any delivery path.

**Premise verification (measured at HEAD `7e4c3f4`, 2026-11-08).** The bakeoff cited
`internal/registry/sqlite_store.go:389` for registry storage. CONFIRMED: line 389 sits in the
`sqliteDeliver` insert into `inbox_entries`; `SQLiteStore` is the registry store selected by
`CR_REGISTRY_BACKEND`. Measured facts the spec builds on: the `Store` interface is big-core +
optional capability interfaces (`internal/registry/store.go:25-51`, `DeadLetterStore`
store.go:135-146); agent cards are NOT stored (projection of the `Agent` row at read time,
`cmd/server/a2acard.go:110-119`); dead letters have 7-day retention enforced by DELETE
(`sqlite_store.go:585`, `postgres_ownership.go:123-124`); messages have 24h TTL and dead-letter
on expiry (`store.go:493-529`); no tiering/cold archive exists anywhere.

## 2. Verified external surface (DuckBrain)

Measured from fleet clients in active use (`/home/kara/wt-1757-r2/internal/sync/duckbrain.go:908-1074`,
`/home/kara/tr-clean-383/scripts/sync_task_router_duckbrain.py:34-45`), 2026-11-08:

| Operation | Contract |
|---|---|
| Write | `POST {baseURL}/api/memories?namespace={ns}` — JSON body `{"key": string, "domain": string, "content": string, "attributes": {}}`, header `X-API-Key: <value from env>` (header NAME only; values never appear in this repo) |
| Read | `GET {baseURL}/api/memories/key/{key}?namespace={ns}` — same auth |
| Storage of record | DuckBrain's git repo; hourly snapshots are the backup story |
| Body cap | ~100 KiB observed cap on request bodies (102400 bytes in the reference client) |

## 3. Interface

New package `internal/archive`. Workers copy these exactly.

```go
// Archiver writes one immutable record to cold storage. Implementations must
// be safe for concurrent use.
type Archiver interface {
	Archive(ctx context.Context, rec Record) (Result, error)
}

type Record struct {
	Key     string // deterministic, globally unique: see §5 key scheme
	Domain  string // always "crier"
	Content string // full JSON of the archived record
}

type Result struct {
	Key        string
	ArchivedAt time.Time
}

// DuckBrainArchiver implements Archiver over the §2 surface.
func NewDuckBrainArchiver(cfg Config, client *http.Client) *DuckBrainArchiver
```

Registry-side hook — one new optional capability interface on the store backends (imitating
`DeadLetterStore`, `store.go:135`):

```go
// ArchiveHook lets a caller observe dead letters immediately before their
// retention DELETE. Returned true = safe to delete; false = retain.
type ArchiveHook interface {
	PurgeExpiredDeadLetters(archive func(dl *DeadLetter) bool) int
}
```

- `SQLiteStore` implements it by selecting due rows (`dead_lettered_at < now-retention`),
  calling `archive(dl)` per row, deleting only rows where it returned true
  (replaces the bare `DELETE` at `sqlite_store.go:585`); `PostgresStore` likewise
  (`postgres_ownership.go:123-124`). Compile check `var _ ArchiveHook = (*SQLiteStore)(nil)`.
- Message expiry (24h TTL) needs NO new hook: expired messages already become dead letters
  (`ownership_test.go:13-111`), so they reach DuckBrain via the same dead-letter path at the
  end of their dead-letter retention.

## 4. Wiring

- Env vars (all read via `config.Load`'s getenv):
  - `CR_ARCHIVE_ENABLED` — tolerant bool, **default false** (`parseTolerantBool` pattern).
  - `CR_DUCKBRAIN_URL` — base URL, required when enabled.
  - `CR_DUCKBRAIN_API_KEY_FILE` — path to a file containing the API key, `chmod 0600`,
    read per request (credential discipline of `HERMES-SESSION-DELIVERY.md`; the raw key is
    never in config, env, or logs).
  - `CR_DUCKBRAIN_NAMESPACE` — target DuckBrain namespace, default `"crier"`.
  - `CR_ARCHIVE_MAX_BODY_BYTES` — int, default `96_000` (safely under the §2 cap).
- Startup refusal (fail-closed, DETECTION pattern): enabled but URL empty, key file missing,
  or key file group/world-readable → exit 1 with the named reason.
- main.go: build `*DuckBrainArchiver` when `cfg.Archive.Enabled`; wrap the sweep runner so each
  `PurgeExpired` tick also invokes the dead-letter archive hook. HTTP client: 10s per-request
  timeout, 3 attempts with exponential backoff (2s/8s).
- The delivery path (`Deliver`/`Retrieve`/`Ack`) never touches the archiver.

## 5. Record scheme

- Dead letter: `Key = "crier/dead-letter/" + agentID + "/" + messageID`, `Domain = "crier"`,
  `Content = json.Marshal(dl)` (the exact `DeadLetter` struct, `internal/registry/deadletter.go:53-96`).
- Reads (operator tooling, `crier archive get <agentID> <messageID>` CLI subcommand): the §2 GET,
  response content parsed back into `DeadLetter`.
- Key collisions: message ids are unique (dead-letter insert is `ON CONFLICT(message_id) DO
  NOTHING`), so keys are unique.

## 6. Error Catalog

| Condition | Behaviour |
|---|---|
| Archive POST fails (network, non-2xx) after retries | row **retained** (archive returns false → no DELETE), `crier_archive_attempts_total{outcome="failed"}` incremented; retried on the next sweep |
| Payload `len(dl.Payload) > CR_ARCHIVE_MAX_BODY_BYTES` | `outcome="too_large"`, row retained — never deleted, never truncated; operator raises the cap or purges manually |
| DuckBrain disabled at startup | capability never asserted; sweep is byte-identical to today |
| Key file unreadable at request time | archive attempt fails → row retained, `outcome="failed"` |
| Non-200/201 from POST | same as network failure |

The refusal invariant: **a record is deleted from the hot store only after its DuckBrain write
returned 2xx.** No path deletes first.

## 7. Edge Cases

- Concurrent sweeps: the sweep runner is single-threaded today; the hook must not be invoked
  concurrently (per-sweep serialisation).
- Duplicate archive (crash between 2xx and DELETE): the deterministic key makes the retry an
  idempotent overwrite; DuckBrain key-upsert semantics tolerate it.
- Empty `Payload` (`[]byte{}`): archived as-is (`"payload": ""` in content JSON).
- Unicode/odd agent ids in keys: ids are URL-path-safe per registry validation; the archiver
  additionally `url.PathEscape`s each key segment.
- Clock skew between crier and DuckBrain: irrelevant — crier timestamps rule; DuckBrain is a
  key-value ledger.

## 8. Testing

1. Test that with `CR_ARCHIVE_ENABLED=false`, `PurgeExpiredDeadLetters` deletes exactly the rows
   the old bare DELETE deleted (diff the table contents before/after the refactor).
2. Test that a dead letter past retention is POSTed (httptest server asserting method, path,
   `X-API-Key` header present, body keys `key/domain/content`) and deleted only after a 2xx.
3. Test that a 500 response retains the row and increments `crier_archive_attempts_total{outcome="failed"}`.
4. Test that an oversized payload is retained with `outcome="too_large"`.
5. Test startup refusal when enabled with missing URL / missing key file / permissive key file mode.
6. Test that the API key value never appears in any log line (capture slog output, grep).

## 9. Metrics

`crier_archive_attempts_total{outcome=archived|failed|too_large}` (CounterVec) and
`crier_archive_duration_seconds` (Histogram, the fixed 12-bucket set of
`docs/observability-spec.md` §3.3), registered in `internal/metrics.Default` — same zero-dependency
registry, no client_golang.

## 10. Coverage table

| R# | Statement | Assertion |
|---|---|---|
| R-D1 | Default off = byte-identical sweep | test: 1 |
| R-D2 | Delete only after 2xx archive | test: 2 |
| R-D3 | Failed archive retains row + metric | test: 3 |
| R-D4 | Oversized payload retained, never truncated | test: 4 |
| R-D5 | Fail-closed startup on bad config | test: 5 |
| R-D6 | Key value never logged | test: 6 |
| R-D7 | Deterministic, idempotent keys | check: §5 scheme + test 2 body assertion |

Out of scope (binding): moving the hot store to DuckBrain (SQLite/Postgres stay); archiving
live inbox entries or task evidence (task outputs are a follow-up row if wanted); batch/bulk
archive API; DuckBrain read-back into the hot store (operator CLI only).
