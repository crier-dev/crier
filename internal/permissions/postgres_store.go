package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the PostgreSQL half of the dual store: the QUERY VIEW built
// from the ordered log. It is what a deployment serves the ACL from —
// indexed, joinable, countable.
//
// It is APPEND-ONLY like the JSONL log: every table is keyed (id, seq) with a
// generated seq, a new version is an INSERT (never an UPDATE), and the fold
// keeps the highest seq per id. That is what makes §6.6's tombstone real on
// this backend too: a revoked grant's original row is still there.
type PostgresStore struct {
	pool *pgxpool.Pool
	// owned records whether this store opened the pool it uses. A borrowed
	// pool (NewPostgresStoreWithPool — the shipped server already holds one for
	// the registry) is NOT closed by Close.
	owned bool
}

var _ Store = (*PostgresStore)(nil)

// SchemaStatements is the DDL, applied verbatim and idempotently. It is
// exported so a deployment can audit the exact SQL rather than infer it, and so
// a test can assert the shape exists.
//
// Every table is append-only: PRIMARY KEY (id, seq) with a GENERATED seq, so a
// record-version is an INSERT and the fold is DISTINCT ON (id) ... ORDER BY seq
// DESC (keep-LAST). The CHECK constraints fix the closed vocabularies so a
// hand-written row cannot introduce an action, role or subject the Go layer
// would refuse.
var SchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS permission_principals (
		id              TEXT NOT NULL,
		seq             BIGINT GENERATED ALWAYS AS IDENTITY,
		ts              TIMESTAMPTZ NOT NULL,
		kind            TEXT NOT NULL DEFAULT 'principal',
		display_name    TEXT NOT NULL DEFAULT '',
		namespace       TEXT NOT NULL DEFAULT '',
		role            TEXT NOT NULL DEFAULT '' CHECK (role IN ('','owner','admin','member','viewer')),
		status          TEXT NOT NULL CHECK (status IN ('invited','active','suspended','revoked')),
		remote          BOOLEAN NOT NULL DEFAULT FALSE,
		remote_instance TEXT NOT NULL DEFAULT '',
		remote_ref      TEXT NOT NULL DEFAULT '',
		created_at      TIMESTAMPTZ NULL,
		last_login_at   TIMESTAMPTZ NULL,
		PRIMARY KEY (id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_principals_latest_idx
		ON permission_principals (id, seq DESC)`,
	`CREATE TABLE IF NOT EXISTS permission_bindings (
		id         TEXT NOT NULL,
		seq        BIGINT GENERATED ALWAYS AS IDENTITY,
		ts         TIMESTAMPTZ NOT NULL,
		kind       TEXT NOT NULL DEFAULT 'binding',
		principal  TEXT NOT NULL,
		agent      TEXT NOT NULL,
		as_agent   BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NULL,
		created_by TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_bindings_pair_idx
		ON permission_bindings (principal, agent, seq DESC)`,
	`CREATE TABLE IF NOT EXISTS permission_grants (
		id           TEXT NOT NULL,
		seq          BIGINT GENERATED ALWAYS AS IDENTITY,
		ts           TIMESTAMPTZ NOT NULL,
		kind         TEXT NOT NULL DEFAULT 'grant',
		principal    TEXT NOT NULL,
		subject_type TEXT NOT NULL CHECK (subject_type IN ('agent','group','capability','session','namespace')),
		subject_ref  TEXT NOT NULL,
		actions      TEXT[] NOT NULL CHECK (array_length(actions, 1) >= 1),
		granted_by   TEXT NOT NULL DEFAULT '',
		granted_at   TIMESTAMPTZ NULL,
		expires_at   TIMESTAMPTZ NULL,
		revoked_at   TIMESTAMPTZ NULL,
		revoked_by   TEXT NOT NULL DEFAULT '',
		note         TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_grants_lookup_idx
		ON permission_grants (principal, subject_type, subject_ref, seq DESC)`,
	`CREATE TABLE IF NOT EXISTS permission_agents (
		id           TEXT NOT NULL,
		seq          BIGINT GENERATED ALWAYS AS IDENTITY,
		ts           TIMESTAMPTZ NOT NULL,
		class        TEXT NOT NULL DEFAULT '' CHECK (class IN ('','personal','service')),
		owner        TEXT NOT NULL DEFAULT '',
		namespace    TEXT NOT NULL DEFAULT '',
		capabilities TEXT[] NOT NULL DEFAULT '{}',
		scopes       JSONB NOT NULL DEFAULT '[]'::jsonb,
		PRIMARY KEY (id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_agents_latest_idx
		ON permission_agents (id, seq DESC)`,
}

// NewPostgresStore opens a pool against connString, applies the schema and
// returns a ready store. The caller owns the returned store and must Close it.
func NewPostgresStore(ctx context.Context, connString string) (*PostgresStore, error) {
	if connString == "" {
		return nil, fmt.Errorf("%w: connection string is empty", ErrInvalidRecord)
	}
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("permissions postgres store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("permissions postgres store: ping: %w", err)
	}
	s := &PostgresStore{pool: pool, owned: true}
	if err := s.ApplySchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// NewPostgresStoreWithPool wraps an existing pool and applies the schema.
// Closing the returned store does NOT close the borrowed pool.
func NewPostgresStoreWithPool(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidRecord)
	}
	s := &PostgresStore{pool: pool}
	if err := s.ApplySchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ApplySchema creates the tables when they are absent. Idempotent.
func (s *PostgresStore) ApplySchema(ctx context.Context) error {
	for _, stmt := range SchemaStatements {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("permissions postgres store: apply schema: %w", err)
		}
	}
	return nil
}

// Close releases the pool when this store opened it.
func (s *PostgresStore) Close() error {
	if s.pool != nil && s.owned {
		s.pool.Close()
	}
	return nil
}

// Append inserts one record-version. Insert-only, so a tombstone keeps the
// record it supersedes.
func (s *PostgresStore) Append(ctx context.Context, rec *Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	switch rec.Type {
	case RecordPrincipal:
		p := rec.Principal
		_, err := s.pool.Exec(ctx, `INSERT INTO permission_principals
			(id, ts, kind, display_name, namespace, role, status, remote, remote_instance, remote_ref, created_at, last_login_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			p.ID, rec.TS.UTC(), p.Kind, p.DisplayName, p.Namespace, string(p.Role), string(p.Status),
			p.Remote, p.RemoteInstance, p.RemoteRef, nullTime(p.CreatedAt), nullTimePtr(p.LastLoginAt))
		return wrapExec(err)
	case RecordBinding:
		b := rec.Binding
		_, err := s.pool.Exec(ctx, `INSERT INTO permission_bindings
			(id, ts, kind, principal, agent, as_agent, created_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			b.ID, rec.TS.UTC(), b.Kind, b.Principal, b.Agent, b.AsAgent, nullTime(b.CreatedAt), b.CreatedBy)
		return wrapExec(err)
	case RecordGrant:
		g := rec.Grant
		actions := make([]string, len(g.Actions))
		for i, a := range g.Actions {
			actions[i] = string(a)
		}
		_, err := s.pool.Exec(ctx, `INSERT INTO permission_grants
			(id, ts, kind, principal, subject_type, subject_ref, actions, granted_by, granted_at, expires_at, revoked_at, revoked_by, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			g.ID, rec.TS.UTC(), g.Kind, g.Principal, string(g.Subject.Type), g.Subject.Ref, actions,
			g.GrantedBy, nullTime(g.GrantedAt), nullTimePtr(g.ExpiresAt), nullTimePtr(g.RevokedAt), g.RevokedBy, g.Note)
		return wrapExec(err)
	case RecordAgent:
		a := rec.Agent
		scopes, err := json.Marshal(a.Scopes)
		if err != nil {
			return fmt.Errorf("permissions postgres store: marshal scopes: %w", err)
		}
		if a.Scopes == nil {
			scopes = []byte("[]")
		}
		caps := a.Capabilities
		if caps == nil {
			caps = []string{}
		}
		_, err = s.pool.Exec(ctx, `INSERT INTO permission_agents
			(id, ts, class, owner, namespace, capabilities, scopes)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)`,
			a.ID, rec.TS.UTC(), string(a.Class), a.Owner, a.Namespace, caps, scopes)
		return wrapExec(err)
	}
	return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, rec.Type)
}

// Snapshot reads the latest version of every record (keep-LAST per id) and
// folds it. DISTINCT ON (id) ... ORDER BY id, seq DESC is Postgres's
// keep-highest-seq-per-id.
func (s *PostgresStore) Snapshot(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{
		principals: map[string]*Principal{},
		bindings:   map[string]*Binding{},
		grants:     map[string]*Grant{},
		agents:     map[string]*AgentInfo{},
	}

	if err := s.loadPrincipals(ctx, snap); err != nil {
		return nil, err
	}
	if err := s.loadBindings(ctx, snap); err != nil {
		return nil, err
	}
	if err := s.loadGrants(ctx, snap); err != nil {
		return nil, err
	}
	if err := s.loadAgents(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *PostgresStore) loadPrincipals(ctx context.Context, snap *Snapshot) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (id)
		id, kind, display_name, namespace, role, status, remote, remote_instance, remote_ref, created_at, last_login_at
		FROM permission_principals ORDER BY id, seq DESC`)
	if err != nil {
		return fmt.Errorf("permissions postgres store: query principals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p := &Principal{}
		var role, status string
		var createdAt *time.Time
		if err := rows.Scan(&p.ID, &p.Kind, &p.DisplayName, &p.Namespace, &role, &status,
			&p.Remote, &p.RemoteInstance, &p.RemoteRef, &createdAt, &p.LastLoginAt); err != nil {
			return fmt.Errorf("permissions postgres store: scan principal: %w", err)
		}
		p.Role = Role(role)
		p.Status = PrincipalStatus(status)
		if createdAt != nil {
			p.CreatedAt = createdAt.UTC()
		}
		// The JSONL log stores UTC instants (Record builders call .UTC()).
		// pgx scans TIMESTAMPTZ in the CONNECTION's zone, so without this the
		// two backends would return the same instant in different Locations —
		// and a struct comparison (or an operator reading a timestamp) would
		// see them as different. Normalize at the boundary.
		p.LastLoginAt = utcPtr(p.LastLoginAt)
		snap.principals[p.ID] = p
	}
	return rows.Err()
}

func (s *PostgresStore) loadBindings(ctx context.Context, snap *Snapshot) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (id)
		id, kind, principal, agent, as_agent, created_at, created_by
		FROM permission_bindings ORDER BY id, seq DESC`)
	if err != nil {
		return fmt.Errorf("permissions postgres store: query bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		b := &Binding{}
		var createdAt *time.Time
		if err := rows.Scan(&b.ID, &b.Kind, &b.Principal, &b.Agent, &b.AsAgent, &createdAt, &b.CreatedBy); err != nil {
			return fmt.Errorf("permissions postgres store: scan binding: %w", err)
		}
		if createdAt != nil {
			b.CreatedAt = createdAt.UTC()
		}
		snap.bindings[bindingKey(b.Principal, b.Agent)] = b
	}
	return rows.Err()
}

func (s *PostgresStore) loadGrants(ctx context.Context, snap *Snapshot) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (id)
		id, kind, principal, subject_type, subject_ref, actions, granted_by, granted_at, expires_at, revoked_at, revoked_by, note
		FROM permission_grants ORDER BY id, seq DESC`)
	if err != nil {
		return fmt.Errorf("permissions postgres store: query grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		g := &Grant{}
		var subjectType string
		var actions []string
		var grantedAt *time.Time
		if err := rows.Scan(&g.ID, &g.Kind, &g.Principal, &subjectType, &g.Subject.Ref, &actions,
			&g.GrantedBy, &grantedAt, &g.ExpiresAt, &g.RevokedAt, &g.RevokedBy, &g.Note); err != nil {
			return fmt.Errorf("permissions postgres store: scan grant: %w", err)
		}
		g.Subject.Type = SubjectType(subjectType)
		for _, a := range actions {
			g.Actions = append(g.Actions, Action(a))
		}
		if grantedAt != nil {
			g.GrantedAt = grantedAt.UTC()
		}
		g.ExpiresAt = utcPtr(g.ExpiresAt)
		g.RevokedAt = utcPtr(g.RevokedAt)
		snap.grants[g.ID] = g
	}
	return rows.Err()
}

func (s *PostgresStore) loadAgents(ctx context.Context, snap *Snapshot) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (id)
		id, class, owner, namespace, capabilities, scopes
		FROM permission_agents ORDER BY id, seq DESC`)
	if err != nil {
		return fmt.Errorf("permissions postgres store: query agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a := &AgentInfo{}
		var class string
		var caps []string
		var scopes []byte
		if err := rows.Scan(&a.ID, &class, &a.Owner, &a.Namespace, &caps, &scopes); err != nil {
			return fmt.Errorf("permissions postgres store: scan agent: %w", err)
		}
		a.Class = AgentClass(class)
		// Normalize empty to nil: PostgreSQL returns an empty array (or an
		// empty JSON document) for a row written with no members, while the
		// JSONL log stores the nil it was given. Without this the two
		// backends would disagree on a field that carries no information.
		a.Capabilities = nilIfEmptyStrings(caps)
		if len(scopes) > 0 {
			if err := json.Unmarshal(scopes, &a.Scopes); err != nil {
				return fmt.Errorf("permissions postgres store: unmarshal scopes for %q: %w", a.ID, err)
			}
		}
		if len(a.Scopes) == 0 {
			a.Scopes = nil
		}
		snap.agents[a.ID] = a
	}
	return rows.Err()
}

func wrapExec(err error) error {
	if err != nil {
		return fmt.Errorf("permissions postgres store: append: %w", err)
	}
	return nil
}

// nullTime converts a zero time.Time to a SQL NULL, so an unset timestamp is
// absent rather than the year 1.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// nullTimePtr passes a pointer through (nil stays NULL, zero becomes NULL).
func nullTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return nullTime(*t)
}

// utcPtr normalizes a scanned timestamp's Location to UTC, so a value read
// back from PostgreSQL compares equal to the same instants the JSONL log
// stores. A nil stays nil.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// nilIfEmptyStrings collapses PostgreSQL's non-nil empty array to nil, so a row
// written with no members folds to the same value the JSONL log stored.
func nilIfEmptyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
