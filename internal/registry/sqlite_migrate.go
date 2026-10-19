package registry

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// SQLite follows the complete embedded PostgreSQL migration sequence. Reuse the
// source DDL where SQLite supports it, translating only type spellings. Overrides
// below cover PostgreSQL-only casts/functions, identity, multi-column ALTER,
// ALTER CONSTRAINT and regex syntax. 004's keyless change is folded into 001:
// SQLite cannot drop a column constraint without rebuilding the table.
// Integer timestamps are UTC Unix nanoseconds; NULL expiry means never.
// SQLite 002 also retains entry_guard metadata, matching the memory backend.
var sqliteMigrationOverrides = map[int]string{
	1: `CREATE TABLE agents (
 id TEXT PRIMARY KEY CHECK(length(trim(id)) > 0), public_key BLOB,
 capabilities TEXT NOT NULL DEFAULT '[]' CHECK(json_type(capabilities) = 'array'),
 status TEXT NOT NULL DEFAULT 'online' CHECK(status IN ('online','offline')),
 registered_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
 CHECK(public_key IS NULL OR length(public_key) = 32));
 CREATE INDEX agents_status_last_seen_idx ON agents(status,last_seen DESC);`,
	2: `CREATE TABLE inbox_entries (
 delivery_sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE CHECK(length(trim(id)) > 0),
 agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 payload BLOB NOT NULL, entry_guard TEXT, created_at INTEGER NOT NULL, expires_at INTEGER,
 leased_at INTEGER, lease_id TEXT, lease_expires_at INTEGER,
 acked BOOLEAN NOT NULL DEFAULT FALSE,
 CHECK(expires_at IS NULL OR expires_at > created_at),
 CHECK((leased_at IS NULL AND lease_id IS NULL AND lease_expires_at IS NULL) OR
 (leased_at IS NOT NULL AND lease_id IS NOT NULL AND lease_id <> '' AND lease_expires_at > leased_at)));
 CREATE INDEX inbox_entries_fifo_idx ON inbox_entries(agent_id,delivery_sequence);
 CREATE INDEX inbox_entries_expires_at_idx ON inbox_entries(expires_at);`,
	3: `ALTER TABLE agents ADD COLUMN webhook TEXT; ALTER TABLE agents ADD COLUMN guard TEXT;`,
	4: `-- Keyless public_key constraint is applied in SQLite migration 001.`,
	7: `ALTER TABLE inbox_entries ADD COLUMN priority INTEGER NOT NULL DEFAULT 0 CHECK(priority BETWEEN 0 AND 9);
 CREATE INDEX inbox_entries_priority_idx ON inbox_entries(agent_id,priority DESC,delivery_sequence) WHERE acked = FALSE;`,
	8: `ALTER TABLE agents ADD COLUMN namespace TEXT CHECK(namespace IS NULL OR length(trim(namespace)) > 0);
 ALTER TABLE inbox_entries ADD COLUMN namespace TEXT CHECK(namespace IS NULL OR length(trim(namespace)) > 0);
 ALTER TABLE dead_letters ADD COLUMN namespace TEXT CHECK(namespace IS NULL OR length(trim(namespace)) > 0);`,
	14: `CREATE TABLE service_delivery_receipts (
 sender TEXT NOT NULL CHECK(length(sender)>0), recipient TEXT NOT NULL CHECK(length(recipient)>0),
 idempotency_key TEXT NOT NULL CHECK(length(idempotency_key)>0),
 body_digest TEXT NOT NULL CHECK(length(body_digest)=64 AND body_digest NOT GLOB '*[^0-9a-f]*'),
 status_code INTEGER NOT NULL, message_id TEXT NOT NULL UNIQUE, response_body BLOB NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(sender,recipient,idempotency_key));`,
	// Keep PostgreSQL's lowercase hex, nonblank, uniqueness and JSON-object
	// constraints at the SQL boundary too. Byte lengths also reject NUL suffixes
	// (SQLite text length and GLOB stop at NUL). Signed JSON stays verbatim TEXT.
	17: `CREATE TABLE trust_anchors (
 id TEXT NOT NULL PRIMARY KEY CHECK(trim(id) <> ''),
 public_key TEXT NOT NULL UNIQUE CHECK(length(CAST(public_key AS BLOB)) = 64 AND instr(public_key, char(0)) = 0 AND public_key NOT GLOB '*[^0-9a-f]*'),
 label TEXT NOT NULL CHECK(trim(label) <> ''), created_at INTEGER NOT NULL);
 CREATE TABLE trust_delegations (
 signature TEXT NOT NULL PRIMARY KEY CHECK(length(CAST(signature AS BLOB)) = 128 AND instr(signature, char(0)) = 0 AND signature NOT GLOB '*[^0-9a-f]*'),
 subject_key TEXT NOT NULL CHECK(length(CAST(subject_key AS BLOB)) = 64 AND instr(subject_key, char(0)) = 0 AND subject_key NOT GLOB '*[^0-9a-f]*'),
 record TEXT NOT NULL CHECK(json_valid(record) AND json_type(record) = 'object'));
 CREATE INDEX trust_delegations_subject_idx ON trust_delegations(subject_key);`,
}

// sqliteMigrationDDL is the single dialect-selection seam used by startup and
// the full-chain executable portability guard. PostgreSQL reads the source as-is.
func sqliteMigrationDDL(version int, source string) string {
	if ddl, ok := sqliteMigrationOverrides[version]; ok {
		return ddl
	}
	return strings.NewReplacer("TIMESTAMPTZ", "INTEGER", "JSONB", "TEXT", "BYTEA", "BLOB", "btrim(", "trim(").Replace(source)
}

func (s *SQLiteStore) migrate() error {
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS registry_schema_migrations (version INTEGER PRIMARY KEY);`); err != nil {
			return err
		}
		files, err := fs.Glob(migrationFS, "migrations/*.up.sql")
		if err != nil {
			return err
		}
		for _, name := range files {
			version, err := strconv.Atoi(strings.Split(strings.TrimPrefix(name, "migrations/"), "_")[0])
			if err != nil {
				return err
			}
			var applied int
			if err = q.QueryRowContext(ctx, `SELECT count(*) FROM registry_schema_migrations WHERE version=?`, version).Scan(&applied); err != nil {
				return err
			}
			if applied != 0 {
				continue
			}
			source, err := migrationFS.ReadFile(name)
			if err != nil {
				return err
			}
			ddl := sqliteMigrationDDL(version, string(source))
			if _, err = q.ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("SQLite migration %03d: %w", version, err)
			}
			if _, err = q.ExecContext(ctx, `INSERT INTO registry_schema_migrations(version) VALUES(?)`, version); err != nil {
				return err
			}
		}
		return nil
	})
}
