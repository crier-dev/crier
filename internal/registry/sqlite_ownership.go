package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const sqliteDeadColumns = `message_id,agent_id,COALESCE(sender,''),payload,created_at,expires_at,dead_lettered_at,reason,COALESCE(idempotency_key,''),COALESCE(namespace,''),COALESCE(principal_id,'')`

func scanSQLiteDead(row sqliteScanner) (*DeadLetter, error) {
	d := &DeadLetter{}
	var created, expiry, dead int64
	err := row.Scan(&d.MessageID, &d.AgentID, &d.Sender, &d.Payload, &created, &expiry, &dead, &d.Reason, &d.IdempotencyKey, &d.Namespace, &d.PrincipalID)
	d.CreatedAt = time.Unix(0, created).UTC()
	d.ExpiredAt = time.Unix(0, expiry).UTC()
	d.DeadLetteredAt = time.Unix(0, dead).UTC()
	return d, err
}
func (s *SQLiteStore) AppendDeadLetter(d *DeadLetter) (added bool, err error) {
	if d == nil || d.MessageID == "" {
		return false, ErrInvalidStoreInput
	}
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		payload := d.Payload
		if payload == nil {
			payload = []byte{}
		}
		r, err := q.ExecContext(ctx, `INSERT INTO dead_letters(message_id,agent_id,sender,payload,created_at,expires_at,dead_lettered_at,reason,idempotency_key,namespace,principal_id) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(message_id) DO NOTHING`, d.MessageID, d.AgentID, nullableSQLiteString(d.Sender), payload, d.CreatedAt.UnixNano(), d.ExpiredAt.UnixNano(), d.DeadLetteredAt.UnixNano(), d.Reason, nullableSQLiteString(d.IdempotencyKey), nullableSQLiteString(d.Namespace), nullableSQLiteString(d.PrincipalID))
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		added = n > 0
		return err
	})
	return added, err
}
func (s *SQLiteStore) ListDeadLetters(id string, limit int) ([]*DeadLetter, error) {
	if id == "" || limit > MaxDeadLetterListLimit {
		return nil, ErrInvalidStoreInput
	}
	if limit <= 0 {
		limit = DefaultDeadLetterListLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqliteDeadColumns+` FROM dead_letters WHERE agent_id=? ORDER BY dead_lettered_at DESC,rowid DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*DeadLetter, 0)
	for rows.Next() {
		d, err := scanSQLiteDead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) LookupDeadLetter(id, message string) (*DeadLetter, bool, error) {
	if id == "" || message == "" {
		return nil, false, ErrInvalidStoreInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := scanSQLiteDead(s.db.QueryRowContext(ctx, `SELECT `+sqliteDeadColumns+` FROM dead_letters WHERE agent_id=? AND message_id=?`, id, message))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return d, err == nil, err
}
func (s *SQLiteStore) Transfer(id, lease string, ids []string, target string, force bool) (n int, err error) {
	if id == "" || target == "" || id == target || len(ids) == 0 {
		return 0, ErrInvalidStoreInput
	}
	seen := map[string]bool{}
	for _, message := range ids {
		if seen[message] {
			return 0, ErrInvalidStoreInput
		}
		seen[message] = true
	}
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, err := sqliteGet(ctx, q, id); err != nil {
			return err
		}
		if _, err := sqliteGet(ctx, q, target); err != nil {
			return err
		}
		entries, err := sqliteEntries(ctx, q, id)
		if err != nil {
			return err
		}
		var moving []*InboxEntry
		conflict := false
		for _, e := range entries {
			if !seen[e.ID] {
				continue
			}
			moving = append(moving, e)
			if !force && e.LeaseID != "" && e.LeaseID != lease {
				conflict = true
			}
		}
		if len(moving) != len(ids) {
			return ErrMessageNotFound
		}
		if conflict {
			return ErrLeaseConflict
		}
		// Delete/reinsert only the moved rows: new delivery_sequence places them at
		// the tail of the target queue, while keeping payload/creation/expiry intact.
		for _, e := range moving {
			if err = sqliteClose(ctx, q, id, e.ID); err != nil {
				return err
			}
			zero := 0
			e.TTLSeconds = &zero
			if err = sqliteDeliver(ctx, q, target, e); err != nil {
				return err
			}
		}
		n = len(moving)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
func sqliteLookupReceipt(row sqliteScanner, digest string) (*ServiceDeliveryReceipt, error) {
	r := &ServiceDeliveryReceipt{}
	err := row.Scan(&r.Status, &r.MessageID, &r.BodyDigest, &r.Response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.BodyDigest != digest {
		return nil, ErrServiceIdempotencyConflict
	}
	return r, nil
}

const sqliteReceiptQuery = `SELECT status_code,message_id,body_digest,response_body FROM service_delivery_receipts WHERE sender=? AND recipient=? AND idempotency_key=?`

func (s *SQLiteStore) LookupServiceDelivery(sender, recipient, key, digest string) (*ServiceDeliveryReceipt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sqliteLookupReceipt(s.db.QueryRowContext(ctx, sqliteReceiptQuery, sender, recipient, key), digest)
}
func (s *SQLiteStore) DeliverServiceIdempotent(sender, recipient, key, digest string, e *InboxEntry, r ServiceDeliveryReceipt) (out *ServiceDeliveryReceipt, replay bool, err error) {
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		var err error
		out, err = sqliteLookupReceipt(q.QueryRowContext(ctx, sqliteReceiptQuery, sender, recipient, key), digest)
		if err != nil {
			return err
		}
		if out != nil {
			replay = true
			return nil
		}
		if err = sqliteDeliver(ctx, q, recipient, e); err != nil {
			return err
		}
		r.MessageID = e.ID
		r.BodyDigest = digest
		_, err = q.ExecContext(ctx, `INSERT INTO service_delivery_receipts(sender,recipient,idempotency_key,body_digest,status_code,message_id,response_body,created_at) VALUES(?,?,?,?,?,?,?,?)`, sender, recipient, key, digest, r.Status, r.MessageID, r.Response, time.Now().UnixNano())
		if err == nil {
			out = cloneServiceReceipt(r)
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, replay, nil
}

var (
	_ DeadLetterStore        = (*SQLiteStore)(nil)
	_ Transferrer            = (*SQLiteStore)(nil)
	_ ServiceIdempotentStore = (*SQLiteStore)(nil)
)
