// store_sql.go — the SQL read-cursor store (spec §4).
//
// ONE implementation over any engine that speaks database/sql, parameterised by
// a Dialect that supplies only what genuinely differs: the placeholder syntax
// and the DDL's timestamp type. SQLite and PostgreSQL are therefore adapters
// over one store, not two copies — the same posture the session view takes
// (CR-CHAT-006).
//
// The table is the spec's, verbatim in names and columns:
//
//	chat_read_cursors (session_id, member_type, member_id,
//	                   read_through_seq, updated_at)
//	  PRIMARY KEY (session_id, member_type, member_id)
//	  INDEX chat_read_cursors_member (member_type, member_id)
//
// There is deliberately NO foreign key to chat_sessions: a cursor is a fact
// about a member's progress, and the JSONL backend has no session table at all.
// A deployment that moves between backends must not lose read state because one
// engine enforced a row it could not see.
//
// Monotonicity is the UPDATE half of §3.2's rule: the upsert writes only when
// the incoming seq is strictly greater than the stored one, so concurrent
// advances converge on the highest value and neither errors (§7).
package reads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	// Both drivers are registered by their database/sql adapter; the session
	// stores already link them, so this adds no dependency — it only lets the
	// cursor store open its own handle when the deployment has not handed it
	// one.
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Dialect is the engine-specific half of the shared SQL cursor store.
type Dialect string

const (
	// DialectSQLite is the pure-Go SQLite engine (TEXT timestamps).
	DialectSQLite Dialect = "sqlite"
	// DialectPostgres is PostgreSQL (TIMESTAMPTZ timestamps).
	DialectPostgres Dialect = "postgres"
)

// sqliteDriverName / postgresDriverName are the names the drivers register.
const (
	sqliteDriverName   = "sqlite"
	postgresDriverName = "pgx"
)

// SQLStore is the SQL-backed cursor store.
type SQLStore struct {
	db      *sql.DB
	dialect Dialect
	owned   bool
	rebind  func(string) string
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore wraps an ALREADY OPEN handle with the cursor store and applies
// the §4 DDL idempotently. The handle is borrowed: Close does not close it,
// because the deployment that opened it owns it (the session store usually
// hands over the handle it is already using, which keeps SQLite on one
// connection and one writer).
func NewSQLStore(ctx context.Context, db *sql.DB, dialect Dialect) (*SQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil database handle", ErrStoreError)
	}
	d, err := normalizeDialect(dialect)
	if err != nil {
		return nil, err
	}
	s := &SQLStore{db: db, dialect: d, owned: false, rebind: rebindFor(d)}
	if err := s.applySchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// NewSQLiteStore opens (creating when absent) a SQLite-backed cursor store. It
// is for a deployment (or a test) that keeps cursors in their own file; the
// server wiring prefers NewSQLStore over the session store's handle.
func NewSQLiteStore(ctx context.Context, path string) (*SQLStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: sqlite path is empty", ErrStoreError)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, fmt.Errorf("%w: open sqlite: %v", ErrStoreError, err)
	}
	// ONE connection: SQLite admits a single writer, and the per-connection
	// pragmas must stay in force for every statement.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: arm sqlite: %v", ErrStoreError, err)
	}
	s := &SQLStore{db: db, dialect: DialectSQLite, owned: true, rebind: rebindFor(DialectSQLite)}
	if err := s.applySchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewPostgresStore opens a PostgreSQL-backed cursor store over the pgx
// database/sql adapter. It is used when the deployment's session store is the
// pgxpool adapter and has no database/sql handle to share.
func NewPostgresStore(ctx context.Context, connString string) (*SQLStore, error) {
	if strings.TrimSpace(connString) == "" {
		return nil, fmt.Errorf("%w: postgres connection string is empty", ErrStoreError)
	}
	db, err := sql.Open(postgresDriverName, connString)
	if err != nil {
		return nil, fmt.Errorf("%w: open postgres: %v", ErrStoreError, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: ping postgres: %v", ErrStoreError, err)
	}
	s := &SQLStore{db: db, dialect: DialectPostgres, owned: true, rebind: rebindFor(DialectPostgres)}
	if err := s.applySchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Backend names the engine this store runs on.
func (s *SQLStore) Backend() string { return string(s.dialect) }

// Dialect returns the engine this store runs on.
func (s *SQLStore) Dialect() Dialect { return s.dialect }

// Close releases the handle when this store opened it. A borrowed handle is the
// caller's and is left open, exactly as the session stores treat one.
func (s *SQLStore) Close() error {
	if s.db != nil && s.owned {
		return s.db.Close()
	}
	return nil
}

// Get returns one member's stored cursor, or ErrCursorNotFound when the member
// has never advanced (§2.5: absent = 0).
func (s *SQLStore) Get(ctx context.Context, sessionID, memberType, memberID string) (*Cursor, error) {
	c, err := s.readOne(ctx, s.rebind(
		`SELECT session_id, member_type, member_id, read_through_seq, updated_at
		   FROM chat_read_cursors
		  WHERE session_id = ? AND member_type = ? AND member_id = ?`),
		sessionID, memberType, memberID)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// List returns every cursor row of a session, ordered by (member_type,
// member_id). Rows for removed members are included: the store keeps the
// historical fact, the read-state surface decides who is shown (§7).
func (s *SQLStore) List(ctx context.Context, sessionID string) ([]Cursor, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT session_id, member_type, member_id, read_through_seq, updated_at
		   FROM chat_read_cursors
		  WHERE session_id = ?`), sessionID)
	if err != nil {
		return nil, fmt.Errorf("%w: list cursors: %v", ErrStoreError, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Cursor
	for rows.Next() {
		c, err := s.scanCursor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list cursors: %v", ErrStoreError, err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MemberType != out[j].MemberType {
			return out[i].MemberType < out[j].MemberType
		}
		return out[i].MemberID < out[j].MemberID
	})
	return out, nil
}

// Advance is the monotonic writer (§3.2, §7). The upsert carries the
// `read_through_seq < excluded.read_through_seq` predicate, so a lower value
// leaves the row untouched instead of overwriting the higher one; the STORED
// row is read back and returned, so a caller always sees the value that won.
func (s *SQLStore) Advance(ctx context.Context, c Cursor) (Cursor, error) {
	if c.SessionID == "" || c.MemberType == "" || c.MemberID == "" {
		return Cursor{}, fmt.Errorf("%w: cursor is missing its session or member", ErrStoreError)
	}
	if c.ReadThroughSeq < 0 {
		return Cursor{}, fmt.Errorf("%w: negative seq %d", ErrStoreError, c.ReadThroughSeq)
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	c.UpdatedAt = c.UpdatedAt.UTC()

	var (
		stmt string
		args []any
	)
	switch s.dialect {
	case DialectPostgres:
		stmt = `INSERT INTO chat_read_cursors (session_id, member_type, member_id, read_through_seq, updated_at)
		        VALUES ($1, $2, $3, $4, $5)
		        ON CONFLICT (session_id, member_type, member_id) DO UPDATE
		          SET read_through_seq = excluded.read_through_seq,
		              updated_at      = excluded.updated_at
		        WHERE read_through_seq < excluded.read_through_seq`
		args = []any{c.SessionID, c.MemberType, c.MemberID, c.ReadThroughSeq, c.UpdatedAt}
	default:
		stmt = `INSERT INTO chat_read_cursors (session_id, member_type, member_id, read_through_seq, updated_at)
		        VALUES (?, ?, ?, ?, ?)
		        ON CONFLICT (session_id, member_type, member_id) DO UPDATE
		          SET read_through_seq = excluded.read_through_seq,
		              updated_at      = excluded.updated_at
		        WHERE read_through_seq < excluded.read_through_seq`
		args = []any{c.SessionID, c.MemberType, c.MemberID, c.ReadThroughSeq, encTime(c.UpdatedAt)}
	}

	if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
		return Cursor{}, fmt.Errorf("%w: advance cursor: %v", ErrStoreError, err)
	}
	stored, err := s.Get(ctx, c.SessionID, c.MemberType, c.MemberID)
	if err != nil {
		if errors.Is(err, ErrCursorNotFound) {
			return Cursor{}, fmt.Errorf("%w: cursor row vanished after advance", ErrStoreError)
		}
		return Cursor{}, err
	}
	return *stored, nil
}

// readOne reads one row; sql.ErrNoRows becomes ErrCursorNotFound.
func (s *SQLStore) readOne(ctx context.Context, query string, args ...any) (*Cursor, error) {
	row := s.db.QueryRowContext(ctx, query, args...)
	c, err := s.scanCursor(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %v", ErrCursorNotFound, args)
		}
		return nil, err
	}
	return c, nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanCursor reads one cursor row, decoding `updated_at` in the engine's own
// timestamp shape (TEXT on SQLite, TIMESTAMPTZ on PostgreSQL).
func (s *SQLStore) scanCursor(sc scanner) (*Cursor, error) {
	var (
		c  Cursor
		ts any
	)
	dest := []any{&c.SessionID, &c.MemberType, &c.MemberID, &c.ReadThroughSeq}
	if s.dialect == DialectPostgres {
		var t time.Time
		dest = append(dest, &t)
		if err := sc.Scan(dest...); err != nil {
			return nil, err
		}
		c.UpdatedAt = t.UTC()
		return &c, nil
	}
	dest = append(dest, &ts)
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	text, ok := ts.(string)
	if !ok {
		return nil, fmt.Errorf("%w: updated_at is %T, want TEXT", ErrStoreError, ts)
	}
	t, err := decTime(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStoreError, err)
	}
	c.UpdatedAt = t
	return &c, nil
}

// applySchema creates the §4 table and index. It is idempotent and safe at
// every boot, so an existing database reaches the table with no migration step.
func (s *SQLStore) applySchema(ctx context.Context) error {
	for _, stmt := range s.schema() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w: apply %s schema: %v", ErrStoreError, s.dialect, err)
		}
	}
	return nil
}

func (s *SQLStore) schema() []string {
	if s.dialect == DialectPostgres {
		return []string{
			`CREATE TABLE IF NOT EXISTS chat_read_cursors (
				session_id       TEXT        NOT NULL,
				member_type      TEXT        NOT NULL,
				member_id        TEXT        NOT NULL,
				read_through_seq BIGINT      NOT NULL DEFAULT 0,
				updated_at       TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (session_id, member_type, member_id)
			)`,
			`CREATE INDEX IF NOT EXISTS chat_read_cursors_member
				ON chat_read_cursors (member_type, member_id)`,
		}
	}
	// SQLite: TEXT timestamps, the same dialect-neutral codec choice the
	// session view makes on both engines.
	return []string{
		`CREATE TABLE IF NOT EXISTS chat_read_cursors (
			session_id       TEXT    NOT NULL,
			member_type      TEXT    NOT NULL,
			member_id        TEXT    NOT NULL,
			read_through_seq INTEGER NOT NULL DEFAULT 0,
			updated_at       TEXT    NOT NULL,
			PRIMARY KEY (session_id, member_type, member_id)
		)`,
		`CREATE INDEX IF NOT EXISTS chat_read_cursors_member
			ON chat_read_cursors (member_type, member_id)`,
	}
}

func normalizeDialect(d Dialect) (Dialect, error) {
	switch Dialect(strings.ToLower(strings.TrimSpace(string(d)))) {
	case DialectSQLite:
		return DialectSQLite, nil
	case DialectPostgres:
		return DialectPostgres, nil
	}
	return "", fmt.Errorf("%w: unknown dialect %q (want %q or %q)", ErrStoreError, d, DialectSQLite, DialectPostgres)
}

// rebindFor is the identity on SQLite and `$1..$n` on PostgreSQL, so ONE query
// text serves both engines.
func rebindFor(d Dialect) func(string) string {
	if d != DialectPostgres {
		return func(q string) string { return q }
	}
	return func(q string) string {
		out := make([]byte, 0, len(q)+8)
		n := 0
		for i := 0; i < len(q); i++ {
			if q[i] == '?' {
				n++
				out = append(out, '$')
				out = append(out, []byte(fmt.Sprintf("%d", n))...)
				continue
			}
			out = append(out, q[i])
		}
		return string(out)
	}
}

// encTime renders a timestamp as canonical RFC 3339 UTC TEXT.
func encTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// decTime parses the stored text back.
func decTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}
