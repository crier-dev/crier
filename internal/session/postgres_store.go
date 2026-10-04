package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the PostgreSQL half of the dual backend: the QUERY VIEW
// built from the ordered log (§2.1, §5.2). It is what the API serves from —
// indexed, joinable, countable.
//
// It is NOT a second writer. There are no two-way live writes (§2.2): new
// state travels record → JSONL line → projection → PostgreSQL, and this store
// implements exactly the projection end of that one direction. Project
// applies one record-version; Load assembles the reduced State back out.
//
// The schema is the §5.2 shape verbatim — chat_sessions, chat_session_members,
// chat_transcript, chat_deliveries, chat_threads, chat_context_shares. Table
// and column names are fixed by the spec "so the view and the client cannot
// invent two schemas"; this package does not add, rename or reinterpret a
// column. Any PostgreSQL-relevant extra (§5.2's `guard` jsonb) is created and
// left NULL because the spec fixes it, not because this row fills it.
type PostgresStore struct {
	pool *pgxpool.Pool

	// owned records whether this store opened the pool it uses. A borrowed
	// pool (NewPostgresStoreWithPool — the shipped server already holds one for
	// the registry) is NOT closed by Close: the caller that opened it owns it.
	owned bool
}

var _ Store = (*PostgresStore)(nil)

// SchemaStatements is the DDL for the §5.2 view, applied verbatim and
// idempotently. It is exported so a deployment can audit the exact SQL rather
// than infer it, and so a test can assert the shape exists.
var SchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS chat_sessions (
		id                TEXT PRIMARY KEY,
		namespace         TEXT NOT NULL DEFAULT '',
		kind              TEXT NOT NULL DEFAULT 'channel' CHECK (kind IN ('channel','direct')),
		title             TEXT NOT NULL DEFAULT '',
		created_by        JSONB NOT NULL,
		created_at        TIMESTAMPTZ NOT NULL,
		state             TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
		closed_at         TIMESTAMPTZ NULL,
		retention_seconds INTEGER NULL,
		group_id          TEXT NOT NULL DEFAULT '',
		visibility        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS chat_session_members (
		session_id  TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type TEXT NOT NULL CHECK (member_type IN ('agent','principal')),
		member_id   TEXT NOT NULL,
		role        TEXT NOT NULL,
		added_at    TIMESTAMPTZ NOT NULL,
		removed_at  TIMESTAMPTZ NULL,
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
		payload      JSONB NULL,
		guard        JSONB NULL,
		audience     JSONB NULL,
		created_at   TIMESTAMPTZ NOT NULL,
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
		updated_at      TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (session_id, message_id, target_agent_id)
	)`,
	`CREATE TABLE IF NOT EXISTS chat_threads (
		thread_id         TEXT PRIMARY KEY,
		session_id        TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		parent_thread_id  TEXT NULL,
		root_message_id   TEXT NOT NULL DEFAULT '',
		anchor_message_id TEXT NULL,
		created_by        JSONB NULL,
		created_at        TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_context_shares (
		session_id          TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
		member_type         TEXT NOT NULL,
		member_id           TEXT NOT NULL,
		mode                TEXT NOT NULL,
		boundary_message_id TEXT NOT NULL DEFAULT '',
		set_by              TEXT NOT NULL DEFAULT '',
		set_at              TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (session_id, member_type, member_id)
	)`,
}

// NewPostgresStore opens a pool against connString, applies the §5.2 schema
// and returns a ready store. The caller owns the returned store and must
// Close it.
func NewPostgresStore(ctx context.Context, connString string) (*PostgresStore, error) {
	if connString == "" {
		return nil, fmt.Errorf("session postgres store: %w: connection string is empty", ErrInvalidRecord)
	}
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("session postgres store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("session postgres store: ping: %w", err)
	}
	s := &PostgresStore{pool: pool, owned: true}
	if err := s.ApplySchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// NewPostgresStoreWithPool wraps an existing pool (the shipped server already
// opens one for the registry) and applies the schema. Closing the returned
// store does NOT close the borrowed pool.
func NewPostgresStoreWithPool(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("session postgres store: %w: nil pool", ErrInvalidRecord)
	}
	s := &PostgresStore{pool: pool}
	if err := s.ApplySchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ApplySchema creates the §5.2 tables and indexes when they are absent. It is
// idempotent and safe to run at every boot.
func (s *PostgresStore) ApplySchema(ctx context.Context) error {
	for _, stmt := range SchemaStatements {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("session postgres store: apply schema: %w", err)
		}
	}
	return nil
}

// Close releases the pool when this store opened it. A borrowed pool is left
// alone — the caller that opened it owns its lifetime.
func (s *PostgresStore) Close() error {
	if s.pool != nil && s.owned {
		s.pool.Close()
	}
	return nil
}

// Append projects one record-version into the view (§2.3's second step). The
// projection is keep-LAST per (session_id, seq) (§5.1): replaying a record at
// the same seq REPLACES the row, which is how the fan-out's delivery outcomes
// are written back onto the same message record after the fact (§3.2).
func (s *PostgresStore) Append(ctx context.Context, rec *Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	switch rec.Type {
	case RecordSessionCreate:
		return s.projectSessionCreate(ctx, rec)
	case RecordMemberAdd:
		return s.projectMemberAdd(ctx, rec)
	case RecordMemberContext:
		return s.projectMemberContext(ctx, rec)
	case RecordMemberRemove:
		return s.projectMemberRemove(ctx, rec)
	case RecordMessage, RecordThreadReply:
		return s.projectMessage(ctx, rec)
	case RecordThreadBranch:
		return s.projectThreadBranch(ctx, rec)
	case RecordClose:
		_, err := s.pool.Exec(ctx,
			`UPDATE chat_sessions SET state = 'closed', closed_at = $2 WHERE id = $1`,
			rec.SessionID, rec.TS.UTC())
		return err
	case RecordReopen:
		_, err := s.pool.Exec(ctx,
			`UPDATE chat_sessions SET state = 'open', closed_at = NULL WHERE id = $1`,
			rec.SessionID)
		return err
	default:
		return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, rec.Type)
	}
}

func (s *PostgresStore) projectSessionCreate(ctx context.Context, rec *Record) error {
	createdBy, err := json.Marshal(rec.CreatedBy)
	if err != nil {
		return fmt.Errorf("marshal created_by: %w", err)
	}
	kind := rec.Kind
	if kind == "" {
		kind = DefaultKind
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO chat_sessions
			(id, namespace, kind, title, created_by, created_at, state, closed_at, retention_seconds, group_id, visibility)
		VALUES ($1,$2,$3,$4,$5,$6,'open',NULL,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
			namespace         = EXCLUDED.namespace,
			kind              = EXCLUDED.kind,
			title             = EXCLUDED.title,
			created_by        = EXCLUDED.created_by,
			retention_seconds = EXCLUDED.retention_seconds,
			group_id          = EXCLUDED.group_id,
			visibility        = EXCLUDED.visibility`,
		rec.SessionID, rec.Namespace, string(kind), rec.Title, createdBy, rec.TS.UTC(),
		rec.RetentionSeconds, rec.Group, rec.Visibility)
	if err != nil {
		return fmt.Errorf("project session.create: %w", err)
	}
	return nil
}

func (s *PostgresStore) projectMemberAdd(ctx context.Context, rec *Record) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_session_members
			(session_id, member_type, member_id, role, added_at, removed_at, added_by, removed_by)
		VALUES ($1,$2,$3,$4,$5,NULL,$6,'')
		ON CONFLICT (session_id, member_type, member_id) DO UPDATE SET
			role       = EXCLUDED.role,
			added_at   = EXCLUDED.added_at,
			removed_at = NULL,
			added_by   = EXCLUDED.added_by,
			removed_by = ''`,
		rec.SessionID, string(rec.MemberType), rec.MemberID, string(rec.Role), rec.TS.UTC(), actorID(rec.Actor))
	if err != nil {
		return fmt.Errorf("project session.member.add: %w", err)
	}
	if rec.ContextShare != nil {
		return s.projectContextShare(ctx, rec, rec.ContextShare)
	}
	return nil
}

func (s *PostgresStore) projectMemberContext(ctx context.Context, rec *Record) error {
	if rec.ContextShare == nil {
		return fmt.Errorf("%w: session.member.context without context_share", ErrInvalidRecord)
	}
	return s.projectContextShare(ctx, rec, rec.ContextShare)
}

func (s *PostgresStore) projectContextShare(ctx context.Context, rec *Record, cs *ContextShare) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_context_shares
			(session_id, member_type, member_id, mode, boundary_message_id, set_by, set_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (session_id, member_type, member_id) DO UPDATE SET
			mode                = EXCLUDED.mode,
			boundary_message_id = EXCLUDED.boundary_message_id,
			set_by              = EXCLUDED.set_by,
			set_at              = EXCLUDED.set_at`,
		rec.SessionID, string(rec.MemberType), rec.MemberID, string(cs.Mode),
		cs.BoundaryMessageID, actorID(rec.Actor), rec.TS.UTC())
	if err != nil {
		return fmt.Errorf("project context share: %w", err)
	}
	return nil
}

func (s *PostgresStore) projectMemberRemove(ctx context.Context, rec *Record) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE chat_session_members
		SET removed_at = $4, removed_by = $5
		WHERE session_id = $1 AND member_type = $2 AND member_id = $3`,
		rec.SessionID, string(rec.MemberType), rec.MemberID, rec.TS.UTC(), actorID(rec.Actor))
	if err != nil {
		return fmt.Errorf("project session.member.remove: %w", err)
	}
	return nil
}

func (s *PostgresStore) projectMessage(ctx context.Context, rec *Record) error {
	var payload, audience []byte
	if len(rec.Payload) > 0 {
		payload = rec.Payload
	}
	if rec.Audience != nil {
		var err error
		audience, err = json.Marshal(rec.Audience)
		if err != nil {
			return fmt.Errorf("marshal audience: %w", err)
		}
	}
	authorType, authorID, principalID := splitAuthor(rec.Author)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chat_transcript
			(session_id, seq, message_id, thread_id, parent_id, message_kind,
			 author_type, author_id, principal_id, payload, guard, audience, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,NULL,$11::jsonb,$12)
		ON CONFLICT (session_id, seq) DO UPDATE SET
			message_id   = EXCLUDED.message_id,
			thread_id    = EXCLUDED.thread_id,
			parent_id    = EXCLUDED.parent_id,
			message_kind = EXCLUDED.message_kind,
			author_type  = EXCLUDED.author_type,
			author_id    = EXCLUDED.author_id,
			principal_id = EXCLUDED.principal_id,
			payload      = EXCLUDED.payload,
			audience     = EXCLUDED.audience,
			created_at   = EXCLUDED.created_at`,
		rec.SessionID, rec.Seq, rec.MessageID, rec.ThreadID, rec.ParentID, string(rec.MessageKind),
		authorType, authorID, principalID, payload, audience, rec.TS.UTC())
	if err != nil {
		return fmt.Errorf("project message: %w", err)
	}

	// A thread ROOT defines its thread (§4.3: a root's thread_id is its own
	// message id), so the §5.2 view gets the root row here. DO NOTHING keeps a
	// branch row that already named a parent — the branch record is
	// authoritative for the tree, this insert only materialises the node.
	if rec.Type == RecordMessage {
		createdBy := []byte("null")
		if rec.Author != nil {
			if createdBy, err = json.Marshal(rec.Author); err != nil {
				return fmt.Errorf("marshal thread created_by: %w", err)
			}
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO chat_threads
				(thread_id, session_id, parent_thread_id, root_message_id, anchor_message_id, created_by, created_at)
			VALUES ($1,$2,NULL,$3,NULL,$4,$5)
			ON CONFLICT (thread_id) DO NOTHING`,
			rec.ThreadID, rec.SessionID, rec.MessageID, createdBy, rec.TS.UTC()); err != nil {
			return fmt.Errorf("project root thread: %w", err)
		}
	}

	// The deliveries are a PROJECTION of the record's outcomes, not a queue:
	// replace this message's rows so the view matches the record exactly on
	// every re-projection (the "outcomes written back onto the same record"
	// case in §3.2).
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM chat_deliveries WHERE session_id = $1 AND message_id = $2`,
		rec.SessionID, rec.MessageID); err != nil {
		return fmt.Errorf("project message deliveries: %w", err)
	}
	for _, o := range rec.Outcomes {
		updated := o.UpdatedAt
		if updated.IsZero() {
			updated = rec.TS
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO chat_deliveries
				(session_id, message_id, target_agent_id, inbox_entry_id, outcome, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (session_id, message_id, target_agent_id) DO UPDATE SET
				inbox_entry_id = EXCLUDED.inbox_entry_id,
				outcome        = EXCLUDED.outcome,
				updated_at     = EXCLUDED.updated_at`,
			rec.SessionID, rec.MessageID, o.Target, o.InboxEntryID, string(o.Outcome), updated.UTC()); err != nil {
			return fmt.Errorf("project delivery outcome: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) projectThreadBranch(ctx context.Context, rec *Record) error {
	createdBy, err := json.Marshal(rec.Actor)
	if err != nil {
		return fmt.Errorf("marshal thread created_by: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO chat_threads
			(thread_id, session_id, parent_thread_id, root_message_id, anchor_message_id, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (thread_id) DO UPDATE SET
			session_id        = EXCLUDED.session_id,
			parent_thread_id  = EXCLUDED.parent_thread_id,
			root_message_id   = EXCLUDED.root_message_id,
			anchor_message_id = EXCLUDED.anchor_message_id,
			created_by        = EXCLUDED.created_by,
			created_at        = EXCLUDED.created_at`,
		rec.ThreadID, rec.SessionID, nullIfEmpty(rec.ParentThreadID), rec.RootMessageID,
		nullIfEmpty(rec.AnchorMessageID), createdBy, rec.TS.UTC())
	if err != nil {
		return fmt.Errorf("project session.thread.branch: %w", err)
	}
	return nil
}

// Records returns the session's message records re-derived for the log
// contract. A projection is not the log (the log is the JSONL file, §2.1), so
// this returns the message records the view can justify — the one record class
// the transcript table holds — and reports ErrSessionNotFound when the session
// itself is unknown.
func (s *PostgresStore) Records(ctx context.Context, sessionID string) ([]*Record, error) {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	recs := make([]*Record, 0, len(st.Messages))
	for _, m := range st.Messages {
		recs = append(recs, m.Record())
	}
	return recs, nil
}

// Load assembles the reduced State from the §5.2 tables — the same State
// Replay produces from the log, which is what makes the two backends
// comparable as one value.
func (s *PostgresStore) Load(ctx context.Context, sessionID string) (*State, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is empty", ErrInvalidRecord)
	}
	st := &State{}
	sess, err := s.loadSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	st.Session = sess
	if st.Members, err = s.loadMembers(ctx, sessionID); err != nil {
		return nil, err
	}
	if st.Messages, err = s.loadMessages(ctx, sessionID); err != nil {
		return nil, err
	}
	if st.Threads, err = s.loadThreads(ctx, sessionID); err != nil {
		return nil, err
	}
	if st.ContextShares, err = s.loadContextShares(ctx, sessionID); err != nil {
		return nil, err
	}
	st.normalize()
	return st, nil
}

func (s *PostgresStore) loadSession(ctx context.Context, sessionID string) (*Session, error) {
	var (
		sess          Session
		kind          string
		state         string
		createdByJSON []byte
		retention     *int
		closedAt      *time.Time
		vis           string
		grp           string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, namespace, kind, title, created_by, created_at, state, closed_at,
		       retention_seconds, group_id, visibility
		FROM chat_sessions WHERE id = $1`, sessionID).Scan(
		&sess.ID, &sess.Namespace, &kind, &sess.Title, &createdByJSON, &sess.CreatedAt, &state,
		&closedAt, &retention, &grp, &vis)
	if err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	sess.Kind = Kind(kind)
	if sess.Kind == "" {
		sess.Kind = DefaultKind
	}
	sess.State = SessionState(state)
	sess.CreatedAt = sess.CreatedAt.UTC()
	if closedAt != nil {
		utc := closedAt.UTC()
		sess.ClosedAt = &utc
	}
	sess.RetentionSeconds = retention
	sess.Group = grp
	sess.Visibility = vis
	if len(createdByJSON) > 0 && !bytes.Equal(createdByJSON, []byte("null")) {
		if err := json.Unmarshal(createdByJSON, &sess.CreatedBy); err != nil {
			return nil, fmt.Errorf("load session created_by: %w", err)
		}
	}
	return &sess, nil
}

func (s *PostgresStore) loadMembers(ctx context.Context, sessionID string) ([]*Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, member_type, member_id, role, added_at, removed_at, added_by, removed_by
		FROM chat_session_members WHERE session_id = $1
		ORDER BY member_type, member_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	defer rows.Close()
	var out []*Member
	for rows.Next() {
		m := &Member{}
		var mt, role string
		if err := rows.Scan(&m.SessionID, &mt, &m.MemberID, &role, &m.AddedAt, &m.RemovedAt, &m.AddedBy, &m.RemovedBy); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.MemberType = MemberType(mt)
		m.Role = MemberRole(role)
		m.AddedAt = m.AddedAt.UTC()
		if m.RemovedAt != nil {
			utc := m.RemovedAt.UTC()
			m.RemovedAt = &utc
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) loadMessages(ctx context.Context, sessionID string) ([]*Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, message_id, thread_id, parent_id, message_kind,
		       author_type, author_id, principal_id, COALESCE(payload, 'null'::jsonb),
		       COALESCE(audience, 'null'::jsonb), created_at
		FROM chat_transcript WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()
	var out []*Message
	byID := map[string]*Message{}
	for rows.Next() {
		m := &Message{SessionID: sessionID}
		var kind, authorType, authorID, principalID string
		var payload, audience []byte
		if err := rows.Scan(&m.Seq, &m.ID, &m.ThreadID, &m.ParentID, &kind,
			&authorType, &authorID, &principalID, &payload, &audience, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Kind = MessageKind(kind)
		m.Author = joinAuthor(authorType, authorID, principalID)
		m.CreatedAt = m.CreatedAt.UTC()
		if !bytes.Equal(payload, []byte("null")) {
			m.Payload = json.RawMessage(payload)
		}
		if !bytes.Equal(audience, []byte("null")) {
			if err := json.Unmarshal(audience, &m.Audience); err != nil {
				return nil, fmt.Errorf("load message audience: %w", err)
			}
		}
		out = append(out, m)
		byID[m.ID] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range out {
		outs, err := s.loadDeliveries(ctx, sessionID, m.ID)
		if err != nil {
			return nil, err
		}
		m.Outcomes = outs
	}
	return out, nil
}

func (s *PostgresStore) loadDeliveries(ctx context.Context, sessionID, messageID string) ([]DeliveryOutcome, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT target_agent_id, inbox_entry_id, outcome, updated_at
		FROM chat_deliveries WHERE session_id = $1 AND message_id = $2
		ORDER BY target_agent_id`, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("load deliveries: %w", err)
	}
	defer rows.Close()
	var out []DeliveryOutcome
	for rows.Next() {
		var o DeliveryOutcome
		var outcome string
		if err := rows.Scan(&o.Target, &o.InboxEntryID, &outcome, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		o.Outcome = Outcome(outcome)
		o.UpdatedAt = o.UpdatedAt.UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *PostgresStore) loadThreads(ctx context.Context, sessionID string) ([]*Thread, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT thread_id, session_id, COALESCE(parent_thread_id, ''), root_message_id,
		       COALESCE(anchor_message_id, ''), COALESCE(created_by, 'null'::jsonb), created_at
		FROM chat_threads WHERE session_id = $1 ORDER BY thread_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load threads: %w", err)
	}
	defer rows.Close()
	var out []*Thread
	for rows.Next() {
		t := &Thread{}
		var createdBy []byte
		if err := rows.Scan(&t.ID, &t.SessionID, &t.ParentThreadID, &t.RootMessageID,
			&t.AnchorMessageID, &createdBy, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		if !bytes.Equal(createdBy, []byte("null")) {
			if err := json.Unmarshal(createdBy, &t.CreatedBy); err != nil {
				return nil, fmt.Errorf("load thread created_by: %w", err)
			}
		}
		t.CreatedAt = t.CreatedAt.UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) loadContextShares(ctx context.Context, sessionID string) ([]*MemberContext, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, member_type, member_id, mode, boundary_message_id, set_by, set_at
		FROM chat_context_shares WHERE session_id = $1
		ORDER BY member_type, member_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load context shares: %w", err)
	}
	defer rows.Close()
	var out []*MemberContext
	for rows.Next() {
		mc := &MemberContext{}
		var mt, mode string
		if err := rows.Scan(&mc.SessionID, &mt, &mc.MemberID, &mode, &mc.BoundaryMessageID, &mc.SetBy, &mc.SetAt); err != nil {
			return nil, fmt.Errorf("scan context share: %w", err)
		}
		mc.MemberType = MemberType(mt)
		mc.Mode = ContextShareMode(mode)
		mc.SetAt = mc.SetAt.UTC()
		out = append(out, mc)
	}
	return out, rows.Err()
}

// splitAuthor flattens an AuthorRef into the §5.2 author_type/author_id/
// principal_id triple: who spoke (agent) and, when a human spoke as it, which
// human.
func splitAuthor(a *AuthorRef) (authorType, authorID, principalID string) {
	if a == nil {
		return "", "", ""
	}
	switch {
	case a.Agent != "":
		return "agent", a.Agent, a.Principal
	case a.AsAgent != "":
		return "principal", a.AsAgent, a.Principal
	case a.Principal != "":
		return "principal", a.Principal, a.Principal
	case a.System != "":
		return "system", a.System, ""
	}
	return "", "", ""
}

// joinAuthor is splitAuthor's inverse.
func joinAuthor(authorType, authorID, principalID string) AuthorRef {
	switch authorType {
	case "agent":
		return AuthorRef{Agent: authorID, Principal: principalID}
	case "principal":
		if principalID != "" && principalID != authorID {
			return AuthorRef{Principal: principalID, AsAgent: authorID}
		}
		return AuthorRef{Principal: authorID}
	case "system":
		return AuthorRef{System: authorID}
	}
	return AuthorRef{}
}

// nullIfEmpty maps an empty optional text column to SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isNoRows reports whether err is pgx's no-rows sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// ---------------------------------------------------------------------------
// Session CRUD + member management (§1–§2), as thin wrappers over Append +
// Load so a caller does not hand-assemble a union Record.
//
// Every record carries a monotonic per-session `seq` (§3.1). Allocation is the
// SINGLE APPENDER's and is an open question owned by CHAT-STORAGE.md §6.1 —
// this package never mints one behind the caller's back, and the PostgreSQL
// projection stores no record stream to derive it from, so the caller supplies
// it. JSONLStore.NextSeq computes the next value from the log itself.
// ---------------------------------------------------------------------------

// CreateSession appends the session.create record (§1.3: a session that is not
// in the log does not exist).
func (s *PostgresStore) CreateSession(ctx context.Context, sess *Session, seq int64) error {
	return s.Append(ctx, sess.CreateRecord(seq, sess.CreatedAt))
}

// Session returns one session object, or ErrSessionNotFound.
func (s *PostgresStore) Session(ctx context.Context, id string) (*Session, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Session, nil
}

// AddMember records a join, and — for a join to a thread in flight — the
// late-join context answer on the SAME event (§4.6 rule 1).
func (s *PostgresStore) AddMember(ctx context.Context, m *Member, seq int64, share *ContextShare) error {
	return s.Append(ctx, m.AddRecord(seq, m.AddedAt, share))
}

// SetMemberContext records a LATER change to a member's context share — a new
// record, never a rewrite of the add (§4.6 rule 3).
func (s *PostgresStore) SetMemberContext(ctx context.Context, mc *MemberContext, seq int64) error {
	return s.Append(ctx, mc.Record(seq, mc.SetAt))
}

// RemoveMember records a removal (§2.4). The member's existing deliveries are
// untouched: an inbox is per-agent and a removal must not reach into it.
func (s *PostgresStore) RemoveMember(ctx context.Context, m *Member, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, m.RemoveRecord(seq, at, actor, reason))
}

// PostMessage appends a message record — session.message for a thread root and
// session.thread.reply for a reply. Posting the SAME message again at the same
// seq after a fan-out writes the delivery outcomes back onto the record
// (§3.2).
func (s *PostgresStore) PostMessage(ctx context.Context, m *Message) error {
	return s.Append(ctx, m.Record())
}

// Branch records a deliberate branch: the only record that creates a new
// thread_id, and it issues no fan-out by itself (§4.5).
func (s *PostgresStore) Branch(ctx context.Context, t *Thread, seq int64, reason string) error {
	return s.Append(ctx, t.BranchRecord(seq, t.CreatedAt, reason))
}

// CloseSession appends a session.close record; the transcript is retained.
func (s *PostgresStore) CloseSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, CloseRecord(id, seq, at, actor, reason))
}

// ReopenSession appends a session.reopen record — an event, not a field
// mutation.
func (s *PostgresStore) ReopenSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef) error {
	return s.Append(ctx, ReopenRecord(id, seq, at, actor))
}

// Members returns the session's membership, ordered deterministically.
func (s *PostgresStore) Members(ctx context.Context, id string) ([]*Member, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Members, nil
}

// Messages returns the transcript, in seq order.
func (s *PostgresStore) Messages(ctx context.Context, id string) ([]*Message, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Messages, nil
}

// Threads returns the thread tree.
func (s *PostgresStore) Threads(ctx context.Context, id string) ([]*Thread, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Threads, nil
}

// ContextShares returns the latest context-share row per member.
func (s *PostgresStore) ContextShares(ctx context.Context, id string) ([]*MemberContext, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.ContextShares, nil
}
