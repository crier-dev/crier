package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Source is a registered webhook identity. Secret is never serialized by the
// public source-list endpoint.
type Source struct {
	ID        string    `json:"source_id"`
	Secret    []byte    `json:"-"`
	Mapping   string    `json:"mapping"`
	DeliverTo string    `json:"deliver_to"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is the durable source, deduplication and transactional-outbox ledger.
type Store struct {
	db   *sql.DB
	path string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("ingest store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(path)+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS ingest_sources (id TEXT PRIMARY KEY, secret BLOB NOT NULL, mapping TEXT NOT NULL, deliver_to TEXT NOT NULL DEFAULT '', tags BLOB NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ingest_dedup (source_id TEXT NOT NULL, event_key TEXT NOT NULL, nonce TEXT NOT NULL, message_id TEXT NOT NULL, payload BLOB NOT NULL, created_at INTEGER NOT NULL, delivered INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(source_id,event_key,nonce))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := ensureDeliverToColumn(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func ensureDeliverToColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(ingest_sources)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "deliver_to" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ingest_sources ADD COLUMN deliver_to TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) CreateSource(ctx context.Context, source Source) error {
	tags, err := json.Marshal(source.Tags)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ingest_sources(id,secret,mapping,deliver_to,tags,created_at) VALUES(?,?,?,?,?,?)`, source.ID, source.Secret, source.Mapping, source.DeliverTo, tags, source.CreatedAt.UnixNano())
	return err
}
func (s *Store) Source(ctx context.Context, id string) (Source, error) {
	var out Source
	var tags []byte
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,secret,mapping,deliver_to,tags,created_at FROM ingest_sources WHERE id=?`, id).Scan(&out.ID, &out.Secret, &out.Mapping, &out.DeliverTo, &tags, &created)
	if err != nil {
		return Source{}, err
	}
	if err := json.Unmarshal(tags, &out.Tags); err != nil {
		return Source{}, err
	}
	out.CreatedAt = time.Unix(0, created).UTC()
	return out, nil
}
func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,mapping,deliver_to,tags,created_at FROM ingest_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Source{}
	for rows.Next() {
		var x Source
		var tags []byte
		var created int64
		if err := rows.Scan(&x.ID, &x.Mapping, &x.DeliverTo, &tags, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tags, &x.Tags); err != nil {
			return nil, err
		}
		x.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

// Accept writes the durable dedup receipt and outbox row in one SQLite commit.
// A duplicate returns the previously accepted message id.
func (s *Store) Accept(ctx context.Context, source, key, nonce, messageID string, payload []byte, at time.Time) (bool, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DELETE FROM ingest_dedup WHERE created_at < ?`, at.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return false, "", err
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT message_id FROM ingest_dedup WHERE source_id=? AND event_key=? AND nonce=?`, source, key, nonce).Scan(&old)
	if err == nil {
		return true, old, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ingest_dedup(source_id,event_key,nonce,message_id,payload,created_at,delivered) VALUES(?,?,?,?,?,?,0)`, source, key, nonce, messageID, payload, at.UnixNano()); err != nil {
		return false, "", err
	}
	if err = tx.Commit(); err != nil {
		return false, "", err
	}
	return false, messageID, nil
}
func (s *Store) Outbox(ctx context.Context, source, key, nonce string) ([]byte, bool, error) {
	var payload []byte
	var delivered int
	err := s.db.QueryRowContext(ctx, `SELECT payload,delivered FROM ingest_dedup WHERE source_id=? AND event_key=? AND nonce=?`, source, key, nonce).Scan(&payload, &delivered)
	return payload, delivered == 1, err
}

type PendingRow struct {
	Source    Source
	Key       string
	MessageID string
	Payload   []byte
}

func (s *Store) PendingRows(ctx context.Context) ([]PendingRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.source_id,d.event_key,d.message_id,d.payload,s.mapping,s.deliver_to,s.tags,s.created_at FROM ingest_dedup d JOIN ingest_sources s ON s.id=d.source_id WHERE d.delivered=0 ORDER BY d.created_at,d.message_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	pending := []PendingRow{}
	for rows.Next() {
		var row PendingRow
		var tags []byte
		var created int64
		if err := rows.Scan(&row.Source.ID, &row.Key, &row.MessageID, &row.Payload, &row.Source.Mapping, &row.Source.DeliverTo, &tags, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tags, &row.Source.Tags); err != nil {
			return nil, err
		}
		row.Source.CreatedAt = time.Unix(0, created).UTC()
		pending = append(pending, row)
	}
	return pending, rows.Err()
}

func (s *Store) MarkDelivered(ctx context.Context, source, key, messageID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE ingest_dedup SET delivered=1 WHERE source_id=? AND event_key=? AND message_id=? AND delivered=0`, source, key, messageID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("mark delivered: expected one pending row, updated %d", n)
	}
	return nil
}

func (s *Store) Pending(ctx context.Context, source, key, nonce string) (bool, error) {
	var delivered int
	err := s.db.QueryRowContext(ctx, `SELECT delivered FROM ingest_dedup WHERE source_id=? AND event_key=? AND nonce=?`, source, key, nonce).Scan(&delivered)
	if err != nil {
		return false, fmt.Errorf("read ingest receipt: %w", err)
	}
	return delivered == 0, nil
}
