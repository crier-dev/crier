# CI-003b — PostgreSQL Persistence for Registry and Inboxes

## 1. Overview

CI-003b replaces the process-local maps in `internal/registry` with PostgreSQL-backed agent registration and inbox delivery. The public HTTP API and the nine storage operations keep their existing behavior: agents are registered once, messages are delivered FIFO, retrieval takes an exclusive lease, acknowledgement permanently deletes only messages under that lease, expired messages are deleted, and deleting an agent cascades to its inbox.

This work is limited to registry and inbox persistence. Relay topic persistence, agent authentication/authorization, cross-region replication, retry queues, message encryption at rest, status-heartbeat updates, and a change to the HTTP/OpenAPI contract are out of scope.

### Required repository layout

```text
config/
  config.go                         # add DatabaseConfig and strict environment parsing
internal/registry/
  handler.go                        # receiver changes from *Store to *Handler
  memory_store.go                   # current map implementation, renamed MemoryStore
  migrate.go                        # embedded golang-migrate runner
  migrations/
    001_create_agents.up.sql
    001_create_agents.down.sql
    002_create_inbox_entries.up.sql
    002_create_inbox_entries.down.sql
  postgres_store.go                 # pgxpool implementation
  registry_test.go                  # unit tests through Store
  postgres_store_integration_test.go # build tag: integration
  store.go                          # Store interface, errors, constructors
  types.go                          # Agent and InboxEntry remain HTTP/domain types
cmd/server/main.go                  # migration/pool construction, handler wiring, close pool
specs/
  ci-003b-postgresql-persistence.md
```

### Invariants

1. `agents.id` identifies exactly one registered agent.
2. Every inbox row belongs to an existing agent; PostgreSQL enforces this with `ON DELETE CASCADE`.
3. An unacknowledged, unexpired message is eligible exactly when it has no lease or its persisted lease expiry is at or before the retrieval time.
4. A message is deleted only by `Ack` when its `agent_id`, `id`, and `lease_id` all match in one transaction, or by expiry cleanup.
5. Retrieval orders messages by database-assigned `delivery_sequence`, not by caller-provided timestamps. This preserves FIFO under concurrent deliveries.
6. All persisted timestamps are `TIMESTAMPTZ` and application code uses UTC `time.Time` values.
7. `lease_expires_at` is an implementation-required persistence column. The current in-memory `LeaseDuration` exists only in a Go object and is lost across process restart; retaining only `leased_at` and `lease_id` cannot safely implement variable lease durations accepted by `Retrieve`.

### Completion criteria

- PostgreSQL is the production store selected at startup; failure to migrate or ping prevents the server from listening.
- `MemoryStore` remains available and is the default unit-test backend.
- All handler calls are made through `Store`; handlers know no SQL or `pgx` types.
- Migrations are forward-only during normal startup and have tested down files for local/test teardown.

The production assignment is deliberately interface-typed so the router cannot accidentally depend on a PostgreSQL-only method:

```go
pgStore, err := registry.NewPostgresStoreWithPoolConfig(ctx, cfg.Database.URL, poolConfig)
if err != nil { /* startup is fatal */ }
var regStore registry.Store = pgStore
```

---

## 2. Dependencies

### Go modules

Add these direct dependencies at the exact versions below, then run `go mod tidy`:

```text
github.com/jackc/pgx/v5 v5.10.0
github.com/golang-migrate/migrate/v4 v4.19.1
github.com/testcontainers/testcontainers-go v0.43.0   # test dependency only
```

Required imports by responsibility:

```go
// internal/registry/postgres_store.go
import (
    "context"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"
    "github.com/jackc/pgx/v5/pgxpool"
)

// internal/registry/migrate.go
import (
    "database/sql"
    "embed"
    "errors"

    "github.com/golang-migrate/migrate/v4"
    "github.com/golang-migrate/migrate/v4/database/postgres"
    "github.com/golang-migrate/migrate/v4/source/iofs"
    _ "github.com/jackc/pgx/v5/stdlib" // registers database/sql driver "pgx"
)
```

PostgreSQL 16 is the supported server version for CI and development. PostgreSQL 14+ is compatible because the schema uses only standard `JSONB`, identity columns, partial indexes, CTEs, and `FOR UPDATE SKIP LOCKED`.

### Runtime prerequisites

- A reachable PostgreSQL database whose role can create tables, indexes, and the `schema_migrations` table used by golang-migrate.
- The production connection string must use `sslmode=require`, `verify-ca`, or `verify-full`; `sslmode=disable` is permitted only for local Docker and testcontainers.
- Docker must be available to run integration tests. Unit tests do not require Docker or PostgreSQL.

### Dependency injection and startup order

`cmd/server/main.go` must create the database-backed store before registering registry routes and before calling `ListenAndServe`:

```go
cfg, err := config.Load()
if err != nil {
    log.Fatalf("load configuration: %v", err)
}

startupCtx, cancelStartup := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
defer cancelStartup()

pgStore, err := registry.NewPostgresStoreWithPoolConfig(startupCtx, cfg.Database.URL, registry.PoolConfig{
    MaxConns:        cfg.Database.MaxConns,
    MinConns:        cfg.Database.MinConns,
    MaxConnLifetime: cfg.Database.MaxConnLifetime,
    MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
})
if err != nil {
    log.Fatalf("initialize PostgreSQL registry store: %v", err)
}
defer pgStore.Close()

var regStore registry.Store = pgStore
registryHandler := registry.NewHandler(regStore)

r.HandleFunc("/agents", registryHandler.HandleRegister).Methods("POST")
r.HandleFunc("/agents", registryHandler.HandleListAgents).Methods("GET")
r.HandleFunc("/agents/{id}", registryHandler.HandleGetAgent).Methods("GET")
r.HandleFunc("/agents/{id}", registryHandler.HandleUnregister).Methods("DELETE")
r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleDeliver).Methods("POST")
r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleRetrieve).Methods("GET")
r.HandleFunc("/agents/{id}/inbox/ack", registryHandler.HandleAck).Methods("POST")
r.HandleFunc("/agents/{id}/inbox/stats", registryHandler.HandleStats).Methods("GET")
```

On `SIGINT` or `SIGTERM`, preserve the existing shutdown ordering and close the pool after `srv.Shutdown(ctx)` returns. The purge goroutine must be cancelled before `pgStore.Close()`:

```go
purgeCancel()
meshSvc.Stop()
if err := srv.Shutdown(ctx); err != nil {
    log.Printf("http shutdown: %v", err)
}
pgStore.Close()
```

`Close` is deliberately not one of the nine `Store` methods because the in-memory implementation owns no resources. It is a concrete `PostgresStore` lifecycle method used only by composition roots and integration-test cleanup.

---

## 3. Interface

### Store contract

Replace the current map-owning `type Store struct` with this interface in `internal/registry/store.go`. These are the exact nine existing public persistence signatures; do not add `context.Context` parameters or change return shapes in this task.

```go
package registry

import "time"

// Store persists registered agents and their lease-based inboxes.
// Implementations must be safe for concurrent callers.
type Store interface {
    Register(agent *Agent) error
    Get(id string) (*Agent, error)
    List() []*Agent
    Unregister(id string) error
    Deliver(agentID string, entry *InboxEntry) error
    Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error)
    Ack(agentID, leaseID string, messageIDs []string) error
    Stats(agentID string) (queueDepth, leasedCount int, oldestAge time.Duration, err error)
    PurgeExpired() int
}

// Handler keeps HTTP concerns separate from storage implementations.
type Handler struct {
    store Store
}

func NewHandler(store Store) *Handler {
    return &Handler{store: store}
}
```

Move every existing `Handle*` method receiver from `func (s *Store)` to `func (h *Handler)` and replace direct calls such as `s.Register(...)` with `h.store.Register(...)`. The handler's JSON request and response types do not change. This is required because Go cannot attach HTTP handler methods to an interface while preserving a concrete backend-independent router surface.

Rename the current in-memory structure and constructor as follows:

```go
type MemoryStore struct {
    mu      sync.RWMutex
    agents  map[string]*Agent
    inboxes map[string][]*InboxEntry
}

func NewMemoryStore() *MemoryStore {
    return &MemoryStore{
        agents:  make(map[string]*Agent),
        inboxes: make(map[string][]*InboxEntry),
    }
}

var _ Store = (*MemoryStore)(nil)
```

Tests must use the interface rather than the concrete type:

```go
func setupTestStore(t *testing.T) Store {
    t.Helper()
    return registry.NewMemoryStore()
}
```

### Error taxonomy

Define errors in `store.go`; implementations wrap these sentinels with IDs and driver errors for logs:

```go
var (
    ErrAgentNotFound = errors.New("agent not found")
    ErrAgentExists   = errors.New("agent already registered")
    ErrLeaseConflict = errors.New("message is not leased under the supplied lease")
    ErrInvalidStoreInput = errors.New("invalid store input")
)
```

The handler must use `errors.Is` rather than its current second `Get` call to choose status codes:

```go
if err := h.store.Ack(id, req.LeaseID, req.MessageIDs); err != nil {
    switch {
    case errors.Is(err, ErrAgentNotFound):
        writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
    default:
        writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
    }
    return
}
```

Existing route behavior remains: duplicate registration is `409`, missing agents are `404`, invalid HTTP input is `400`, and an invalid/missing/expired lease or unknown message in `Ack` is `409`.

### PostgreSQL types and constructor

```go
// PoolConfig is backend configuration after config.Load has parsed environment values.
type PoolConfig struct {
    MaxConns        int32
    MinConns        int32
    MaxConnLifetime time.Duration
    MaxConnIdleTime time.Duration
}

func DefaultPoolConfig() PoolConfig {
    return PoolConfig{
        MaxConns: 4, MinConns: 0,
        MaxConnLifetime: 30 * time.Minute,
        MaxConnIdleTime: 5 * time.Minute,
    }
}

type PostgresStore struct {
    pool *pgxpool.Pool
}

func NewPostgresStore(
    ctx context.Context,
    connString string,
) (*PostgresStore, error)

func NewPostgresStoreWithPoolConfig(
    ctx context.Context,
    connString string,
    poolConfig PoolConfig,
) (*PostgresStore, error)

func (s *PostgresStore) Close()

var _ Store = (*PostgresStore)(nil)
```

`NewPostgresStore` calls `NewPostgresStoreWithPoolConfig(ctx, connString, DefaultPoolConfig())`. `NewPostgresStoreWithPoolConfig` must reject an empty connection string, call `RunMigrations(ctx, connString)`, apply all four `PoolConfig` fields to a parsed `pgxpool.Config`, create the pool with `pgxpool.NewWithConfig`, and call `pool.Ping(ctx)`. Any failure must close already-open resources and return a wrapped error; the server must not start in an unready state.

---

## 4. Behavior

All PostgreSQL methods use a short-lived internal operation context:

```go
func (s *PostgresStore) operationContext() (context.Context, context.CancelFunc) {
    return context.WithTimeout(context.Background(), 5*time.Second)
}
```

The externally fixed `Store` interface has no request context, so this is the mandatory database timeout. A timeout returns the wrapped context/pgx error. `List` cannot return an error by contract: it logs the query error with `log.Printf` and returns a non-nil empty slice. All other methods return errors.

### Register

1. Reject `nil`, blank `agent.ID`, a public key not exactly `ed25519.PublicKeySize` bytes, an invalid nonempty status, or capabilities that cannot be marshalled. Return `ErrInvalidStoreInput`.
2. Set `RegisteredAt` and `LastSeen` to one UTC `now` value, default blank status to `StatusOnline`, and convert nil capabilities to `[]string{}`.
3. Marshal capabilities with `json.Marshal`; do not interpolate JSON into SQL.
4. Execute the insert below. Map SQLSTATE `23505` to `ErrAgentExists`.

```sql
INSERT INTO agents (
    id, public_key, capabilities, status, registered_at, last_seen
) VALUES ($1, $2, $3::jsonb, $4, $5, $6);
```

Parameters are `agent.ID`, `[]byte(agent.PublicKey)`, JSON bytes, `string(agent.Status)`, `now`, and `now`. Assignment back into `agent` is required so the HTTP response contains the persisted defaults/timestamps.

### Get and List

`Get` uses this exact projection and maps `pgx.ErrNoRows` to `ErrAgentNotFound`:

```sql
SELECT id, public_key, capabilities, status, registered_at, last_seen
FROM agents
WHERE id = $1;
```

`List` uses the same projection and a deterministic order:

```sql
SELECT id, public_key, capabilities, status, registered_at, last_seen
FROM agents
ORDER BY registered_at ASC, id ASC;
```

Scanning uses `var publicKey []byte` and `var capabilitiesJSON []byte`. Reject/return an internal scan error if the key length is not 32 or `json.Unmarshal(capabilitiesJSON, &agent.Capabilities)` fails. Convert `publicKey` with a copy, never retain a mutable driver buffer:

```go
key := make(HexKey, ed25519.PublicKeySize)
copy(key, publicKey)
agent.PublicKey = key
```

### Unregister

Deleting the parent row must be the only application query. The foreign key cascade deletes all child inbox rows atomically.

```sql
DELETE FROM agents
WHERE id = $1;
```

When `RowsAffected() == 0`, return `fmt.Errorf("%w: %q", ErrAgentNotFound, id)`; otherwise return nil. Do not issue a separate inbox deletion.

### Deliver

1. Reject `nil`, blank `agentID`, blank `entry.ID` after ID generation, empty or invalid JSON payload, or an expiry at/before creation. `json.Valid(entry.Payload)` is required before any SQL call.
2. Set `entry.AgentID`, default `CreatedAt` to UTC now, default `ExpiresAt` to `CreatedAt.Add(24*time.Hour)`, clear `LeasedAt`, `LeaseID`, and `ACKed`, and generate a crypto-random 16-byte hexadecimal ID when blank.
3. Insert once. A concurrent `Unregister` is handled by the foreign key, not a preflight `Get`.

```sql
INSERT INTO inbox_entries (
    id, agent_id, payload, created_at, expires_at,
    leased_at, lease_id, lease_expires_at, acked
) VALUES ($1, $2, $3::jsonb, $4, $5, NULL, NULL, NULL, FALSE);
```

Map SQLSTATE `23503` (the `agents` foreign key) to `ErrAgentNotFound`, SQLSTATE `23505` to `ErrInvalidStoreInput` for duplicate message ID, and any JSON/input error to `ErrInvalidStoreInput` before SQL execution.

The single `INSERT` is PostgreSQL's atomic durable append operation; no multi-statement transaction is needed for Deliver. It either commits the complete row or commits nothing.

### Retrieve: atomic select and lease

`Retrieve` must reject blank `agentID`, `leaseDuration <= 0`, or `maxMessages <= 0` with `ErrInvalidStoreInput`. It must generate one cryptographically random lease ID for the call, calculate `leaseExpiresAt := now.Add(leaseDuration)`, and execute the following in one read-committed transaction:

1. Lock/check the agent first. This distinguishes an unknown agent from a valid empty inbox and prevents parent deletion during the leasing transaction.

```sql
SELECT 1
FROM agents
WHERE id = $1
FOR KEY SHARE;
```

No row is `ErrAgentNotFound`.

2. Select, lock, update, and return a disjoint FIFO batch with `FOR UPDATE SKIP LOCKED`. `$1` is agent ID; `$2` is now; `$3` is maxMessages; `$4` is lease ID; `$5` is lease expiry.

```sql
WITH candidates AS (
    SELECT id
    FROM inbox_entries
    WHERE agent_id = $1
      AND acked = FALSE
      AND expires_at > $2
      AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
    ORDER BY delivery_sequence ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $3
), leased AS (
    UPDATE inbox_entries AS entry
    SET leased_at = $2,
        lease_id = $4,
        lease_expires_at = $5
    FROM candidates
    WHERE entry.id = candidates.id
    RETURNING entry.id, entry.agent_id, entry.payload, entry.created_at,
              entry.expires_at, entry.leased_at, entry.lease_id, entry.acked,
              entry.delivery_sequence
)
SELECT id, agent_id, payload, created_at, expires_at, leased_at, lease_id, acked
FROM leased
ORDER BY delivery_sequence ASC;
```

3. Commit only after all rows have been scanned. If any query, scan, or commit step fails, rollback and return `nil, "", err`.

The returned `InboxEntry` values expose `LeasedAt` and `LeaseID`; they set `LeaseDuration` to the supplied duration for compatibility, but `lease_expires_at` remains an internal database implementation detail. A valid empty inbox returns `[]*InboxEntry{}`, the newly generated nonempty lease ID, and nil error, matching current handler expectations.

`SKIP LOCKED` is mandatory: concurrent retrievers do not block each other or return the same row. A skipped row may cause one retriever to receive fewer than `maxMessages`; that is correct and the next retrieve can process it.

### Ack: validate then delete atomically

`Ack` checks the agent in a transaction with the same `FOR KEY SHARE` query as Retrieve. An empty `messageIDs` slice is a no-op after the agent existence check, preserving current behavior. Empty `leaseID` and duplicate message IDs return `ErrInvalidStoreInput`.

For nonempty unique IDs, execute this delete inside that transaction:

```sql
DELETE FROM inbox_entries
WHERE agent_id = $1
  AND lease_id = $2
  AND id = ANY($3::text[])
RETURNING id;
```

`$3` is passed as a Go `[]string`; pgx/v5 encodes it as PostgreSQL `text[]`. Compare the number of returned IDs with the number requested before committing. A count mismatch rolls back and returns `fmt.Errorf("%w: expected %d matching messages, got %d", ErrLeaseConflict, len(messageIDs), deleted)`. It intentionally deletes no subset when any ID is missing, belongs to another agent, has a wrong lease, was already acknowledged, or was concurrently re-leased.

### Stats

The agent existence query occurs first. The aggregate query counts all live unacknowledged messages, including actively leased ones, exactly as the current implementation does:

```sql
SELECT
    COUNT(*)::bigint AS queue_depth,
    COUNT(*) FILTER (
        WHERE lease_expires_at IS NOT NULL AND lease_expires_at > $2
    )::bigint AS leased_count,
    MIN(created_at) AS oldest_created_at
FROM inbox_entries
WHERE agent_id = $1
  AND acked = FALSE
  AND expires_at > $2;
```

`$2` is one UTC `now`. Convert the two counts to `int` only after checking they do not exceed the platform `int` limit; otherwise return an error. If `oldest_created_at` is NULL, return age zero. Otherwise return `now.Sub(oldestCreatedAt)` and clamp a negative duration to zero for clock-skew safety.

### PurgeExpired

`PurgeExpired` returns only the number of deleted TTL-expired messages. It must execute the delete and lease release in one transaction with one shared UTC `now`:

```sql
DELETE FROM inbox_entries
WHERE expires_at <= $1;
```

Store `RowsAffected()` as `removed`, then release elapsed leases from still-live rows:

```sql
UPDATE inbox_entries
SET leased_at = NULL,
    lease_id = NULL,
    lease_expires_at = NULL
WHERE expires_at > $1
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= $1;
```

Commit and return `removed`. On begin/query/commit failure, rollback, log `postgres purge expired: <error>`, and return `0`, because the required signature cannot return an error. Retrieval independently treats `lease_expires_at <= now` as eligible, so a delayed 30-second purge ticker never delays redelivery.

---

## 5. Data

### Migration file: `internal/registry/migrations/001_create_agents.up.sql`

```sql
CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    public_key BYTEA NOT NULL,
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    status TEXT NOT NULL DEFAULT 'online',
    registered_at TIMESTAMPTZ NOT NULL,
    last_seen TIMESTAMPTZ NOT NULL,

    CONSTRAINT agents_id_not_blank CHECK (length(btrim(id)) > 0),
    CONSTRAINT agents_public_key_ed25519_length CHECK (octet_length(public_key) = 32),
    CONSTRAINT agents_capabilities_is_array CHECK (jsonb_typeof(capabilities) = 'array'),
    CONSTRAINT agents_status_valid CHECK (status IN ('online', 'offline'))
);

CREATE INDEX agents_status_last_seen_idx
    ON agents (status, last_seen DESC);
```

### Migration file: `internal/registry/migrations/001_create_agents.down.sql`

```sql
DROP TABLE IF EXISTS agents;
```

### Migration file: `internal/registry/migrations/002_create_inbox_entries.up.sql`

```sql
CREATE TABLE inbox_entries (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    payload JSONB NOT NULL,
    delivery_sequence BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    leased_at TIMESTAMPTZ NULL,
    lease_id TEXT NULL,
    lease_expires_at TIMESTAMPTZ NULL,
    acked BOOLEAN NOT NULL DEFAULT FALSE,

    CONSTRAINT inbox_entries_id_not_blank CHECK (length(btrim(id)) > 0),
    CONSTRAINT inbox_entries_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT inbox_entries_lease_fields_consistent CHECK (
        (leased_at IS NULL AND lease_id IS NULL AND lease_expires_at IS NULL)
        OR
        (leased_at IS NOT NULL AND lease_id IS NOT NULL AND lease_id <> ''
         AND lease_expires_at IS NOT NULL AND lease_expires_at > leased_at)
    ),
    CONSTRAINT inbox_entries_agent_id_fkey
        FOREIGN KEY (agent_id)
        REFERENCES agents(id)
        ON DELETE CASCADE
);

-- Required Retrieve predicate index: agent_id + acked + expires_at.
CREATE INDEX inbox_entries_retrieve_idx
    ON inbox_entries (agent_id, acked, expires_at)
    WHERE acked = FALSE;

-- Required FIFO/locking index: prevents sorting an entire agent queue for each retrieve.
CREATE INDEX inbox_entries_fifo_idx
    ON inbox_entries (agent_id, delivery_sequence)
    WHERE acked = FALSE;

-- Required Ack lookup index. The primary-key predicate remains authoritative.
CREATE INDEX inbox_entries_lease_id_idx
    ON inbox_entries (lease_id)
    WHERE lease_id IS NOT NULL;

-- Required TTL purge index.
CREATE INDEX inbox_entries_expires_at_idx
    ON inbox_entries (expires_at);

-- Required expired-lease release index.
CREATE INDEX inbox_entries_lease_expires_at_idx
    ON inbox_entries (lease_expires_at)
    WHERE lease_expires_at IS NOT NULL;
```

`delivery_sequence` and `lease_expires_at` are intentionally additional columns beyond the HTTP `InboxEntry` JSON shape. They solve FIFO ordering and restart-safe variable leases without changing any API payload.

### Migration file: `internal/registry/migrations/002_create_inbox_entries.down.sql`

```sql
DROP TABLE IF EXISTS inbox_entries;
```

### Go-to-database mapping

| Go field | PostgreSQL representation | Encode/decode rule |
|---|---|---|
| `Agent.ID` | `agents.id TEXT` | nonblank string |
| `Agent.PublicKey` | `agents.public_key BYTEA` | `[]byte(HexKey)` on write; copy 32 bytes into `HexKey` on read; JSON remains hex |
| `Agent.Capabilities` | `agents.capabilities JSONB` | `json.Marshal([]string)` / `json.Unmarshal`; nil becomes `[]` |
| `Agent.Status` | `agents.status TEXT` | exact `online` or `offline` |
| `Agent.RegisteredAt`, `LastSeen` | `TIMESTAMPTZ` | UTC `time.Time` |
| `InboxEntry.Payload` | `inbox_entries.payload JSONB` | validate `json.Valid`, pass JSON bytes with `::jsonb`, scan JSON bytes |
| `InboxEntry.CreatedAt`, `ExpiresAt` | `TIMESTAMPTZ` | UTC `time.Time` |
| `InboxEntry.LeasedAt`, `LeaseID` | nullable `TIMESTAMPTZ`, `TEXT` | nil/empty only when all lease fields are null |
| `InboxEntry.LeaseDuration` | no direct column | compatibility value set on Retrieve; canonical persisted deadline is `lease_expires_at` |
| `InboxEntry.ACKed` | `BOOLEAN` | insert false; ACK deletes rather than updates true |

### Embedded migrations

`internal/registry/migrate.go` must embed SQL files and expose only this runner:

```go
package registry

import (
    "context"
    "database/sql"
    "embed"
    "errors"
    "fmt"

    "github.com/golang-migrate/migrate/v4"
    migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
    "github.com/golang-migrate/migrate/v4/source/iofs"
    _ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func RunMigrations(ctx context.Context, connString string) error {
    if connString == "" {
        return fmt.Errorf("migrations: %w: database URL is empty", ErrInvalidStoreInput)
    }

    db, err := sql.Open("pgx", connString)
    if err != nil {
        return fmt.Errorf("open migration database: %w", err)
    }
    defer db.Close()
    if err := db.PingContext(ctx); err != nil {
        return fmt.Errorf("ping migration database: %w", err)
    }

    source, err := iofs.New(migrationFS, "migrations")
    if err != nil {
        return fmt.Errorf("open embedded migrations: %w", err)
    }
    driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
    if err != nil {
        return fmt.Errorf("create migration driver: %w", err)
    }
    m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
    if err != nil {
        return fmt.Errorf("create migrator: %w", err)
    }
    defer m.Close()

    if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
        return fmt.Errorf("apply migrations: %w", err)
    }
    return nil
}
```

Normal startup invokes `m.Up()` only; it never calls `Down`, `Steps(-n)`, or `Drop`. Down migrations are for explicit local/test teardown. A failed migration leaves golang-migrate's dirty version intact, causes startup failure, and requires an operator to inspect and repair it before retrying; automatic forced migration versions are prohibited.

---

## 6. States

### Agent state machine

```text
UNREGISTERED --Register--> REGISTERED_ONLINE --Unregister--> UNREGISTERED
                                 |
                                 +-- (future status writer, not CI-003b) --> REGISTERED_OFFLINE
```

- `UNREGISTERED`: no `agents` row; `Get`, `Deliver`, `Retrieve`, `Ack`, and `Stats` return `ErrAgentNotFound`.
- `REGISTERED_ONLINE`: row has `status = 'online'`; inbox operations are permitted.
- `REGISTERED_OFFLINE`: row has `status = 'offline'`; inbox operations remain permitted because offline delivery is the purpose of the queue. CI-003b introduces no status mutation endpoint.
- Unregister is terminal for the record and cascades every inbox state to deletion.

### Inbox message state machine

```text
DELIVERED_UNLEASED --Retrieve--> LEASED --Ack--> DELETED
        |                           |
        |                           +-- lease_expires_at <= now --> DELIVERED_UNLEASED
        |
        +-- expires_at <= now ----------------------------------> EXPIRED_DELETED

Any state --Unregister agent--> CASCADE_DELETED
```

| State | Database condition | Retrieve result |
|---|---|---|
| `DELIVERED_UNLEASED` | `acked=false`, `expires_at > now`, all lease fields NULL | eligible |
| `LEASED` | `acked=false`, `expires_at > now`, `lease_expires_at > now` | ineligible to other retrievers |
| `LEASE_EXPIRED` | `acked=false`, `expires_at > now`, `lease_expires_at <= now` | immediately eligible; next Retrieve overwrites lease atomically |
| `EXPIRED_DELETED` | row removed by `PurgeExpired` | never returned |
| `DELETED` | row removed by `Ack` | never returned |
| `CASCADE_DELETED` | parent agent deleted | never returned |

The `acked=true` storage state is not a durable operational state: the schema retains the column to preserve the domain type and query predicate, but `Ack` deletes rows rather than marking them. `Deliver` always writes false.

The elapsed-lease predicate used by both retrieval and cleanup is exact and does not rely on a process-local duration:

```sql
lease_expires_at IS NULL OR lease_expires_at <= $now
```

### Startup and shutdown states

```text
CONFIG_LOADED
  -> MIGRATING
  -> POOL_CREATED
  -> POOL_PINGED
  -> SERVING
  -> SHUTTING_DOWN (cancel purge, stop mesh, HTTP shutdown)
  -> POOL_CLOSED
```

Migration failure, pool parse failure, connection failure, or ping failure transitions directly to `STARTUP_FAILED`; no HTTP listener opens. During shutdown, no new requests are accepted after `srv.Shutdown` begins, the purge goroutine exits via `purgeCtx.Done()`, and only then is the pool closed.

---

## 7. Errors

### Configuration and startup errors

| Condition | Error/action | Server behavior |
|---|---|---|
| no configured database URL | `config.Load` returns error | fatal before migration |
| `min_conns < 0`, `max_conns <= 0`, or `min_conns > max_conns` | `config.Load` returns validation error | fatal before migration |
| invalid duration or port | `config.Load` returns validation error | fatal before migration |
| embedded migration cannot open/apply | `NewPostgresStore` wraps `apply migrations` error | fatal before listener |
| database unavailable / TLS failure | pool creation or `Ping` error | fatal before listener |
| dirty migration version | migration error is surfaced unchanged/wrapped | fatal; operator repairs database |

### Store error mapping

| Operation | Condition | Return |
|---|---|---|
| Register | existing ID / SQLSTATE `23505` | `ErrAgentExists` |
| Register | nil/invalid agent, public key, status, capabilities | `ErrInvalidStoreInput` |
| Get | no row | `ErrAgentNotFound` |
| Unregister | no deleted row | `ErrAgentNotFound` |
| Deliver | FK violation / SQLSTATE `23503` | `ErrAgentNotFound` |
| Deliver | invalid payload, expiry, or duplicate ID | `ErrInvalidStoreInput` |
| Retrieve | missing agent | `ErrAgentNotFound` |
| Retrieve | nonpositive lease or max | `ErrInvalidStoreInput` |
| Ack | missing agent | `ErrAgentNotFound` |
| Ack | empty lease or duplicate IDs | `ErrInvalidStoreInput` |
| Ack | a requested row missing or wrong lease | `ErrLeaseConflict` |
| Stats | missing agent | `ErrAgentNotFound` |
| List / PurgeExpired | database failure | log; return empty slice / zero because the fixed interface cannot return an error |
| all non-List operations | timeout, scan, transaction, or driver failure | wrapped underlying error, no partial success |

### Exact configuration

Use `CR_DATABASE_URL` as the namespaced primary variable and `DATABASE_URL` as an interoperability fallback. The existing `CRIER_DATABASE_URL` must remain a final deprecated fallback for one release so `.env.example` and existing deployers do not break. If more than one is set, precedence is `CR_DATABASE_URL`, then `DATABASE_URL`, then `CRIER_DATABASE_URL`.

```go
type DatabaseConfig struct {
    URL             string
    MaxConns        int32
    MinConns        int32
    MaxConnLifetime time.Duration
    MaxConnIdleTime time.Duration
    ConnectTimeout  time.Duration
}

type Config struct {
    Port     int
    Database DatabaseConfig
}
```

`config.Load` changes from `func Load() Config` to `func Load() (Config, error)` and uses these exact settings:

| Variable | Default | Validation |
|---|---:|---|
| `CRIER_PORT` | `8767` | integer 1..65535 |
| `CR_DATABASE_URL` | none | primary URL; nonempty after fallback resolution |
| `DATABASE_URL` | none | fallback URL |
| `CRIER_DATABASE_URL` | none | deprecated final fallback URL |
| `CR_DATABASE_MAX_CONNS` | `4` | integer `> 0` |
| `CR_DATABASE_MIN_CONNS` | `0` | integer `>= 0` and `<= max` |
| `CR_DATABASE_MAX_CONN_LIFETIME` | `30m` | Go duration `> 0` |
| `CR_DATABASE_MAX_CONN_IDLE_TIME` | `5m` | Go duration `> 0` |
| `CR_DATABASE_CONNECT_TIMEOUT` | `10s` | Go duration `> 0` |

Parsing must use `strconv.Atoi`, `strconv.ParseInt`, and `time.ParseDuration`, returning descriptive errors rather than silently applying defaults for malformed values. Never log the resolved URL because it may contain a password.

### HTTP implications

Existing handler validation remains before store invocation. In addition, handler errors are classified with `errors.Is`, not string matching. Database driver errors are logged server-side and translated to a generic HTTP `500` by adding a small `writeStoreError` helper; they must not expose connection strings, SQL, table names, credentials, or PostgreSQL error details to clients. Existing domain errors retain their current `404`/`409` response shape.

```go
func writeStoreError(w http.ResponseWriter, err error) {
    log.Printf("registry store error: %v", err)
    writeJSON(w, http.StatusInternalServerError, map[string]string{
        "error": "registry storage unavailable",
    })
}
```

---

## 8. Testing

### Unit tests: in-memory backend

Keep `internal/registry/registry_test.go` as fast unit coverage. Replace concrete `*Store` helper parameters with the interface:

```go
func setupTestStore(t *testing.T) Store {
    t.Helper()
    return NewMemoryStore()
}

func registerTestAgent(t *testing.T, store Store) *Agent { /* existing helper body */ }
func setupRouter(store Store) *mux.Router {
    handler := NewHandler(store)
    r := mux.NewRouter()
    r.HandleFunc("/agents", handler.HandleRegister).Methods("POST")
    // register the remaining existing routes on handler
    return r
}
```

The existing 26 tests must continue to cover registration, duplicate registration, list/get, unregister, delivery, FIFO retrieve, ACK, wrong lease, lease expiry, TTL purge, stats, max limit, HTTP errors, and HexKey JSON round trip. Run them with:

```bash
make test-short
```

### Integration test environment

Create `internal/registry/postgres_store_integration_test.go` with `//go:build integration`. It starts one `postgres:16-alpine` container through testcontainers-go, runs `NewPostgresStore` (which auto-migrates), and closes the concrete pool in cleanup.

```go
//go:build integration

func setupPostgresTestStore(t *testing.T) *PostgresStore {
    t.Helper()
    ctx := context.Background()
    // Start postgres:16-alpine with POSTGRES_USER=crier, POSTGRES_PASSWORD=crier,
    // POSTGRES_DB=crier_test. Wait for the PostgreSQL log/readiness strategy.
    // Obtain the mapped connection string with sslmode=disable.
    store, err := NewPostgresStoreWithPoolConfig(ctx, connString, PoolConfig{
        MaxConns: 4, MinConns: 0,
        MaxConnLifetime: 30 * time.Minute,
        MaxConnIdleTime: 5 * time.Minute,
    })
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() {
        store.Close()
        // terminate container; fail cleanup only through t.Log
    })
    return store
}
```

To avoid one container per test, a package-level `sync.Once` may start the container. Before each test, execute `TRUNCATE inbox_entries, agents RESTART IDENTITY CASCADE` through the pool. This is allowed only in test code and only against the testcontainer URL.

Add this Makefile target without changing the default CI test target:

```make
.PHONY: test-integration
test-integration:
	go test -tags=integration -count=1 -timeout 5m ./internal/registry
```

### Required PostgreSQL integration cases

1. **Migrations and schema:** starting from empty PostgreSQL creates both tables, all five inbox indexes, all constraints, and the expected golang-migrate version.
2. **Round trip:** register an ed25519 public key and capabilities; Get returns byte-identical key and exact string slice. Deliver a nested JSON payload; Retrieve returns semantically identical JSON.
3. **Duplicate agent:** the second Register maps SQLSTATE `23505` to `ErrAgentExists`.
4. **Missing agent:** Get, Unregister, Deliver, Retrieve, Ack, and Stats each map to `ErrAgentNotFound` as applicable.
5. **FIFO:** deliver messages in known order with caller timestamps deliberately reversed; Retrieve returns ID order matching database `delivery_sequence`, not timestamps.
6. **Concurrent delivery/retrieval:** insert at least 100 messages, run at least four concurrent retrievers with a barrier, collect IDs, and assert every returned ID is unique; total returned IDs equals 100 when no rows are skipped permanently.
7. **Lease expiry:** retrieve with a 50 ms lease, wait until deadline, do not call purge, retrieve again, and verify redelivery. Then call `PurgeExpired` and verify lease fields are NULL in SQL.
8. **ACK atomicity:** lease three rows; acknowledge one correct and one wrong/nonexistent ID together; verify error is `ErrLeaseConflict` and all three rows remain. Acknowledge the valid three IDs and verify all are deleted.
9. **TTL cleanup:** deliver a row whose expiry is in the past or wait for a short expiry; `PurgeExpired` returns exactly one and stats depth is zero.
10. **Foreign-key cascade:** deliver rows, Unregister agent, then query `inbox_entries` directly and assert zero rows for the removed agent.
11. **Pool readiness/lifecycle:** an invalid/closed URL fails `NewPostgresStore`; `Close` is idempotent; a startup `Ping` failure prevents a usable store.
12. **Invalid data:** invalid public key length, invalid status, invalid JSON payload, zero lease duration, zero max messages, and duplicate ACK IDs return `ErrInvalidStoreInput` without inserting or deleting rows.

Run integration coverage locally or in a PostgreSQL-enabled CI job:

```bash
make test-integration
```

### Migration test requirements

- Run `RunMigrations` twice and require the second call to succeed via `migrate.ErrNoChange` handling.
- From an empty database, apply `001` then `002`; verify the FK points to `agents(id)` with `ON DELETE CASCADE`.
- In a disposable test DB only, apply the down migration for `002`, then `001`, and assert neither application table exists.
- A migration failure/dirty migration must fail startup; never test or implement automatic `force` repair.

---

## 9. Security

1. **Credentials:** database URLs are secrets. Read them only from environment; do not place a production URL in Go source, test fixtures, migration files, logs, errors returned to HTTP clients, or committed `.env` files.
2. **Transport:** production URLs require TLS (`sslmode=require` or stronger). Local/test URLs use `sslmode=disable` only on loopback/testcontainer networks.
3. **Least privilege:** the runtime role requires `CONNECT`, `USAGE` on schema, and DML on `agents`/`inbox_entries`; the migration role additionally requires DDL. Deployments may use distinct migration and runtime roles, but CI-003b's startup runner requires a DDL-capable role until migration responsibility is split.
4. **SQL injection:** every value uses `$n` placeholders. Dynamic table/column/order strings are prohibited. `agentID`, message IDs, lease IDs, payload JSON, and capability JSON must never be concatenated into SQL.
5. **Payload safety:** validate JSON with `json.Valid` before `payload::jsonb` insertion. JSONB stores structured data but does not execute it. The handler's response remains JSON encoded, not string-concatenated.
6. **Key integrity:** enforce 32-byte ed25519 public keys in both Go and `octet_length(public_key) = 32`. PostgreSQL stores raw bytes; only JSON/API boundaries hex-encode through `HexKey`.
7. **Lease secrecy:** lease IDs use 16 random bytes from `crypto/rand` encoded as 32 lowercase hexadecimal characters. Never use timestamps, UUID math/rand, or database sequence values for leases.
8. **Authorization boundary:** this task preserves the current unauthenticated API. It must not claim that persistence provides authentication. A later authentication task must enforce whether a caller can retrieve/ack an agent's inbox.
9. **Availability controls:** five-second operation deadlines, bounded `max` validation in handlers, pool limits, FK enforcement, and partial indexes reduce accidental resource exhaustion. The handler must cap `max` at `100` before calling Store; a larger value returns HTTP `400`.
10. **Retention:** payloads are deleted at `expires_at` by the 30-second purger and are cascaded when the agent unregisters. PostgreSQL backups may retain data according to infrastructure backup policy; CI-003b introduces no audit archive.

The only allowed SQL construction form is parameterized execution; this example is normative for all user-controlled values:

```go
_, err := s.pool.Exec(ctx,
    "DELETE FROM agents WHERE id = $1",
    agentID,
)
```

---

## 10. Performance

### Pool settings and lifecycle

The default pool is deliberately small for the initial service:

```go
poolCfg, err := pgxpool.ParseConfig(connString)
if err != nil { /* return wrapped error */ }
poolCfg.MaxConns = poolConfig.MaxConns        // default 4
poolCfg.MinConns = poolConfig.MinConns        // default 0
poolCfg.MaxConnLifetime = poolConfig.MaxConnLifetime // default 30m
poolCfg.MaxConnIdleTime = poolConfig.MaxConnIdleTime // default 5m
pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
if err != nil { /* return wrapped error */ }
if err := pool.Ping(ctx); err != nil {
    pool.Close()
    return nil, fmt.Errorf("ping PostgreSQL pool: %w", err)
}
```

A pool is process-local. Its `MaxConns` must be sized with all service replicas in mind: `replica_count * CR_DATABASE_MAX_CONNS` must remain below the database role/instance connection budget with headroom for migrations and administration. Do not create a pool per request or per Store method.

### Query and index targets

| Operation | Query shape | Required target |
|---|---|---|
| Register / Deliver / Ack / Unregister | one insert/delete transaction | p95 < 25 ms on local-region PostgreSQL |
| Get / Stats | indexed point lookup + aggregate | p95 < 20 ms |
| List | ordered table scan | p95 < 100 ms for 10,000 agents |
| Retrieve | `FOR UPDATE SKIP LOCKED`, up to 100 rows | p95 < 50 ms for a 100-row batch under four concurrent consumers |
| PurgeExpired | expiry index scan plus lease-expiry index scan | p95 < 100 ms for 100,000 live inbox rows |

Required index-to-query mapping:

- `inbox_entries_retrieve_idx (agent_id, acked, expires_at)` filters live candidates for Retrieve and directly satisfies the required agent/ACK/expiry access path.
- `inbox_entries_fifo_idx (agent_id, delivery_sequence) WHERE acked = FALSE` supplies ordered candidate scanning without sorting a full agent inbox.
- `inbox_entries_lease_id_idx` supports lease-related inspection and the lease component of ACK predicates.
- `inbox_entries_expires_at_idx` supports TTL deletion.
- `inbox_entries_lease_expires_at_idx` supports un-leasing elapsed leases.
- the `agents` primary key serves Get, Unregister, and the agent existence checks.

Run `EXPLAIN (ANALYZE, BUFFERS)` against a seeded development database before changing predicates or index order. Any change that removes `SKIP LOCKED`, the `delivery_sequence` order, `expires_at` eligibility filter, or atomic Ack validation is a correctness regression even if a benchmark appears faster.

### Retention, batching, and observability

- The existing purge ticker remains `30 * time.Second`; it calls `regStore.PurgeExpired()` and logs only failures (the method itself returns only a count).
- Handler validation caps `max` at 100. The default remains 10. No unbounded Retrieve query is permitted.
- Messages are retained at most their explicit TTL plus the purge interval; retrieval excludes rows at or after expiry immediately, so the ticker affects storage reclamation rather than delivery semantics.
- Add structured logs/metrics at the composition layer in a later observability task; CI-003b must at minimum log migration, startup, List, and purge database failures without values that could contain credentials or payloads.
- Do not add an ORM, a database trigger, polling worker, or N+1 agent lookups. pgxpool plus the listed SQL is the sole persistence path.

### Verification gate

After implementation, the required commands are:

```bash
make test-short
make test-integration
make lint
make build
```

A schema/query review must additionally confirm:

```bash
# Applied migrations in the test database show versions 1 and 2 and no dirty state.
# EXPLAIN is executed manually against the Retrieve query with representative rows.
# Integration test verifies concurrent retrievers receive no duplicate message IDs.
```
