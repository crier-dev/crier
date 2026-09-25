package registry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crier-dev/crier/internal/namespace"
)

// PoolConfig is backend configuration after config.Load has parsed environment values.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DefaultPoolConfig returns a PoolConfig with sensible defaults:
// 4 max connections, no minimum, 30-minute connection lifetime,
// 5-minute idle timeout.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxConns:        4,
		MinConns:        0,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// connPool is the subset of *pgxpool.Pool methods that PostgresStore needs.
// Using an interface enables mock-based unit tests without a real database.
type connPool interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
	Ping(ctx context.Context) error
	Close()
}

// PostgresStore is a pgxpool-backeded implementation of Store.
type PostgresStore struct {
	pool connPool

	// listMu guards listErr, the recorded failure of the LAST List call
	// (nil when it succeeded), exposed via ListError so bridge callers
	// (the MCP list_agents surface) can tell a failing database from an
	// empty registry (DF-CRIER-200). Same last-call semantics as
	// RemoteStore (DF-CRIER-199): ListError describes the last call only,
	// not a request-local atomic pair.
	listMu  sync.Mutex
	listErr error
}

var _ Store = (*PostgresStore)(nil)

// Compile-time capability assertion: PostgresStore implements the optional
// ListErrorReporter Store capability (store.go, DF-CRIER-199/200).
var _ ListErrorReporter = (*PostgresStore)(nil)

// Compile-time capability assertion: PostgresStore records liveness evidence
// (the mesh heartbeat path, presence.go, CR-FEAT-024). HeartbeatSink depends on
// this interface, so a rename here must fail the build rather than silently
// wiring no sink — a registry that stops recording heartbeats goes back to
// reporting a crashed agent as online.
var _ Toucher = (*PostgresStore)(nil)

// setListError records the failure of the current List call.
func (s *PostgresStore) setListError(err error) {
	s.listMu.Lock()
	s.listErr = err
	s.listMu.Unlock()
}

// ListError returns the failure of the last List call, nil when it
// succeeded. It implements the optional ListErrorReporter Store capability
// (store.go), which callers like the MCP bridge's list_agents handler use
// to answer with an error instead of a laundered empty agent list.
func (s *PostgresStore) ListError() error {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	return s.listErr
}

// NewPostgresStore opens a pgxpool, runs pending migrations, and returns
// a ready-to-use PostgresStore. Uses DefaultPoolConfig for pool settings.
func NewPostgresStore(ctx context.Context, connString string) (*PostgresStore, error) {
	return NewPostgresStoreWithPoolConfig(ctx, connString, DefaultPoolConfig())
}

// NewPostgresStoreWithPoolConfig opens a pgxpool with the given pool
// configuration, runs pending migrations, and returns a PostgresStore.
// Validates the connection string and pool configuration before connecting.
func NewPostgresStoreWithPoolConfig(ctx context.Context, connString string, poolConfig PoolConfig) (*PostgresStore, error) {
	if connString == "" {
		return nil, fmt.Errorf("postgres store: %w: connection string is empty", ErrInvalidStoreInput)
	}

	if err := RunMigrations(ctx, connString); err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	poolCfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	poolCfg.MaxConns = poolConfig.MaxConns
	poolCfg.MinConns = poolConfig.MinConns
	poolCfg.MaxConnLifetime = poolConfig.MaxConnLifetime
	poolCfg.MaxConnIdleTime = poolConfig.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL pool: %w", err)
	}

	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// operationContext returns a short-lived context for individual DB operations.
func (s *PostgresStore) operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// agentConfigColumns is the column list shared by Get and List, in the exact
// order both scan it: the six registration columns followed by the three
// optional configs added by 003_add_agent_config_columns and
// 005_add_agent_a2a_column, and the realm added by 007_add_namespaces.
//
// `namespace` is COALESCEd to ” because NULL is the canonical storage of the
// DEFAULT namespace (007): an existing row — and every new row in a deployment
// that declares no namespaces — reads back as "", exactly what the in-memory
// backend and the wire form use.
const agentConfigColumns = `id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, COALESCE(namespace, '')`

// marshalOptionalConfig marshals one of an agent's optional configs (webhook,
// guard, a2a) for its nullable JSONB column. A nil config — absent, or
// explicitly null on the wire — yields a nil slice, which pgx encodes as SQL
// NULL (pgtype.Map.Encode documents the nil return as "the SQL value NULL"; the
// JSONB plan returns (nil, nil) for a nil []byte). A config that cannot be
// marshalled is reported as ErrInvalidStoreInput rather than persisted as
// nothing: silent acceptance of an unusable config is the DF-CRIER-151
// failure shape this backend must not reproduce.
func marshalOptionalConfig[T any](cfg *T) ([]byte, error) {
	if cfg == nil {
		return nil, nil
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal agent config: %v", ErrInvalidStoreInput, err)
	}
	return raw, nil
}

// unmarshalOptionalConfig is the read-side inverse of marshalOptionalConfig:
// SQL NULL (a nil src, which is what the JSONB scan plan yields for NULL) and
// a stored JSON null both leave dst nil — the wire shape stays "config
// absent", identical to the in-memory backend — while anything else is
// decoded into a fresh value.
func unmarshalOptionalConfig[T any](raw []byte, dst **T) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		*dst = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*dst = &v
	return nil
}

// Register adds an agent. Maps duplicate PK to ErrAgentExists.
func (s *PostgresStore) Register(agent *Agent) error {
	if agent == nil || agent.ID == "" {
		return fmt.Errorf("%w: nil agent or blank ID", ErrInvalidStoreInput)
	}
	// An empty key is a keyless agent (DF-CRIER-192): registered while
	// signature enforcement was off, stored as SQL NULL by migration 004.
	// Any non-empty key must still be exactly one ed25519 public key.
	if len(agent.PublicKey) != ed25519.PublicKeySize && len(agent.PublicKey) != 0 {
		return fmt.Errorf("%w: public key must be %d bytes", ErrInvalidStoreInput, ed25519.PublicKeySize)
	}
	// Only the two STORED statuses are registrable (migrations/001's CHECK
	// constraint is exactly this set). "stale" is deliberately absent: it is
	// DERIVED per read from `last_seen` (presence.go, CR-FEAT-024) and a caller
	// registering a row as stale is claiming a conclusion about evidence the
	// row does not have yet — the next read would derive its real value anyway.
	if agent.Status != "" && agent.Status != StatusOnline && agent.Status != StatusOffline {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidStoreInput, agent.Status)
	}

	caps := agent.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("%w: marshal capabilities: %v", ErrInvalidStoreInput, err)
	}
	webhookJSON, err := marshalOptionalConfig(agent.Webhook)
	if err != nil {
		return err
	}
	guardJSON, err := marshalOptionalConfig(agent.Guard)
	if err != nil {
		return err
	}
	a2aJSON, err := marshalOptionalConfig(agent.A2A)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	status := agent.Status
	if status == "" {
		status = StatusOnline
	}

	// A keyless agent stores SQL NULL, not an empty bytea: migration 004's
	// CHECK still requires 32 bytes for any non-null value, and NULL is the
	// canonical "no key" representation (nil slice → pgx encodes NULL, the
	// same contract as marshalOptionalConfig).
	var pubKeyArg []byte
	if len(agent.PublicKey) > 0 {
		pubKeyArg = []byte(agent.PublicKey)
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	_, err = s.pool.Exec(ctx, `
INSERT INTO agents (
    id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, namespace
) VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10);`,
		agent.ID, pubKeyArg, capsJSON, string(status), now, now, webhookJSON, guardJSON, a2aJSON,
		nullText(namespace.Canonical(agent.Namespace)),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %q", ErrAgentExists, agent.ID)
		}
		return fmt.Errorf("register agent: %w", err)
	}

	// Assign persisted defaults back to the caller's object.
	agent.RegisteredAt = now
	agent.LastSeen = now
	agent.Status = status
	agent.Capabilities = caps
	return nil
}

// Get returns an agent by ID, mapping ErrNoRows to ErrAgentNotFound.
func (s *PostgresStore) Get(id string) (*Agent, error) {
	ctx, cancel := s.operationContext()
	defer cancel()

	var (
		agent            Agent
		publicKey        []byte
		capabilitiesJSON []byte
		webhookJSON      []byte
		guardJSON        []byte
		a2aJSON          []byte
	)
	err := s.pool.QueryRow(ctx, `
SELECT `+agentConfigColumns+`
FROM agents
WHERE id = $1;`, id).Scan(
		&agent.ID, &publicKey, &capabilitiesJSON, &agent.Status, &agent.RegisteredAt, &agent.LastSeen,
		&webhookJSON, &guardJSON, &a2aJSON, &agent.Namespace,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
		}
		return nil, fmt.Errorf("get agent: %w", err)
	}

	// A NULL/empty stored key is a keyless agent (DF-CRIER-192): registered
	// while signature enforcement was off. It decodes as an empty HexKey —
	// identical to the in-memory backend's representation — and stays
	// unusable on every signed route (agentsig fails closed). Any other
	// non-32-byte length is genuinely corrupt data.
	if len(publicKey) != ed25519.PublicKeySize && len(publicKey) != 0 {
		return nil, fmt.Errorf("get agent: public key length %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	key := make(HexKey, len(publicKey))
	copy(key, publicKey)
	agent.PublicKey = key

	if err := json.Unmarshal(capabilitiesJSON, &agent.Capabilities); err != nil {
		return nil, fmt.Errorf("get agent: unmarshal capabilities: %w", err)
	}
	if err := unmarshalOptionalConfig(webhookJSON, &agent.Webhook); err != nil {
		return nil, fmt.Errorf("get agent: unmarshal webhook: %w", err)
	}
	if err := unmarshalOptionalConfig(guardJSON, &agent.Guard); err != nil {
		return nil, fmt.Errorf("get agent: unmarshal guard: %w", err)
	}
	if err := unmarshalOptionalConfig(a2aJSON, &agent.A2A); err != nil {
		return nil, fmt.Errorf("get agent: unmarshal a2a: %w", err)
	}
	return &agent, nil
}

// List returns all agents ordered by registration time then ID.
// Per the Store contract, List cannot return an error; on failure it logs and
// returns a non-nil empty slice. The failure is also recorded for ListError,
// so the MCP bridge's list_agents surface can tell a failing database from
// an empty registry (DF-CRIER-200). ListError describes the LAST call: a
// successful List clears it.
func (s *PostgresStore) List() []*Agent {
	ctx, cancel := s.operationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
SELECT `+agentConfigColumns+`
FROM agents
ORDER BY registered_at ASC, id ASC;`)
	if err != nil {
		slog.Error("postgres list", "error", err)
		s.setListError(fmt.Errorf("postgres list: query: %w", err))
		return []*Agent{}
	}
	defer rows.Close()

	out := make([]*Agent, 0)
	for rows.Next() {
		var (
			agent            Agent
			publicKey        []byte
			capabilitiesJSON []byte
			webhookJSON      []byte
			guardJSON        []byte
			a2aJSON          []byte
		)
		if err := rows.Scan(&agent.ID, &publicKey, &capabilitiesJSON, &agent.Status, &agent.RegisteredAt, &agent.LastSeen,
			&webhookJSON, &guardJSON, &a2aJSON, &agent.Namespace); err != nil {
			slog.Error("postgres list scan", "error", err)
			s.setListError(fmt.Errorf("postgres list: scan: %w", err))
			return []*Agent{}
		}
		// NULL/empty stored key = keyless agent (DF-CRIER-192): keep the
		// row, decode as an empty HexKey (see Get). Any other non-32-byte
		// length is corrupt and drops the whole listing, as before.
		if len(publicKey) != ed25519.PublicKeySize && len(publicKey) != 0 {
			slog.Warn("postgres list: invalid public key", "len", len(publicKey), "agent_id", agent.ID)
			s.setListError(fmt.Errorf("postgres list: agent %q: invalid public key: length %d, want %d",
				agent.ID, len(publicKey), ed25519.PublicKeySize))
			return []*Agent{}
		}
		key := make(HexKey, len(publicKey))
		copy(key, publicKey)
		agent.PublicKey = key
		if err := json.Unmarshal(capabilitiesJSON, &agent.Capabilities); err != nil {
			slog.Error("postgres list: unmarshal capabilities", "error", err)
			s.setListError(fmt.Errorf("postgres list: agent %q: unmarshal capabilities: %w", agent.ID, err))
			return []*Agent{}
		}
		if err := unmarshalOptionalConfig(webhookJSON, &agent.Webhook); err != nil {
			slog.Error("postgres list: unmarshal webhook", "error", err, "agent_id", agent.ID)
			s.setListError(fmt.Errorf("postgres list: agent %q: unmarshal webhook: %w", agent.ID, err))
			return []*Agent{}
		}
		if err := unmarshalOptionalConfig(guardJSON, &agent.Guard); err != nil {
			slog.Error("postgres list: unmarshal guard", "error", err, "agent_id", agent.ID)
			s.setListError(fmt.Errorf("postgres list: agent %q: unmarshal guard: %w", agent.ID, err))
			return []*Agent{}
		}
		if err := unmarshalOptionalConfig(a2aJSON, &agent.A2A); err != nil {
			slog.Error("postgres list: unmarshal a2a", "error", err, "agent_id", agent.ID)
			s.setListError(fmt.Errorf("postgres list: agent %q: unmarshal a2a: %w", agent.ID, err))
			return []*Agent{}
		}
		out = append(out, &agent)
	}
	if err := rows.Err(); err != nil {
		slog.Error("postgres list rows", "error", err)
		s.setListError(fmt.Errorf("postgres list: rows: %w", err))
		return []*Agent{}
	}
	s.setListError(nil)
	return out
}

// Unregister deletes an agent; the FK cascade removes inbox rows atomically.
func (s *PostgresStore) Unregister(id string) error {
	ctx, cancel := s.operationContext()
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
DELETE FROM agents
WHERE id = $1;`, id)
	if err != nil {
		return fmt.Errorf("unregister agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return nil
}

// Update replaces the mutable registration fields of an existing agent — the
// PATCH /agents/{id} path (CR-FEAT-007). The Postgres backend persists all
// three mutable fields: capabilities, webhook and guard are each written to
// their column, and an explicit nil (webhook absent or null in the PATCH
// body, spec §7) writes SQL NULL — so "removes the webhook" means removed on
// this backend, not accepted-and-ignored (DF-CRIER-151).
// last_seen is advanced to the moment of the write and assigned back to the
// caller's agent, so a caller rendering the returned object (HandleUpdateAgent
// does) reports the value that is now persisted rather than the pre-write one
// (DF-CRIER-156). The timestamp is generated once and used both for the
// statement and for the caller.
// Returns ErrAgentNotFound when the agent does not exist.
func (s *PostgresStore) Update(agent *Agent) error {
	if agent == nil || agent.ID == "" {
		return fmt.Errorf("%w: nil agent or blank ID", ErrInvalidStoreInput)
	}
	caps := agent.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("%w: marshal capabilities: %v", ErrInvalidStoreInput, err)
	}
	webhookJSON, err := marshalOptionalConfig(agent.Webhook)
	if err != nil {
		return err
	}
	guardJSON, err := marshalOptionalConfig(agent.Guard)
	if err != nil {
		return err
	}
	a2aJSON, err := marshalOptionalConfig(agent.A2A)
	if err != nil {
		return err
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	// Postgres timestamps have microsecond resolution and TRUNCATE the
	// nanoseconds (measured against real PG: 300/300 samples stored
	// now.Truncate(time.Microsecond), 153/300 diverged from now.Round).
	// Truncating here means the value handed back to the caller is exactly the
	// instant the row holds — a nanosecond-precision value would make the
	// PATCH 200 body differ from the next GET by sub-microsecond precision.
	now := time.Now().UTC().Truncate(time.Microsecond)
	tag, err := s.pool.Exec(ctx, `
UPDATE agents
SET capabilities = $2::jsonb, webhook = $3::jsonb, guard = $4::jsonb, a2a = $5::jsonb, last_seen = $6
WHERE id = $1;`, agent.ID, capsJSON, webhookJSON, guardJSON, a2aJSON, now)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agent.ID)
	}
	// The row now carries `now`; hand the same instant back to the caller so
	// the PATCH response equals what a following GET returns (DF-CRIER-156).
	// Assigned only after the write succeeded — a failed update must not
	// report a last_seen that was never persisted.
	agent.LastSeen = now
	return nil
}

// Touch advances an agent's last_seen from a liveness signal — the mesh
// heartbeat path (CR-FEAT-024, registry.Toucher). It writes ONE column: the
// registration fields, the stored status and every other field are untouched,
// so a heartbeat can never reshape a row it is only supposed to date.
//
// GREATEST makes the write MONOTONIC: a heartbeat that arrives with a timestamp
// older than the stored one (clock adjustment, a replayed frame) leaves
// last_seen where it was. A bare assignment would let such a frame make a live
// agent look older than its real evidence — the same class of lie the heartbeat
// exists to remove. RowsAffected still distinguishes the only other outcome:
// no such row (ErrAgentNotFound).
//
// Truncated to microseconds before the write, matching Update: Postgres stores
// microsecond precision, and truncating here means the value the store applied
// is exactly the value the column holds.
//
// Returns ErrAgentNotFound for an unknown id — the ordinary case for a peer
// that connected without a registry row, which the mesh sink logs at debug.
func (s *PostgresStore) Touch(id string, at time.Time) error {
	if id == "" {
		return fmt.Errorf("%w: blank ID", ErrInvalidStoreInput)
	}
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC().Truncate(time.Microsecond)

	ctx, cancel := s.operationContext()
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
UPDATE agents
SET last_seen = GREATEST(last_seen, $2)
WHERE id = $1;`, id, at)
	if err != nil {
		return fmt.Errorf("touch agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return nil
}

// nullText maps an absent string to SQL NULL, so "not recorded" has exactly one
// representation in the database (CR-FEAT-025): an empty sender or idempotency
// key stores NULL, never ”, and reads back as the empty string either way.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pgTimestamptz renders a message expiry for the inbox_entries.expires_at
// column. The zero time means "never expires" (ttl_seconds=0, DF-CRIER-37):
// Postgres has no zero time.Time, and the column is NOT NULL with a
// `CHECK (expires_at > created_at)` constraint, so "never" is stored as the
// native timestamptz `infinity` — which satisfies the CHECK, is never matched
// by the purge predicate (expires_at <= now), and always passes the claim
// predicate (expires_at > now). No schema change is needed.
func pgTimestamptz(expiry time.Time) pgtype.Timestamptz {
	if expiry.IsZero() {
		return pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}
	}
	return pgtype.Timestamptz{Time: expiry, Valid: true}
}

// expiryFromTimestamptz is the read-side inverse of pgTimestamptz: a stored
// `infinity` decodes back to the zero time, so the Postgres backend reports
// the same never-expires representation as the in-memory backend. Since
// DF-CRIER-182 that zero time is rendered as JSON null on the wire (the key
// present, the value null) — it is never sent as the zero time
// 0001-01-01T00:00:00Z. Reading into a plain time.Time would fail outright:
// pgx refuses `infinity` for a *time.Time destination.
func expiryFromTimestamptz(ts pgtype.Timestamptz) time.Time {
	switch ts.InfinityModifier {
	case pgtype.Infinity:
		return time.Time{}
	case pgtype.NegativeInfinity:
		// crier never writes this; treat it as long expired rather than as
		// never-expiring, which is the safe direction.
		return time.Unix(0, 0).UTC()
	default:
		return ts.Time.UTC()
	}
}

// Deliver appends a message to an agent's FIFO inbox.
func (s *PostgresStore) Deliver(agentID string, entry *InboxEntry) error {
	if agentID == "" {
		return fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if entry == nil {
		return fmt.Errorf("%w: nil inbox entry", ErrInvalidStoreInput)
	}
	if entry.ID == "" {
		id, err := newLeaseID()
		if err != nil {
			return fmt.Errorf("generate message id: %w", err)
		}
		entry.ID = id
	}
	if !json.Valid(entry.Payload) {
		return fmt.Errorf("%w: invalid JSON payload", ErrInvalidStoreInput)
	}

	entry.AgentID = agentID
	now := time.Now().UTC()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	} else {
		entry.CreatedAt = entry.CreatedAt.UTC()
	}
	// Apply the delivery's requested lifetime (DF-CRIER-37): ttl_seconds > 0
	// → that many seconds, ttl_seconds == 0 → never expires (ExpiresAt stays
	// the zero time), absent → the 24h default.
	if err := resolveMessageExpiry(entry); err != nil {
		return err
	}
	if !entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = entry.ExpiresAt.UTC()
		if !entry.ExpiresAt.After(entry.CreatedAt) {
			return fmt.Errorf("%w: expiry at or before creation", ErrInvalidStoreInput)
		}
	}

	// Clear lease/ack fields on deliver.
	entry.LeasedAt = nil
	entry.LeaseID = ""
	entry.ACKed = false

	ctx, cancel := s.operationContext()
	defer cancel()

	// sender and idempotency_key are stored WITH the message (CR-FEAT-025): the
	// first is the address a MESSAGE_EXPIRED receipt is sent to if this message
	// expires unacknowledged, the second is the provenance a dead letter
	// reports. An absent value is stored as SQL NULL — never as an empty string
	// — so "not recorded" has exactly one representation.
	_, err := s.pool.Exec(ctx, `
INSERT INTO inbox_entries (
    id, agent_id, payload, sender, idempotency_key, created_at, expires_at,
    leased_at, lease_id, lease_expires_at, acked, namespace
) VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, NULL, NULL, NULL, FALSE, $8);`,
		entry.ID, agentID, entry.Payload, nullText(entry.Sender), nullText(entry.IdempotencyKey),
		entry.CreatedAt, pgTimestamptz(entry.ExpiresAt),
		nullText(namespace.Canonical(entry.Namespace)),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23503":
				return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
			case "23505":
				return fmt.Errorf("%w: duplicate message ID %q", ErrInvalidStoreInput, entry.ID)
			}
		}
		return fmt.Errorf("deliver: %w", err)
	}
	return nil
}

// Retrieve atomically selects, locks, and leases a disjoint FIFO batch.
// The lease ID is minted only once a non-empty batch is locked, so an empty
// or fully leased inbox returns a non-nil empty slice and an empty lease ID
// (DF-CRIER-32). The row locks taken by the claiming SELECT are held until
// the transaction commits, which keeps concurrent retrievers disjoint.
func (s *PostgresStore) Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error) {
	if agentID == "" {
		return nil, "", fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if leaseDuration <= 0 {
		return nil, "", fmt.Errorf("%w: lease duration must be positive", ErrInvalidStoreInput)
	}
	if maxMessages <= 0 {
		return nil, "", fmt.Errorf("%w: max messages must be positive", ErrInvalidStoreInput)
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("retrieve begin: %w", err)
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	leaseExpiresAt := now.Add(leaseDuration)

	// 1. Lock/check the agent first.
	var agentCheck int
	err = tx.QueryRow(ctx, `
SELECT 1
FROM agents
WHERE id = $1
FOR KEY SHARE;`, agentID).Scan(&agentCheck)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
		}
		return nil, "", fmt.Errorf("retrieve agent check: %w", err)
	}

	// 2. Claim a disjoint FIFO batch: lock the candidate rows (skipping rows
	// a concurrent retriever already locked) and read them back. The locks
	// live until commit, so nothing needs to be re-checked when the batch is
	// stamped below.
	rows, err := tx.Query(ctx, `
SELECT id, agent_id, payload, COALESCE(sender, ''), COALESCE(idempotency_key, ''),
       created_at, expires_at, COALESCE(namespace, '')
FROM inbox_entries
WHERE agent_id = $1
  AND acked = FALSE
  AND expires_at > $2
  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
ORDER BY delivery_sequence ASC
FOR UPDATE SKIP LOCKED
LIMIT $3;`,
		agentID, now, maxMessages,
	)
	if err != nil {
		return nil, "", fmt.Errorf("retrieve select: %w", err)
	}

	result := make([]*InboxEntry, 0)
	for rows.Next() {
		var entry InboxEntry
		// expires_at is read through pgtype.Timestamptz so a stored
		// `infinity` (ttl_seconds=0 → never expires, DF-CRIER-37) decodes
		// instead of erroring, then normalizes to the zero time.
		var expiresAt pgtype.Timestamptz
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.Sender,
			&entry.IdempotencyKey, &entry.CreatedAt, &expiresAt, &entry.Namespace); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("retrieve scan: %w", err)
		}
		entry.ExpiresAt = expiryFromTimestamptz(expiresAt)
		result = append(result, &entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("retrieve rows: %w", err)
	}

	// Nothing claimable: no lease is minted, and the transaction (which wrote
	// nothing) rolls back via the deferred Rollback.
	if len(result) == 0 {
		return []*InboxEntry{}, "", nil
	}

	leaseID, err := newLeaseID()
	if err != nil {
		return nil, "", fmt.Errorf("generate lease id: %w", err)
	}

	ids := make([]string, 0, len(result))
	for _, entry := range result {
		ids = append(ids, entry.ID)
		entry.LeaseID = leaseID
		entry.LeaseDuration = leaseDuration
		leasedAt := now
		entry.LeasedAt = &leasedAt
	}

	tag, err := tx.Exec(ctx, `
UPDATE inbox_entries
SET leased_at = $2,
    lease_id = $3,
    lease_expires_at = $4
WHERE agent_id = $1
  AND id = ANY($5::text[]);`,
		agentID, now, leaseID, leaseExpiresAt, ids,
	)
	if err != nil {
		return nil, "", fmt.Errorf("retrieve lease: %w", err)
	}
	if int(tag.RowsAffected()) != len(ids) {
		return nil, "", fmt.Errorf("retrieve lease: leased %d of %d locked candidate message(s)", tag.RowsAffected(), len(ids))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("retrieve commit: %w", err)
	}

	return result, leaseID, nil
}

// Ack validates then deletes messages atomically inside a transaction.
// An ID that does not exist in the inbox is reported as ErrMessageNotFound; an
// ID that exists under a different (or no) lease is reported as
// ErrLeaseConflict. Missing IDs take precedence so an all-unknown request is
// never mistaken for a stale-lease problem (DF-CRIER-32).
func (s *PostgresStore) Ack(agentID, leaseID string, messageIDs []string) error {
	if agentID == "" {
		return fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if leaseID == "" {
		return fmt.Errorf("%w: blank lease ID", ErrInvalidStoreInput)
	}
	// Check for duplicate message IDs.
	seen := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		if seen[id] {
			return fmt.Errorf("%w: duplicate message ID %q", ErrInvalidStoreInput, id)
		}
		seen[id] = true
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ack begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Agent existence check.
	var agentCheck int
	err = tx.QueryRow(ctx, `
SELECT 1
FROM agents
WHERE id = $1
FOR KEY SHARE;`, agentID).Scan(&agentCheck)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
		}
		return fmt.Errorf("ack agent check: %w", err)
	}

	// Empty messageIDs must be rejected: a lease-only ack is a silent no-op
	// that leaves messages queued for redelivery (CR-GAP-014).
	if len(messageIDs) == 0 {
		return fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}

	// Classify every requested ID before deleting: an ID absent from the
	// inbox is a not-found (404) while an ID present under another lease is a
	// lease conflict (409). Both used to share one sentinel and one status.
	lookup, err := tx.Query(ctx, `
SELECT id, COALESCE(lease_id, '') AS lease_id
FROM inbox_entries
WHERE agent_id = $1
  AND id = ANY($2::text[]);`, agentID, messageIDs)
	if err != nil {
		return fmt.Errorf("ack lookup: %w", err)
	}

	currentLease := make(map[string]string, len(messageIDs))
	for lookup.Next() {
		var (
			id       string
			existing string
		)
		if err := lookup.Scan(&id, &existing); err != nil {
			lookup.Close()
			return fmt.Errorf("ack lookup scan: %w", err)
		}
		currentLease[id] = existing
	}
	lookup.Close()
	if err := lookup.Err(); err != nil {
		return fmt.Errorf("ack lookup rows: %w", err)
	}

	var missing, mismatched []string
	for _, id := range messageIDs {
		current, ok := currentLease[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if current != leaseID {
			mismatched = append(mismatched, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: message id(s) %s do not exist in agent %q's inbox (never delivered, already acknowledged, or expired)",
			ErrMessageNotFound, quoteMessageIDs(missing), agentID)
	}
	if len(mismatched) > 0 {
		return fmt.Errorf("%w: message %q is not leased under lease %q (current lease: %q)",
			ErrLeaseConflict, mismatched[0], leaseID, currentLease[mismatched[0]])
	}

	rows, err := tx.Query(ctx, `
DELETE FROM inbox_entries
WHERE agent_id = $1
  AND lease_id = $2
  AND id = ANY($3::text[])
RETURNING id;`, agentID, leaseID, messageIDs)
	if err != nil {
		return fmt.Errorf("ack delete: %w", err)
	}

	deleted := 0
	for rows.Next() {
		deleted++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ack delete rows: %w", err)
	}

	if deleted != len(messageIDs) {
		return fmt.Errorf("%w: expected %d matching messages, got %d", ErrLeaseConflict, len(messageIDs), deleted)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ack commit: %w", err)
	}
	return nil
}

// Stats returns queue depth, leased count, and oldest message age.
func (s *PostgresStore) Stats(agentID string) (queueDepth, leasedCount int, oldestAge time.Duration, err error) {
	if agentID == "" {
		return 0, 0, 0, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("stats begin: %w", err)
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()

	// Agent existence check.
	var agentCheck int
	err = tx.QueryRow(ctx, `
SELECT 1
FROM agents
WHERE id = $1
FOR KEY SHARE;`, agentID).Scan(&agentCheck)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, 0, fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
		}
		return 0, 0, 0, fmt.Errorf("stats agent check: %w", err)
	}

	var (
		depth  int64
		leased int64
	)

	var oldestCreatedAt *time.Time

	err = tx.QueryRow(ctx, `
SELECT
    COUNT(*)::bigint AS queue_depth,
    COUNT(*) FILTER (
        WHERE lease_expires_at IS NOT NULL AND lease_expires_at > $2
    )::bigint AS leased_count,
    MIN(created_at) AS oldest_created_at
FROM inbox_entries
WHERE agent_id = $1
  AND acked = FALSE
  AND expires_at > $2;`, agentID, now).Scan(&depth, &leased, &oldestCreatedAt)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("stats aggregate: %w", err)
	}
	if depth > math.MaxInt {
		return 0, 0, 0, fmt.Errorf("stats: queue depth %d exceeds int range", depth)
	}
	if leased > math.MaxInt {
		return 0, 0, 0, fmt.Errorf("stats: leased count %d exceeds int range", leased)
	}

	var age time.Duration
	if oldestCreatedAt != nil {
		age = now.Sub(*oldestCreatedAt)
		if age < 0 {
			age = 0
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("stats commit: %w", err)
	}

	return int(depth), int(leased), age, nil
}

// PurgeExpired has moved to postgres_ownership.go (CR-FEAT-025): the sweep is
// the same one, but it now reports the rows it removed instead of only counting
// them, and it also applies the dead-letter retention window. PurgeExpired
// remains the count-only entry point.
