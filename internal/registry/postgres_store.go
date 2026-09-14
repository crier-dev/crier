package registry

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
}

var _ Store = (*PostgresStore)(nil)

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

// Register adds an agent. Maps duplicate PK to ErrAgentExists.
func (s *PostgresStore) Register(agent *Agent) error {
	if agent == nil || agent.ID == "" {
		return fmt.Errorf("%w: nil agent or blank ID", ErrInvalidStoreInput)
	}
	if len(agent.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key must be %d bytes", ErrInvalidStoreInput, ed25519.PublicKeySize)
	}
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

	now := time.Now().UTC()
	status := agent.Status
	if status == "" {
		status = StatusOnline
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	_, err = s.pool.Exec(ctx, `
INSERT INTO agents (
    id, public_key, capabilities, status, registered_at, last_seen
) VALUES ($1, $2, $3::jsonb, $4, $5, $6);`,
		agent.ID, []byte(agent.PublicKey), capsJSON, string(status), now, now,
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
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, public_key, capabilities, status, registered_at, last_seen
FROM agents
WHERE id = $1;`, id).Scan(
		&agent.ID, &publicKey, &capabilitiesJSON, &agent.Status, &agent.RegisteredAt, &agent.LastSeen,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
		}
		return nil, fmt.Errorf("get agent: %w", err)
	}

	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("get agent: public key length %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	key := make(HexKey, ed25519.PublicKeySize)
	copy(key, publicKey)
	agent.PublicKey = key

	if err := json.Unmarshal(capabilitiesJSON, &agent.Capabilities); err != nil {
		return nil, fmt.Errorf("get agent: unmarshal capabilities: %w", err)
	}
	return &agent, nil
}

// List returns all agents ordered by registration time then ID.
// Per the Store contract, List cannot return an error; on failure it logs and
// returns a non-nil empty slice.
func (s *PostgresStore) List() []*Agent {
	ctx, cancel := s.operationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
SELECT id, public_key, capabilities, status, registered_at, last_seen
FROM agents
ORDER BY registered_at ASC, id ASC;`)
	if err != nil {
		slog.Error("postgres list", "error", err)
		return []*Agent{}
	}
	defer rows.Close()

	out := make([]*Agent, 0)
	for rows.Next() {
		var (
			agent            Agent
			publicKey        []byte
			capabilitiesJSON []byte
		)
		if err := rows.Scan(&agent.ID, &publicKey, &capabilitiesJSON, &agent.Status, &agent.RegisteredAt, &agent.LastSeen); err != nil {
			slog.Error("postgres list scan", "error", err)
			return []*Agent{}
		}
		if len(publicKey) != ed25519.PublicKeySize {
			slog.Warn("postgres list: invalid public key", "len", len(publicKey), "agent_id", agent.ID)
			return []*Agent{}
		}
		key := make(HexKey, ed25519.PublicKeySize)
		copy(key, publicKey)
		agent.PublicKey = key
		if err := json.Unmarshal(capabilitiesJSON, &agent.Capabilities); err != nil {
			slog.Error("postgres list: unmarshal capabilities", "error", err)
			return []*Agent{}
		}
		out = append(out, &agent)
	}
	if err := rows.Err(); err != nil {
		slog.Error("postgres list rows", "error", err)
		return []*Agent{}
	}
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
// PATCH /agents/{id} path (CR-FEAT-007). The Postgres backend persists
// capabilities; webhook config is not persisted here (mirrors Register, whose
// INSERT also omits it — webhook remains an in-memory registration field).
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

	ctx, cancel := s.operationContext()
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
UPDATE agents
SET capabilities = $2::jsonb, last_seen = $3
WHERE id = $1;`, agent.ID, capsJSON, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agent.ID)
	}
	return nil
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
// the same never-expires representation as the in-memory backend and the
// documented wire contract (expires_at 0001-01-01T00:00:00Z when
// ttl_seconds was 0). Reading into a plain time.Time would fail outright:
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

	_, err := s.pool.Exec(ctx, `
INSERT INTO inbox_entries (
    id, agent_id, payload, created_at, expires_at,
    leased_at, lease_id, lease_expires_at, acked
) VALUES ($1, $2, $3::jsonb, $4, $5, NULL, NULL, NULL, FALSE);`,
		entry.ID, agentID, entry.Payload, entry.CreatedAt, pgTimestamptz(entry.ExpiresAt),
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
SELECT id, agent_id, payload, created_at, expires_at
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
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.CreatedAt, &expiresAt); err != nil {
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

// PurgeExpired deletes TTL-expired messages and releases elapsed leases.
// Returns the number of deleted TTL-expired messages.
func (s *PostgresStore) PurgeExpired() int {
	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()

	tag, err := tx.Exec(ctx, `
DELETE FROM inbox_entries
WHERE expires_at <= $1;`, now)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}
	removed := int(tag.RowsAffected())

	_, err = tx.Exec(ctx, `
UPDATE inbox_entries
SET leased_at = NULL,
    lease_id = NULL,
    lease_expires_at = NULL
WHERE expires_at > $1
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= $1;`, now)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}

	return removed
}
