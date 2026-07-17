package registry

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is backend configuration after config.Load has parsed environment values.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxConns:        4,
		MinConns:        0,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// PostgresStore is a pgxpool-backed implementation of Store.
type PostgresStore struct {
	pool *pgxpool.Pool
}

var _ Store = (*PostgresStore)(nil)

func NewPostgresStore(ctx context.Context, connString string) (*PostgresStore, error) {
	return NewPostgresStoreWithPoolConfig(ctx, connString, DefaultPoolConfig())
}

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
		agent           Agent
		publicKey       []byte
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
		log.Printf("postgres list: %v", err)
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
			log.Printf("postgres list scan: %v", err)
			return []*Agent{}
		}
		if len(publicKey) != ed25519.PublicKeySize {
			log.Printf("postgres list: public key length %d for %q", len(publicKey), agent.ID)
			return []*Agent{}
		}
		key := make(HexKey, ed25519.PublicKeySize)
		copy(key, publicKey)
		agent.PublicKey = key
		if err := json.Unmarshal(capabilitiesJSON, &agent.Capabilities); err != nil {
			log.Printf("postgres list: unmarshal capabilities: %v", err)
			return []*Agent{}
		}
		out = append(out, &agent)
	}
	if err := rows.Err(); err != nil {
		log.Printf("postgres list rows: %v", err)
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
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = entry.CreatedAt.Add(24 * time.Hour)
	} else {
		entry.ExpiresAt = entry.ExpiresAt.UTC()
	}
	if !entry.ExpiresAt.After(entry.CreatedAt) {
		return fmt.Errorf("%w: expiry at or before creation", ErrInvalidStoreInput)
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
		entry.ID, agentID, entry.Payload, entry.CreatedAt, entry.ExpiresAt,
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

	leaseID, err := newLeaseID()
	if err != nil {
		return nil, "", fmt.Errorf("generate lease id: %w", err)
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

	// 2. Select, lock, update, and return a disjoint FIFO batch.
	rows, err := tx.Query(ctx, `
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
ORDER BY delivery_sequence ASC;`,
		agentID, now, maxMessages, leaseID, leaseExpiresAt,
	)
	if err != nil {
		return nil, "", fmt.Errorf("retrieve select: %w", err)
	}

	result := make([]*InboxEntry, 0)
	for rows.Next() {
		var (
			entry    InboxEntry
			leasedAt *time.Time
		)
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.CreatedAt, &entry.ExpiresAt, &leasedAt, &entry.LeaseID, &entry.ACKed); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("retrieve scan: %w", err)
		}
		entry.LeasedAt = leasedAt
		entry.LeaseDuration = leaseDuration
		result = append(result, &entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("retrieve rows: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("retrieve commit: %w", err)
	}

	return result, leaseID, nil
}

// Ack validates then deletes messages atomically inside a transaction.
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

	// Empty messageIDs is a no-op after the agent existence check.
	if len(messageIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("ack commit (noop): %w", err)
		}
		return nil
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
		log.Printf("postgres purge expired: %v", err)
		return 0
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()

	tag, err := tx.Exec(ctx, `
DELETE FROM inbox_entries
WHERE expires_at <= $1;`, now)
	if err != nil {
		log.Printf("postgres purge expired: %v", err)
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
		log.Printf("postgres purge expired: %v", err)
		return 0
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("postgres purge expired: %v", err)
		return 0
	}

	return removed
}
