package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Migration 017 owns the schema. Only the immutable record's indexed hex
// projections are normalized; signed JSON (including time offsets) is retained.
func sqliteTrustInsertError(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE) {
		return ErrTrustExists
	}
	return err
}

func (s *SQLiteStore) AddTrustAnchor(a trust.ChainAnchor) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		_, err := q.ExecContext(ctx, `INSERT INTO trust_anchors (id, public_key, label, created_at) VALUES (?,?,?,?)`, a.ID, strings.ToLower(a.PublicKey), a.Label, sqliteTime(a.CreatedAt))
		return sqliteTrustInsertError(err)
	})
}

func (s *SQLiteStore) ListTrustAnchors() ([]trust.ChainAnchor, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, public_key, label, created_at FROM trust_anchors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []trust.ChainAnchor{}
	for rows.Next() {
		var a trust.ChainAnchor
		var at sql.NullInt64
		if err := rows.Scan(&a.ID, &a.PublicKey, &a.Label, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = sqliteAt(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) AddTrustDelegation(d trust.ChainDelegation) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	anchors, err := s.ListTrustAnchors()
	if err != nil {
		return err
	}
	held, err := s.ListTrustDelegations("")
	if err != nil {
		return err
	}
	// Records are append-only, so no known voucher can disappear between this
	// check and the INSERT. Cross-process uniqueness is enforced by SQLite.
	if err := checkTrustDelegation(anchors, held, d); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		_, err := q.ExecContext(ctx, `INSERT INTO trust_delegations (signature, subject_key, record) VALUES (?,?,?)`, strings.ToLower(d.Signature), strings.ToLower(d.SubjectKey), string(raw))
		return sqliteTrustInsertError(err)
	})
}

func (s *SQLiteStore) ListTrustDelegations(subject string) ([]trust.ChainDelegation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM trust_delegations WHERE (? = '' OR subject_key = ?) ORDER BY signature`, strings.ToLower(subject), strings.ToLower(subject))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []trust.ChainDelegation{}
	for rows.Next() {
		var raw []byte
		var d trust.ChainDelegation
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

var _ TrustStore = (*SQLiteStore)(nil)
