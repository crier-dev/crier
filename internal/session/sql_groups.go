// sql_groups.go — the SQL-backed GroupStore (CR-CHAT-022): ONE implementation
// of the group roster contract running on any engine that speaks database/sql,
// parameterised by the same Dialect the session view uses (CR-CHAT-006). The
// SQLite and PostgreSQL adapters are therefore two dialects over this one
// store, exactly as the session view is.
//
// The tables are `chat_groups` (one row per group NAME) plus
// `chat_group_members` (one row per (name, member_id)); their DDL is part of
// every dialect's SchemaStatements, so an existing deployment reaches them
// with no migration step. The roster is a PROJECTION, not an append log here:
// the JSONL log (JSONLGroupStore, groups.go) remains the append-log form, and
// this view is written by Create/Update directly — the same write-direction
// split CHAT-STORAGE.md §2.2 fixes for sessions (record → view; never the
// reverse). Update preserves the creation facts the same way the JSONL store
// does: an edit is never a re-create.
//
// Timestamps are canonical RFC 3339 UTC TEXT on both engines — the same
// dialect-neutral codec choice sql_store.go makes, for the same reason.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// sqlGroupSchemaStatements is the named-group DDL (SQLite spelling: TEXT
// timestamps). It is part of the SQLite dialect's SchemaStatements.
var sqlGroupSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS chat_groups (
		name       TEXT PRIMARY KEY,
		namespace  TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
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

// SQLGroupStore is the SQL-backed GroupStore over the shared dialect pair.
type SQLGroupStore struct {
	db *sql.DB
	// rebind converts the canonical `?` placeholders to the engine's own.
	rebind func(string) string
}

var _ GroupStore = (*SQLGroupStore)(nil)

// NewSQLGroupStore wraps an open handle with the SQL group store. rebind is
// the dialect's placeholder conversion (the identity on SQLite, `$N` on
// PostgreSQL) — the same function SQLStore applies.
func NewSQLGroupStore(db *sql.DB, rebind func(string) string) *SQLGroupStore {
	return &SQLGroupStore{db: db, rebind: rebind}
}

// Create appends the group's first version. An existing name is ErrGroupExists.
func (s *SQLGroupStore) Create(ctx context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	g.CreatedAt = g.CreatedAt.UTC()
	g.UpdatedAt = g.CreatedAt
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("group store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	exists := ""
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT name FROM chat_groups WHERE name = ?`), g.Name).Scan(&exists); err == nil {
		return fmt.Errorf("%w: %q", ErrGroupExists, g.Name)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("group store: read: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO chat_groups (name, namespace, created_by, created_at, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
		g.Name, g.Namespace, g.CreatedBy, encTime(g.CreatedAt), g.UpdatedBy, encTime(g.UpdatedAt)); err != nil {
		return fmt.Errorf("group store: insert: %w", err)
	}
	if err := insertGroupMembers(ctx, tx, s.rebind, g.Name, g.Members); err != nil {
		return err
	}
	return tx.Commit()
}

// Update writes the new version (the view's keep-LAST per name is the row
// itself). The new version carries its own UpdatedAt/UpdatedBy; the creation
// facts are preserved from the stored row — an edit is never a re-create
// (the same rule the JSONL store applies).
func (s *SQLGroupStore) Update(ctx context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	g.UpdatedAt = g.UpdatedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("group store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var createdAt, createdBy string
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT created_at, created_by FROM chat_groups WHERE name = ?`), g.Name).Scan(&createdAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrGroupNotFound, g.Name)
	} else if err != nil {
		return fmt.Errorf("group store: read: %w", err)
	}
	if err := decTimeString(createdAt, &g.CreatedAt); err != nil {
		return fmt.Errorf("group store: read: %w", err)
	}
	g.CreatedBy = createdBy
	if _, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE chat_groups SET namespace = ?, updated_by = ?, updated_at = ? WHERE name = ?`),
		g.Namespace, g.UpdatedBy, encTime(g.UpdatedAt), g.Name); err != nil {
		return fmt.Errorf("group store: update: %w", err)
	}
	// The roster is replaced in full: the edit event is the new version.
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM chat_group_members WHERE group_name = ?`), g.Name); err != nil {
		return fmt.Errorf("group store: replace roster: %w", err)
	}
	if err := insertGroupMembers(ctx, tx, s.rebind, g.Name, g.Members); err != nil {
		return err
	}
	return tx.Commit()
}

// Get returns the CURRENT roster of one group.
func (s *SQLGroupStore) Get(ctx context.Context, name string) (*Group, error) {
	g, err := s.readOne(ctx, name)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// List returns every group's current version, sorted by name.
func (s *SQLGroupStore) List(ctx context.Context) ([]*Group, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT name, namespace, created_by, created_at, updated_by, updated_at FROM chat_groups`))
	if err != nil {
		return nil, fmt.Errorf("group store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Group
	for rows.Next() {
		var g Group
		var createdAt, updatedAt string
		if err := rows.Scan(&g.Name, &g.Namespace, &g.CreatedBy, &createdAt, &g.UpdatedBy, &updatedAt); err != nil {
			return nil, fmt.Errorf("group store: list: %w", err)
		}
		if err := decTimeString(createdAt, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("group store: list: %w", err)
		}
		if err := decTimeString(updatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("group store: list: %w", err)
		}
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("group store: list: %w", err)
	}
	for _, g := range out {
		if err := s.loadMembers(ctx, g); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readOne reads one group row plus its roster.
func (s *SQLGroupStore) readOne(ctx context.Context, name string) (*Group, error) {
	var g Group
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT name, namespace, created_by, created_at, updated_by, updated_at FROM chat_groups WHERE name = ?`),
		name).Scan(&g.Name, &g.Namespace, &g.CreatedBy, &createdAt, &g.UpdatedBy, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, name)
	} else if err != nil {
		return nil, fmt.Errorf("group store: read: %w", err)
	}
	if err := decTimeString(createdAt, &g.CreatedAt); err != nil {
		return nil, fmt.Errorf("group store: read: %w", err)
	}
	if err := decTimeString(updatedAt, &g.UpdatedAt); err != nil {
		return nil, fmt.Errorf("group store: read: %w", err)
	}
	if err := s.loadMembers(ctx, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// loadMembers reads the roster of one group, ordered by the insert position.
func (s *SQLGroupStore) loadMembers(ctx context.Context, g *Group) error {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT member_id FROM chat_group_members WHERE group_name = ? ORDER BY position`), g.Name)
	if err != nil {
		return fmt.Errorf("group store: read roster: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

// insertGroupMembers writes one roster row per member, keeping the caller's
// order (the members are already de-duplicated and sorted by validateGroup).
func insertGroupMembers(ctx context.Context, tx *sql.Tx, rebind func(string) string, name string, members []string) error {
	for i, m := range members {
		if _, err := tx.ExecContext(ctx, rebind(
			`INSERT INTO chat_group_members (group_name, member_id, position) VALUES (?, ?, ?)`),
			name, m, i); err != nil {
			return fmt.Errorf("group store: insert member: %w", err)
		}
	}
	return nil
}

// decTimeString parses one canonical RFC 3339 TEXT timestamp into t.
func decTimeString(s string, t *time.Time) error {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("timestamp %q: %w", s, err)
	}
	*t = v
	return nil
}
