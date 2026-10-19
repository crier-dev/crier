package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// An additive projection reaches already-created databases without rewriting
// their transcript schema. JSONL already retains these fields in the record.
// No reservation is stored here: the message record itself owns the key.
// There is no TTL or process-lifetime reset: retain this metadata alongside the
// transcript for messages AND task creation records. File-backed SQLite and
// PostgreSQL restore it on Load after reopen; JSONL restores it from its log.
// SQLite :memory: intentionally loses the entire session on process exit.
const messageRequestSchema = `CREATE TABLE IF NOT EXISTS chat_message_requests (
	session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
	message_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	outcomes TEXT NOT NULL,
	PRIMARY KEY (session_id, message_id)
)`

// Even an unkeyed send must retain its structured diagnostic on SQL reopen.
// Keep the metadata and transcript in the same existing transaction.
func hasMessageRequestMetadata(rec *Record) bool {
	if rec.Type != RecordMessage && rec.Type != RecordThreadReply {
		return false
	}
	if rec.IdempotencyKey != "" || rec.SenderIdentityBasis != "" {
		return true
	}
	for _, o := range rec.Outcomes {
		if o.Reason != "" || o.Code != "" || o.Retryable {
			return true
		}
	}
	return false
}

func (s *SQLStore) projectMessageRequest(ctx context.Context, rec *Record) error {
	outcomes, err := marshalMessageRequest(rec)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO chat_message_requests (session_id, message_id, idempotency_key, outcomes)
		VALUES (?, ?, ?, ?) ON CONFLICT (session_id, message_id) DO UPDATE SET
		idempotency_key = excluded.idempotency_key, outcomes = excluded.outcomes`,
		rec.SessionID, rec.MessageID, rec.IdempotencyKey, string(outcomes))
	return err
}

func (s *PostgresStore) projectMessageRequest(ctx context.Context, rec *Record) error {
	outcomes, err := marshalMessageRequest(rec)
	if err != nil {
		return err
	}
	_, err = s.requestExecutor().Exec(ctx, `INSERT INTO chat_message_requests (session_id, message_id, idempotency_key, outcomes)
		VALUES ($1, $2, $3, $4) ON CONFLICT (session_id, message_id) DO UPDATE SET
		idempotency_key = excluded.idempotency_key, outcomes = excluded.outcomes`,
		rec.SessionID, rec.MessageID, rec.IdempotencyKey, string(outcomes))
	return err
}

// Preserve the legacy array encoding for old records. New identity-bearing
// records use an envelope in the SAME metadata column: no schema migration.
func marshalMessageRequest(rec *Record) ([]byte, error) {
	if rec.SenderIdentityBasis == "" {
		return json.Marshal(rec.Outcomes)
	}
	return json.Marshal(struct {
		Outcomes            []DeliveryOutcome `json:"outcomes"`
		SenderIdentityBasis string            `json:"sender_identity_basis"`
	}{rec.Outcomes, rec.SenderIdentityBasis})
}

func restoreMessageRequest(messages []*Message, id, key, raw string) error {
	for _, m := range messages {
		if m.ID != id {
			continue
		}
		m.IdempotencyKey = key
		var outcomes []DeliveryOutcome
		if strings.HasPrefix(strings.TrimSpace(raw), "{") {
			var metadata struct {
				Outcomes            []DeliveryOutcome `json:"outcomes"`
				SenderIdentityBasis string            `json:"sender_identity_basis"`
			}
			if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
				return fmt.Errorf("load message request metadata: %w", err)
			}
			outcomes = metadata.Outcomes
			m.SenderIdentityBasis = metadata.SenderIdentityBasis
		} else if err := json.Unmarshal([]byte(raw), &outcomes); err != nil {
			return fmt.Errorf("load message request outcomes: %w", err)
		}
		// Keep the delivery projection authoritative for lifecycle changes;
		// restore only its missing refusal detail from the durable record.
		for i := range m.Outcomes {
			for _, recorded := range outcomes {
				if recorded.Target == m.Outcomes[i].Target {
					m.Outcomes[i].Reason = recorded.Reason
					// PostgreSQL timestamptz rounds to microseconds. Preserve the
					// original wire timestamp while this is still the same outcome;
					// a later leased/acked state keeps its own projection timestamp.
					if recorded.Outcome == m.Outcomes[i].Outcome && recorded.InboxEntryID == m.Outcomes[i].InboxEntryID {
						m.Outcomes[i].Code = recorded.Code
						m.Outcomes[i].Retryable = recorded.Retryable
						m.Outcomes[i].UpdatedAt = recorded.UpdatedAt
					}
				}
			}
		}
		return nil
	}
	return nil
}

func (s *SQLStore) loadMessageRequests(ctx context.Context, id string, messages []*Message) error {
	rows, err := s.query(ctx, `SELECT message_id, idempotency_key, outcomes FROM chat_message_requests WHERE session_id = ?`, id)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid, key, raw string
		if err := rows.Scan(&mid, &key, &raw); err != nil {
			return err
		}
		if err := restoreMessageRequest(messages, mid, key, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *PostgresStore) loadMessageRequests(ctx context.Context, id string, messages []*Message) error {
	rows, err := s.requestExecutor().Query(ctx, `SELECT message_id, idempotency_key, outcomes FROM chat_message_requests WHERE session_id = $1`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var mid, key, raw string
		if err := rows.Scan(&mid, &key, &raw); err != nil {
			return err
		}
		if err := restoreMessageRequest(messages, mid, key, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

// pgx's pool and transaction expose the same projection operations.
type requestPGExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *PostgresStore) requestExecutor() requestPGExecutor {
	if s.requestTx != nil {
		return s.requestTx
	}
	return s.pool
}
