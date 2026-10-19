package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore is a durable single-file registry. WAL allows readers during a
// writer; BEGIN IMMEDIATE serializes read/modify/write operations across BOTH
// goroutines and independent store handles/processes (not merely a Go mutex).
// Every successful write is committed with synchronous=FULL before returning.
// Back up the database with SQLite's backup API, or close it before copying;
// copying just the main file while WAL writers are active is not a backup.
type SQLiteStore struct {
	inboxAcknowledgements
	db      *sql.DB
	listMu  sync.Mutex
	listErr error
}

type sqliteQueries struct{ *sql.Conn }

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("SQLite registry requires a filesystem path (CR_REGISTRY_SQLITE_PATH)")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	params := url.Values{"_pragma": []string{"busy_timeout(10000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)"}}
	u.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &SQLiteStore{db: db}
	if err = s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize SQLite registry: %w", err)
	}
	return s, nil
}
func (s *SQLiteStore) Close() error { return s.db.Close() }
func (s *SQLiteStore) Healthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.db.PingContext(ctx) == nil
}
func (s *SQLiteStore) transaction(fn func(context.Context, *sqliteQueries) error) error {
	return s.withTransaction("BEGIN IMMEDIATE", fn)
}
func (s *SQLiteStore) withTransaction(begin string, fn func(context.Context, *sqliteQueries) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		return err
	}
	// A canceled caller context must not leave a transaction on a pooled conn.
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = conn.ExecContext(cleanup, "ROLLBACK")
	}()
	if err = fn(ctx, &sqliteQueries{conn}); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
func sqliteTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}
func sqliteAt(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(0, n.Int64).UTC()
}
func sqliteJSON(v any) ([]byte, error) { return json.Marshal(v) }

const sqliteAgentColumns = `id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, COALESCE(namespace,'')`

type sqliteScanner interface{ Scan(...any) error }

func scanSQLiteAgent(row sqliteScanner) (*Agent, error) {
	a := &Agent{}
	var key, caps, wh, g, aa []byte
	var registered, last int64
	if err := row.Scan(&a.ID, &key, &caps, &a.Status, &registered, &last, &wh, &g, &aa, &a.Namespace); err != nil {
		return nil, err
	}
	a.PublicKey = HexKey(key)
	a.RegisteredAt = time.Unix(0, registered).UTC()
	a.LastSeen = time.Unix(0, last).UTC()
	for _, item := range []struct {
		raw []byte
		out any
	}{{caps, &a.Capabilities}, {wh, &a.Webhook}, {g, &a.Guard}, {aa, &a.A2A}} {
		if len(item.raw) > 0 {
			if err := json.Unmarshal(item.raw, item.out); err != nil {
				return nil, err
			}
		}
	}
	return a, nil
}
func sqliteGet(ctx context.Context, q *sqliteQueries, id string) (*Agent, error) {
	a, err := scanSQLiteAgent(q.QueryRowContext(ctx, `SELECT `+sqliteAgentColumns+` FROM agents WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return a, err
}
func (s *SQLiteStore) Get(id string) (*Agent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := scanSQLiteAgent(s.db.QueryRowContext(ctx, `SELECT `+sqliteAgentColumns+` FROM agents WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return a, err
}
func (s *SQLiteStore) Register(a *Agent) error {
	if a == nil {
		return ErrInvalidStoreInput
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		old, err := sqliteGet(ctx, q, a.ID)
		if err == nil {
			if registrationKeyDiffers(old, a) {
				return fmt.Errorf("%w: %q", ErrPublicKeyMismatch, a.ID)
			}
			return fmt.Errorf("%w: %q", ErrAgentExists, a.ID)
		}
		if !errors.Is(err, ErrAgentNotFound) {
			return err
		}
		a.RegisteredAt = time.Now().UTC()
		a.LastSeen = a.RegisteredAt
		if a.Status == "" {
			a.Status = StatusOnline
		}
		if a.Capabilities == nil {
			a.Capabilities = []string{}
		}
		caps, err := sqliteJSON(a.Capabilities)
		if err != nil {
			return err
		}
		wh, err := sqliteJSON(a.Webhook)
		if err != nil {
			return err
		}
		g, err := sqliteJSON(a.Guard)
		if err != nil {
			return err
		}
		aa, err := sqliteJSON(a.A2A)
		if err != nil {
			return err
		}
		key := []byte(a.PublicKey)
		if len(key) == 0 {
			key = nil
		}
		_, err = q.ExecContext(ctx, `INSERT INTO agents(id,public_key,capabilities,status,registered_at,last_seen,webhook,guard,a2a,namespace) VALUES(?,?,?,?,?,?,?,?,?,?)`, a.ID, key, caps, a.Status, sqliteTime(a.RegisteredAt), sqliteTime(a.LastSeen), wh, g, aa, nullableSQLiteString(a.Namespace))
		return err
	})
}
func nullableSQLiteString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func (s *SQLiteStore) List() []*Agent {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	out := make([]*Agent, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqliteAgentColumns+` FROM agents ORDER BY id`)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			a, e := scanSQLiteAgent(rows)
			if e != nil {
				err = e
				break
			}
			out = append(out, a)
		}
		if err == nil {
			err = rows.Err()
		}
	}
	s.listErr = err
	if err != nil {
		slog.Error("SQLite registry list", "error", err)
		return nil
	}
	return out
}
func (s *SQLiteStore) ListError() error { s.listMu.Lock(); defer s.listMu.Unlock(); return s.listErr }
func (s *SQLiteStore) Unregister(id string) error {
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		r, err := q.ExecContext(ctx, `DELETE FROM agents WHERE id=?`, id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err == nil && n == 0 {
			return ErrAgentNotFound
		}
		return err
	})
}
func (s *SQLiteStore) Update(a *Agent) error {
	if a == nil {
		return ErrInvalidStoreInput
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		old, err := sqliteGet(ctx, q, a.ID)
		if err != nil {
			return err
		}
		a.Status = old.Status
		a.RegisteredAt = old.RegisteredAt
		a.Namespace = old.Namespace
		a.LastSeen = time.Now().UTC()
		if a.Capabilities == nil {
			a.Capabilities = []string{}
		}
		caps, err := sqliteJSON(a.Capabilities)
		if err != nil {
			return err
		}
		wh, err := sqliteJSON(a.Webhook)
		if err != nil {
			return err
		}
		g, err := sqliteJSON(a.Guard)
		if err != nil {
			return err
		}
		aa, err := sqliteJSON(a.A2A)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `UPDATE agents SET capabilities=?,webhook=?,guard=?,a2a=?,last_seen=? WHERE id=?`, caps, wh, g, aa, sqliteTime(a.LastSeen), a.ID)
		return err
	})
}
func (s *SQLiteStore) Touch(id string, at time.Time) error {
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, err := sqliteGet(ctx, q, id); err != nil {
			return err
		}
		if at.IsZero() {
			at = time.Now()
		}
		_, err := q.ExecContext(ctx, `UPDATE agents SET last_seen=max(last_seen,?) WHERE id=?`, sqliteTime(at), id)
		return err
	})
}

// The row codec is shared by claim, peek, close, purge and transfer. Lease
// duration is reconstructed from persisted timestamps, not lost at JSON's
// LeaseDuration json:"-" field. Payload is BLOB, so arbitrary bytes survive.
const sqliteEntryColumns = `id,agent_id,payload,created_at,expires_at,leased_at,lease_id,lease_expires_at,acked,COALESCE(sender,''),COALESCE(idempotency_key,''),priority,COALESCE(namespace,''),COALESCE(thread_id,''),location,COALESCE(delivery_ms,0),COALESCE(principal_id,''),selection,COALESCE(reply_to,''),entry_guard`

func scanSQLiteEntry(row sqliteScanner) (*InboxEntry, error) {
	e := &InboxEntry{}
	var created int64
	var expiry, leased, until sql.NullInt64
	var lease sql.NullString
	var loc, selection, guard []byte
	err := row.Scan(&e.ID, &e.AgentID, &e.Payload, &created, &expiry, &leased, &lease, &until, &e.ACKed, &e.Sender, &e.IdempotencyKey, &e.Priority, &e.Namespace, &e.ThreadID, &loc, &e.DeliveryMs, &e.PrincipalID, &selection, &e.ReplyTo, &guard)
	if err != nil {
		return nil, err
	}
	e.CreatedAt = time.Unix(0, created).UTC()
	e.ExpiresAt = sqliteAt(expiry)
	e.LeaseID = lease.String
	if leased.Valid {
		at := sqliteAt(leased)
		e.LeasedAt = &at
		e.LeaseDuration = time.Duration(until.Int64 - leased.Int64)
	}
	if len(loc) > 0 {
		if err = json.Unmarshal(loc, &e.Location); err != nil {
			return nil, err
		}
	}
	if len(guard) > 0 {
		if err = json.Unmarshal(guard, &e.Guard); err != nil {
			return nil, err
		}
	}
	applySelectionJSONB(e, selection)
	return e, nil
}
func sqliteEntries(ctx context.Context, q *sqliteQueries, agentID string) ([]*InboxEntry, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries WHERE agent_id=? ORDER BY delivery_sequence`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*InboxEntry, 0)
	for rows.Next() {
		e, err := scanSQLiteEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func sqliteDeliver(ctx context.Context, q *sqliteQueries, agentID string, e *InboxEntry) error {
	if e == nil {
		return ErrInvalidStoreInput
	}
	if _, err := sqliteGet(ctx, q, agentID); err != nil {
		return err
	}
	e.AgentID = agentID
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if err := resolveMessageExpiry(e); err != nil {
		return err
	}
	if e.ID == "" {
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		e.ID = id
	}
	e.LeasedAt = nil
	e.LeaseID = ""
	e.LeaseDuration = 0
	e.ACKed = false
	loc, err := sqliteJSON(e.Location)
	if err != nil {
		return err
	}
	guard, err := sqliteJSON(e.Guard)
	if err != nil {
		return err
	}
	selection := selectionJSONB(e)
	payload := e.Payload
	if payload == nil {
		payload = []byte{}
	}
	_, err = q.ExecContext(ctx, `INSERT INTO inbox_entries(id,agent_id,payload,created_at,expires_at,sender,idempotency_key,priority,namespace,thread_id,location,delivery_ms,principal_id,selection,reply_to,entry_guard) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, agentID, payload, sqliteTime(e.CreatedAt), sqliteTime(e.ExpiresAt), nullableSQLiteString(e.Sender), nullableSQLiteString(e.IdempotencyKey), e.Priority, nullableSQLiteString(e.Namespace), nullableSQLiteString(e.ThreadID), loc, e.DeliveryMs, nullableSQLiteString(e.PrincipalID), selection, nullableSQLiteString(e.ReplyTo), guard)
	return err
}
func (s *SQLiteStore) Deliver(agentID string, e *InboxEntry) error {
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error { return sqliteDeliver(ctx, q, agentID, e) })
}
func (s *SQLiteStore) Peek(agentID, messageID string) (*InboxEntry, error) {
	if agentID == "" || messageID == "" {
		return nil, ErrInvalidStoreInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e, err := scanSQLiteEntry(s.db.QueryRowContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries WHERE agent_id=? AND id=?`, agentID, messageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMessageNotFound
	}
	return e, err
}
func (s *SQLiteStore) PeekInbox(agentID string) (out []*InboxEntry, err error) {
	if agentID == "" {
		return nil, ErrInvalidStoreInput
	}
	// A deferred read transaction keeps existence and rows on one snapshot;
	// unlike BEGIN IMMEDIATE it does not reserve the writer slot.
	err = s.withTransaction("BEGIN", func(ctx context.Context, q *sqliteQueries) error {
		if _, e := sqliteGet(ctx, q, agentID); e != nil {
			return e
		}
		var e error
		out, e = sqliteEntries(ctx, q, agentID)
		return e
	})
	return out, err
}
func (s *SQLiteStore) Retrieve(agentID string, d time.Duration, max int) (out []*InboxEntry, lease string, err error) {
	out = make([]*InboxEntry, 0)
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, e := sqliteGet(ctx, q, agentID); e != nil {
			return e
		}
		if max <= 0 {
			return nil
		}
		now := time.Now().UTC()
		rows, e := q.QueryContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries WHERE agent_id=? AND acked=FALSE AND (expires_at IS NULL OR expires_at>=?) AND (lease_id IS NULL OR lease_expires_at<?) ORDER BY priority DESC,delivery_sequence LIMIT ?`, agentID, now.UnixNano(), now.UnixNano(), max)
		if e != nil {
			return e
		}
		for rows.Next() {
			entry, e := scanSQLiteEntry(rows)
			if e != nil {
				_ = rows.Close()
				return e
			}
			out = append(out, entry)
		}
		e = rows.Err()
		_ = rows.Close()
		if e != nil {
			return e
		}
		if len(out) == 0 {
			return nil
		}
		lease, e = newLeaseID()
		if e != nil {
			return e
		}
		effective := d
		if effective == 0 {
			effective = 30 * time.Second
		}
		for _, entry := range out {
			if _, e = q.ExecContext(ctx, `UPDATE inbox_entries SET leased_at=?,lease_id=?,lease_expires_at=? WHERE agent_id=? AND id=?`, now.UnixNano(), lease, now.Add(effective).UnixNano(), agentID, entry.ID); e != nil {
				return e
			}
			at := now
			entry.LeasedAt = &at
			entry.LeaseID = lease
			entry.LeaseDuration = effective
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, lease, nil
}
func (s *SQLiteStore) Ack(agentID, lease string, ids []string) error {
	err := s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, err := sqliteGet(ctx, q, agentID); err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrInvalidStoreInput
		}
		entries := map[string]*InboxEntry{}
		var missing, mismatch []string
		for _, id := range ids {
			e, err := scanSQLiteEntry(q.QueryRowContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries WHERE agent_id=? AND id=?`, agentID, id))
			if errors.Is(err, sql.ErrNoRows) {
				missing = append(missing, id)
				continue
			}
			if err != nil {
				return err
			}
			entries[id] = e
			if e.LeaseID != lease {
				mismatch = append(mismatch, id)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: %s", ErrMessageNotFound, quoteMessageIDs(missing))
		}
		if len(mismatch) > 0 {
			return fmt.Errorf("%w: %s", ErrLeaseConflict, quoteMessageIDs(mismatch))
		}
		for id, e := range entries {
			o, task, err := acknowledgementObservation(e, time.Now().UTC())
			if err != nil {
				return err
			}
			if task {
				if err = sqliteSaveObservation(ctx, q, agentID, o); err != nil {
					return err
				}
			}
			if _, err = q.ExecContext(ctx, `DELETE FROM inbox_entries WHERE agent_id=? AND id=?`, agentID, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.recordAcknowledgement(agentID, ids)
	}
	return err
}
func (s *SQLiteStore) Stats(id string) (depth, leased int, age time.Duration, err error) {
	now := time.Now()
	err = s.withTransaction("BEGIN", func(ctx context.Context, q *sqliteQueries) error {
		if _, e := sqliteGet(ctx, q, id); e != nil {
			return e
		}
		var oldest sql.NullInt64
		e := q.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN lease_id IS NOT NULL THEN 1 ELSE 0 END),0),min(created_at) FROM inbox_entries WHERE agent_id=? AND acked=FALSE AND (expires_at IS NULL OR expires_at>=?)`, id, now.UnixNano()).Scan(&depth, &leased, &oldest)
		if oldest.Valid {
			age = now.Sub(sqliteAt(oldest))
		}
		return e
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return depth, leased, age, nil
}
func (s *SQLiteStore) QueueDepth() (depth QueueDepth, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var oldest sql.NullInt64
	now := time.Now()
	err = s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN lease_id IS NOT NULL THEN 1 ELSE 0 END),0),min(created_at) FROM inbox_entries WHERE acked=FALSE AND (expires_at IS NULL OR expires_at>=?)`, now.UnixNano()).Scan(&depth.Pending, &depth.Leased, &oldest)
	if oldest.Valid {
		depth.OldestAge = now.Sub(sqliteAt(oldest))
	}
	return depth, err
}
func (s *SQLiteStore) PurgeExpired() int { return s.PurgeExpiredReport(nil) }
func (s *SQLiteStore) PurgeExpiredReport(report func(string, *InboxEntry)) int {
	var expired []*InboxEntry
	err := s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		now := time.Now()
		rows, err := q.QueryContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries WHERE expires_at<?`, now.UnixNano())
		if err != nil {
			return err
		}
		for rows.Next() {
			e, err := scanSQLiteEntry(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			expired = append(expired, e)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if _, err = q.ExecContext(ctx, `DELETE FROM inbox_entries WHERE expires_at<?`, now.UnixNano()); err != nil {
			return err
		}
		if _, err = q.ExecContext(ctx, `UPDATE inbox_entries SET leased_at=NULL,lease_id=NULL,lease_expires_at=NULL WHERE lease_expires_at<?`, now.UnixNano()); err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `DELETE FROM dead_letters WHERE dead_lettered_at<?`, now.Add(-DefaultDeadLetterRetention).UnixNano())
		return err
	})
	if err != nil {
		slog.Error("SQLite registry purge", "error", err)
		return 0
	}
	for _, e := range expired {
		if report != nil {
			report(e.AgentID, e)
		}
	}
	return len(expired)
}
func (s *SQLiteStore) RevokeLeases(id string) (n int, err error) {
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		r, err := q.ExecContext(ctx, `UPDATE inbox_entries SET leased_at=NULL,lease_id=NULL,lease_expires_at=NULL WHERE agent_id=? AND acked=FALSE AND lease_id IS NOT NULL`, id)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		n = int(count)
		return err
	})
	return n, err
}
func (s *SQLiteStore) CloseEntry(agentID, messageID string) error {
	if agentID == "" || messageID == "" {
		return ErrInvalidStoreInput
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error { return sqliteClose(ctx, q, agentID, messageID) })
}
func sqliteClose(ctx context.Context, q *sqliteQueries, agentID, messageID string) error {
	if _, err := sqliteGet(ctx, q, agentID); err != nil {
		return err
	}
	r, err := q.ExecContext(ctx, `DELETE FROM inbox_entries WHERE agent_id=? AND id=?`, agentID, messageID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return ErrMessageNotFound
	}
	return err
}

var (
	_ Store             = (*SQLiteStore)(nil)
	_ updater           = (*SQLiteStore)(nil)
	_ Toucher           = (*SQLiteStore)(nil)
	_ ListErrorReporter = (*SQLiteStore)(nil)
	_ HealthReporter    = (*SQLiteStore)(nil)
	_ InboxLister       = (*SQLiteStore)(nil)
	_ InboxPeeker       = (*SQLiteStore)(nil)
	_ PurgeReporter     = (*SQLiteStore)(nil)
	_ DepthReporter     = (*SQLiteStore)(nil)
	_ InboxAckObserver  = (*SQLiteStore)(nil)
	_ InboxCloser       = (*SQLiteStore)(nil)
	_ LeaseRevoker      = (*SQLiteStore)(nil)
	_ DeadLetterLookup  = (*SQLiteStore)(nil)
)
