package session

import "strconv"

// The two engine dialects of the shared SQL session store (CR-CHAT-006). Each
// supplies ONLY what genuinely differs — placeholder syntax, DDL, connection
// setup — so the repository itself (sql_store.go) is engine-agnostic.

// sqliteDialect is the SQLite adapter: `?` placeholders (the store's canonical
// form, so no rebinding is needed) and a schema whose types are SQLite's own.
type sqliteDialect struct{}

func (sqliteDialect) Name() string { return BackendSQLite }

// Rebind is the identity: the store's canonical placeholder IS SQLite's.
func (sqliteDialect) Rebind(query string) string { return query }

// InitStatements arms the single-connection pragmas. A SQLite deployment runs
// one writer (NewSQLiteStore pools a single connection), so the per-connection
// settings hold for every statement. `journal_mode` is deliberately NOT set
// here: it returns a row, and the default rollback journal is the most
// portable choice.
func (sqliteDialect) InitStatements() []string {
	return []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA synchronous = NORMAL`,
	}
}

func (sqliteDialect) SchemaStatements() []string { return sqliteSchemaStatements }

// sqliteSchemaStatements is the §5.2 view in SQLite's spelling. Table and
// column NAMES are the spec's, unchanged — only the types differ
// (TEXT for the JSON-ish and timestamp columns, INTEGER for the counters),
// because the dialect-neutral codec in sql_store.go stores them as canonical
// text on both engines.
var sqliteSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS chat_sessions (
		id                TEXT PRIMARY KEY,
		namespace         TEXT NOT NULL DEFAULT '',
		kind              TEXT NOT NULL DEFAULT 'channel' CHECK (kind IN ('channel','direct')),
		title             TEXT NOT NULL DEFAULT '',
		created_by        TEXT NOT NULL,
		created_at        TEXT NOT NULL,
		state             TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
		closed_at         TEXT NULL,
		retention_seconds INTEGER NULL,
		group_id          TEXT NOT NULL DEFAULT '',
		visibility        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS chat_session_members (
		session_id  TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type TEXT NOT NULL CHECK (member_type IN ('agent','principal')),
		member_id   TEXT NOT NULL,
		role        TEXT NOT NULL,
		added_at    TEXT NOT NULL,
		removed_at  TEXT NULL,
		added_by    TEXT NOT NULL DEFAULT '',
		removed_by  TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (session_id, member_type, member_id)
	)`,
	`CREATE INDEX IF NOT EXISTS chat_session_members_active_idx
		ON chat_session_members (session_id, member_type, member_id)
		WHERE removed_at IS NULL`,
	`CREATE TABLE IF NOT EXISTS chat_transcript (
		session_id   TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		seq          INTEGER NOT NULL,
		message_id   TEXT NOT NULL,
		thread_id    TEXT NOT NULL,
		parent_id    TEXT NOT NULL DEFAULT '',
		message_kind TEXT NOT NULL CHECK (message_kind IN ('plain','addressed','task')),
		author_type  TEXT NOT NULL DEFAULT '',
		author_id    TEXT NOT NULL DEFAULT '',
		principal_id TEXT NOT NULL DEFAULT '',
		payload      TEXT NULL,
		guard        TEXT NULL,
		audience     TEXT NULL,
		created_at   TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS chat_transcript_message_id_idx
		ON chat_transcript (session_id, message_id)`,
	`CREATE TABLE IF NOT EXISTS chat_deliveries (
		session_id      TEXT NOT NULL,
		message_id      TEXT NOT NULL,
		target_agent_id TEXT NOT NULL,
		inbox_entry_id  TEXT NOT NULL DEFAULT '',
		outcome         TEXT NOT NULL,
		updated_at      TEXT NOT NULL,
		PRIMARY KEY (session_id, message_id, target_agent_id)
	)`,
	`CREATE TABLE IF NOT EXISTS chat_threads (
		thread_id         TEXT PRIMARY KEY,
		session_id        TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		parent_thread_id  TEXT NULL,
		root_message_id   TEXT NOT NULL DEFAULT '',
		anchor_message_id TEXT NULL,
		created_by        TEXT NULL,
		created_at        TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_context_shares (
		session_id          TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type         TEXT NOT NULL,
		member_id           TEXT NOT NULL,
		mode                TEXT NOT NULL,
		boundary_message_id TEXT NOT NULL DEFAULT '',
		set_by              TEXT NOT NULL DEFAULT '',
		set_at              TEXT NOT NULL,
		PRIMARY KEY (session_id, member_type, member_id)
	)`,
}

// postgresDialect is the PostgreSQL adapter over the SHARED repository: the
// same query text, with the store's `?` placeholders rebound to PostgreSQL's
// `$n`. It is the second engine proving the requirement — "supporting a second
// engine does not require rewriting or copying the session store" — because it
// adds nothing but this rebinding and the DDL below.
//
// It lives beside the pre-existing pgxpool-backed PostgresStore, which is
// untouched and remains the projection the shipped registry pool path uses
// (NewPostgresStore / NewPostgresStoreWithPool). A deployment selects ONE
// engine, and these adapters name the same §5.2 view, so do not point both at
// one database.
type postgresDialect struct{}

func (postgresDialect) Name() string { return BackendPostgres }

// Rebind rewrites every `?` to `$1`, `$2`, … in order. The store's queries
// carry no `?` inside a string literal, so a positional scan is exact.
func (postgresDialect) Rebind(query string) string {
	out := make([]byte, 0, len(query)+8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			out = append(out, '$')
			out = strconv.AppendInt(out, int64(n), 10)
			continue
		}
		out = append(out, query[i])
	}
	return string(out)
}

// InitStatements: nothing to arm — a pooled server connection needs no pragmas,
// and any session setting belongs to the DSN.
func (postgresDialect) InitStatements() []string { return nil }

func (postgresDialect) SchemaStatements() []string { return postgresSchemaStatements }

// postgresSchemaStatements is the same §5.2 view declared with PostgreSQL's
// own types (BIGINT for the sequence, INTEGER for the retention counter). The
// JSON-ish and timestamp columns stay TEXT so the ONE scan path in
// sql_store.go reads both engines identically.
var postgresSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS chat_sessions (
		id                TEXT PRIMARY KEY,
		namespace         TEXT NOT NULL DEFAULT '',
		kind              TEXT NOT NULL DEFAULT 'channel' CHECK (kind IN ('channel','direct')),
		title             TEXT NOT NULL DEFAULT '',
		created_by        TEXT NOT NULL,
		created_at        TEXT NOT NULL,
		state             TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
		closed_at         TEXT NULL,
		retention_seconds INTEGER NULL,
		group_id          TEXT NOT NULL DEFAULT '',
		visibility        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS chat_session_members (
		session_id  TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type TEXT NOT NULL CHECK (member_type IN ('agent','principal')),
		member_id   TEXT NOT NULL,
		role        TEXT NOT NULL,
		added_at    TEXT NOT NULL,
		removed_at  TEXT NULL,
		added_by    TEXT NOT NULL DEFAULT '',
		removed_by  TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (session_id, member_type, member_id)
	)`,
	`CREATE INDEX IF NOT EXISTS chat_session_members_active_idx
		ON chat_session_members (session_id, member_type, member_id)
		WHERE removed_at IS NULL`,
	`CREATE TABLE IF NOT EXISTS chat_transcript (
		session_id   TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		seq          BIGINT NOT NULL,
		message_id   TEXT NOT NULL,
		thread_id    TEXT NOT NULL,
		parent_id    TEXT NOT NULL DEFAULT '',
		message_kind TEXT NOT NULL CHECK (message_kind IN ('plain','addressed','task')),
		author_type  TEXT NOT NULL DEFAULT '',
		author_id    TEXT NOT NULL DEFAULT '',
		principal_id TEXT NOT NULL DEFAULT '',
		payload      TEXT NULL,
		guard        TEXT NULL,
		audience     TEXT NULL,
		created_at   TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS chat_transcript_message_id_idx
		ON chat_transcript (session_id, message_id)`,
	`CREATE TABLE IF NOT EXISTS chat_deliveries (
		session_id      TEXT NOT NULL,
		message_id      TEXT NOT NULL,
		target_agent_id TEXT NOT NULL,
		inbox_entry_id  TEXT NOT NULL DEFAULT '',
		outcome         TEXT NOT NULL,
		updated_at      TEXT NOT NULL,
		PRIMARY KEY (session_id, message_id, target_agent_id)
	)`,
	`CREATE TABLE IF NOT EXISTS chat_threads (
		thread_id         TEXT PRIMARY KEY,
		session_id        TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		parent_thread_id  TEXT NULL,
		root_message_id   TEXT NOT NULL DEFAULT '',
		anchor_message_id TEXT NULL,
		created_by        TEXT NULL,
		created_at        TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_context_shares (
		session_id          TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type         TEXT NOT NULL,
		member_id           TEXT NOT NULL,
		mode                TEXT NOT NULL,
		boundary_message_id TEXT NOT NULL DEFAULT '',
		set_by              TEXT NOT NULL DEFAULT '',
		set_at              TEXT NOT NULL,
		PRIMARY KEY (session_id, member_type, member_id)
	)`,
}
