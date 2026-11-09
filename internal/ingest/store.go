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
		`CREATE TABLE IF NOT EXISTS feed_state (feed_id TEXT PRIMARY KEY, state_json BLOB NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ingest_deadletter (source_id TEXT NOT NULL, event_key TEXT NOT NULL, message_id TEXT NOT NULL, row_ids BLOB NOT NULL, attempts INTEGER NOT NULL, last_error TEXT NOT NULL, last_at INTEGER NOT NULL, PRIMARY KEY(source_id,event_key))`,
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

func (s *Store) LoadFeedState(ctx context.Context, id string) (FeedState, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT state_json FROM feed_state WHERE feed_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return FeedState{FeedID: id, Seen: map[string]time.Time{}}, nil
	}
	if err != nil {
		return FeedState{}, err
	}
	var state FeedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return FeedState{}, err
	}
	if state.Seen == nil {
		state.Seen = map[string]time.Time{}
	}
	return state, nil
}

func (s *Store) SaveFeedState(ctx context.Context, state FeedState) error {
	pruneSeen(state.Seen, time.Now().UTC())
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO feed_state(feed_id,state_json,updated_at) VALUES(?,?,?) ON CONFLICT(feed_id) DO UPDATE SET state_json=excluded.state_json,updated_at=excluded.updated_at`, state.FeedID, raw, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM feed_state WHERE updated_at < ?`, time.Now().UTC().Add(-feedStateTTL).UnixNano())
	return err
}

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
	// Attempts is the dead-letter failure count recorded for this row's
	// logical event key (CRIER-GITHOOK-DISPATCH §10 amendment 2). 0 means the
	// row has never failed a delivery.
	Attempts int
}

func (s *Store) PendingRows(ctx context.Context) ([]PendingRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.source_id,d.event_key,d.message_id,d.payload,s.mapping,s.deliver_to,s.tags,d.created_at,COALESCE(l.attempts,0) FROM ingest_dedup d JOIN ingest_sources s ON s.id=d.source_id LEFT JOIN ingest_deadletter l ON l.source_id=d.source_id AND l.event_key=d.event_key WHERE d.delivered=0 ORDER BY d.created_at,d.message_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	pending := []PendingRow{}
	for rows.Next() {
		var row PendingRow
		var tags []byte
		var created int64
		if err := rows.Scan(&row.Source.ID, &row.Key, &row.MessageID, &row.Payload, &row.Source.Mapping, &row.Source.DeliverTo, &tags, &created, &row.Attempts); err != nil {
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

// SeenContentHash reports whether ANY ledger row for the source carries a
// logical event key ending in the given content hash — the rebase probe of
// §10 amendment 3: a rebase/cherry-pick that rewrites a row's id re-queues a
// clone whose key hashes the SAME content, and that clone is absorbed instead
// of delivered twice. A same-id content edit changes the hash and therefore
// never matches (it is a NEW logical event).
func (s *Store) SeenContentHash(ctx context.Context, source, contentHash string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM ingest_dedup WHERE source_id=? AND event_key LIKE '%:' || ? LIMIT 1`, source, contentHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeadLetter is one intake batch that exhausted its delivery retry budget
// (CRIER-GITHOOK-DISPATCH §10 amendment 2 — the operator-visible successor of
// drop-with-log, which is REMOVED from the contract). Count is the number of
// failed delivery attempts, LastError/LastAt the most recent failure, RowIDs
// the board row ids the payload carried.
type DeadLetter struct {
	SourceID  string    `json:"source_id"`
	EventKey  string    `json:"event_key"`
	MessageID string    `json:"message_id"`
	RowIDs    []string  `json:"row_ids"`
	Count     int       `json:"count"`
	LastError string    `json:"last_error"`
	LastAt    time.Time `json:"last_at"`
}

// SeenEventKey reports whether ANY ledger row exists for the logical event key
// (source + event_key, any nonce, delivered or not). This is the dedup probe
// the reconciliation sweep diffs the board against: SEEN and ADDED for the
// same row map to ONE key (§10 amendment 3), so one ledger hit — regardless of
// which verb enqueued it or which nonce carried it — absorbs the other.
func (s *Store) SeenEventKey(ctx context.Context, source, eventKey string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM ingest_dedup WHERE source_id=? AND event_key=? LIMIT 1`, source, eventKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RecordDeadLetter upserts one failed delivery attempt against the logical
// event key: the count accumulates across attempts, the last failure and row
// ids are overwritten, and the row is NEVER deleted by a failure — a batch
// that exhausts its budget stays queryable here instead of a dropped.log line.
func (s *Store) RecordDeadLetter(ctx context.Context, source, eventKey, messageID string, rowIDs []string, failure error, at time.Time) (DeadLetter, error) {
	ids, err := json.Marshal(rowIDs)
	if err != nil {
		return DeadLetter{}, err
	}
	reason := ""
	if failure != nil {
		reason = failure.Error()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ingest_deadletter(source_id,event_key,message_id,row_ids,attempts,last_error,last_at) VALUES(?,?,?,?,1,?,?) ON CONFLICT(source_id,event_key) DO UPDATE SET attempts=attempts+1, message_id=excluded.message_id, row_ids=excluded.row_ids, last_error=excluded.last_error, last_at=excluded.last_at`,
		source, eventKey, messageID, ids, reason, at.UnixNano())
	if err != nil {
		return DeadLetter{}, err
	}
	return s.DeadLetter(ctx, source, eventKey)
}

// DeadLetter reads one entry; sql.ErrNoRows when the batch has no recorded
// failure (or its delivery later succeeded and the entry was cleared).
func (s *Store) DeadLetter(ctx context.Context, source, eventKey string) (DeadLetter, error) {
	var out DeadLetter
	var ids []byte
	var lastAt int64
	out.SourceID, out.EventKey = source, eventKey
	err := s.db.QueryRowContext(ctx, `SELECT message_id,row_ids,attempts,last_error,last_at FROM ingest_deadletter WHERE source_id=? AND event_key=?`, source, eventKey).Scan(&out.MessageID, &ids, &out.Count, &out.LastError, &lastAt)
	if err != nil {
		return DeadLetter{}, err
	}
	if err := json.Unmarshal(ids, &out.RowIDs); err != nil {
		return DeadLetter{}, err
	}
	out.LastAt = time.Unix(0, lastAt).UTC()
	return out, nil
}

// DeadLetters lists every entry, most recent failure first — the operator's
// view of what the delivery path gave up on.
func (s *Store) DeadLetters(ctx context.Context) ([]DeadLetter, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id,event_key,message_id,row_ids,attempts,last_error,last_at FROM ingest_deadletter ORDER BY last_at DESC,event_key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []DeadLetter{}
	for rows.Next() {
		var d DeadLetter
		var ids []byte
		var lastAt int64
		if err := rows.Scan(&d.SourceID, &d.EventKey, &d.MessageID, &ids, &d.Count, &d.LastError, &lastAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(ids, &d.RowIDs); err != nil {
			return nil, err
		}
		d.LastAt = time.Unix(0, lastAt).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearDeadLetter removes the entry for a logical event key — called on a
// successful delivery so a batch that recovered (transport back up) stops
// reading as dead.
func (s *Store) ClearDeadLetter(ctx context.Context, source, eventKey string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ingest_deadletter WHERE source_id=? AND event_key=?`, source, eventKey)
	return err
}

// RequeueDeadLetter is the operator re-drive: clearing the entry resets the
// attempt count, so the next Drain retries the batch. The outbox row was
// never deleted — the payload is still in the ledger.
func (s *Store) RequeueDeadLetter(ctx context.Context, source, eventKey string) error {
	return s.ClearDeadLetter(ctx, source, eventKey)
}
