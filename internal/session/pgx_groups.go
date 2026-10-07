// pgx_groups.go — the pgxpool-backed GroupStore (CR-CHAT-022): the same
// named-group contract the SQLGroupStore serves, over the pgxpool the shipped
// registry already holds. The tables are the same `chat_groups` /
// `chat_group_members` pair (their DDL is part of PostgresStore's
// SchemaStatements), so every PostgreSQL path sees one roster and an existing
// database reaches the tables with no migration step.
package session

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLGroupSchemaStatements is the group DDL in PostgreSQL's own spelling,
// exported from the pgxpool store exactly as the session view's DDL is
// (PostgresStore.SchemaStatements) so a deployment can audit it.
var SQLGroupSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS chat_groups (
		name       TEXT PRIMARY KEY,
		namespace  TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_group_members (
		group_name TEXT NOT NULL REFERENCES chat_groups(name) ON DELETE CASCADE,
		member_id  TEXT NOT NULL,
		position   INTEGER NOT NULL,
		PRIMARY KEY (group_name, member_id)
	)`,
	`CREATE INDEX IF NOT EXISTS chat_group_members_name_idx
		ON chat_group_members (group_name, position)`,
}

// pgxGroupStore is the GroupStore over a pgxpool.
type pgxGroupStore struct {
	pool *pgxpool.Pool
}

var _ GroupStore = (*pgxGroupStore)(nil)

// NewPgxGroupStore wraps an existing pool with the named-group store.
func NewPgxGroupStore(pool *pgxpool.Pool) GroupStore {
	return &pgxGroupStore{pool: pool}
}

// Groups exposes the named-group roster store (CR-CHAT-022) over the SAME
// pool the session view runs on. The group DDL is part of
// PostgresStore.SchemaStatements, so the tables exist once ApplySchema ran.
func (s *PostgresStore) Groups() GroupStore { return NewPgxGroupStore(s.pool) }

// Create appends the group's first version. An existing name is ErrGroupExists.
func (s *pgxGroupStore) Create(ctx context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	g.CreatedAt = g.CreatedAt.UTC()
	g.UpdatedAt = g.CreatedAt
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("group store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exists := ""
	if err := tx.QueryRow(ctx, `SELECT name FROM chat_groups WHERE name = $1`, g.Name).Scan(&exists); err == nil {
		return fmt.Errorf("%w: %q", ErrGroupExists, g.Name)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("group store: read: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO chat_groups (name, namespace, created_by, created_at, updated_by, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		g.Name, g.Namespace, g.CreatedBy, g.CreatedAt, g.UpdatedBy, g.UpdatedAt); err != nil {
		return fmt.Errorf("group store: insert: %w", err)
	}
	if err := insertGroupMembersPgx(ctx, tx, g.Name, g.Members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update writes the new version (keep-LAST is the row). The creation facts are
// preserved from the stored row — an edit is never a re-create.
func (s *pgxGroupStore) Update(ctx context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	g.UpdatedAt = g.UpdatedAt.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("group store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `SELECT created_at, created_by FROM chat_groups WHERE name = $1`, g.Name).Scan(&g.CreatedAt, &g.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrGroupNotFound, g.Name)
	} else if err != nil {
		return fmt.Errorf("group store: read: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE chat_groups SET namespace = $2, updated_by = $3, updated_at = $4 WHERE name = $1`,
		g.Name, g.Namespace, g.UpdatedBy, g.UpdatedAt); err != nil {
		return fmt.Errorf("group store: update: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chat_group_members WHERE group_name = $1`, g.Name); err != nil {
		return fmt.Errorf("group store: replace roster: %w", err)
	}
	if err := insertGroupMembersPgx(ctx, tx, g.Name, g.Members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Get returns the CURRENT roster of one group.
func (s *pgxGroupStore) Get(ctx context.Context, name string) (*Group, error) {
	g, err := s.readOne(ctx, s.pool, name)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// List returns every group's current version, sorted by name.
func (s *pgxGroupStore) List(ctx context.Context) ([]*Group, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, namespace, created_by, created_at, updated_by, updated_at FROM chat_groups`)
	if err != nil {
		return nil, fmt.Errorf("group store: list: %w", err)
	}
	defer rows.Close()
	var out []*Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.Name, &g.Namespace, &g.CreatedBy, &g.CreatedAt, &g.UpdatedBy, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("group store: list: %w", err)
		}
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("group store: list: %w", err)
	}
	for _, g := range out {
		if err := s.loadMembers(ctx, s.pool, g); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readOne reads one group row (q is a *pgxpool.Pool or a pgx.Tx — both
// implement both Query and QueryRow, so one anonymous interface covers them).
func (s *pgxGroupStore) readOne(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, name string) (*Group, error) {
	var g Group
	err := q.QueryRow(ctx,
		`SELECT name, namespace, created_by, created_at, updated_by, updated_at FROM chat_groups WHERE name = $1`,
		name).Scan(&g.Name, &g.Namespace, &g.CreatedBy, &g.CreatedAt, &g.UpdatedBy, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, name)
	} else if err != nil {
		return nil, fmt.Errorf("group store: read: %w", err)
	}
	if err := s.loadMembers(ctx, q, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// loadMembers reads the roster of one group, ordered by the insert position
// (q is a *pgxpool.Pool or a pgx.Tx — both implement Query).
func (s *pgxGroupStore) loadMembers(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, g *Group) error {
	rows, err := q.Query(ctx,
		`SELECT member_id FROM chat_group_members WHERE group_name = $1 ORDER BY position`, g.Name)
	if err != nil {
		return fmt.Errorf("group store: read roster: %w", err)
	}
	defer rows.Close()
	g.Members = []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("group store: read roster: %w", err)
		}
		g.Members = append(g.Members, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("group store: read roster: %w", err)
	}
	return nil
}

// insertGroupMembersPgx writes one roster row per member, keeping order.
func insertGroupMembersPgx(ctx context.Context, tx pgx.Tx, name string, members []string) error {
	for i, m := range members {
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_group_members (group_name, member_id, position) VALUES ($1, $2, $3)`,
			name, m, i); err != nil {
			return fmt.Errorf("group store: insert member: %w", err)
		}
	}
	return nil
}
