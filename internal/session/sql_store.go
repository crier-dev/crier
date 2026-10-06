package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Dialect is the engine-specific half of the shared SQL session store
// (CR-CHAT-006) — the ONLY thing that differs between SQLite and PostgreSQL.
// Everything else (the projection, the keep-LAST upsert, the State assembly,
// the CRUD surface) lives once in SQLStore, which is what makes supporting a
// second engine an adapter rather than a second store.
type Dialect interface {
	// Name is the engine's backend spelling (the CR_SESSION_BACKEND value).
	Name() string
	// Rebind converts the store's canonical `?` placeholders into the
	// engine's own, so ONE query text serves both engines.
	Rebind(query string) string
	// InitStatements are executed once per open (engine pragmas / session
	// setup). They must be statements that do not return rows.
	InitStatements() []string
	// SchemaStatements is the DDL for the view, applied idempotently.
	SchemaStatements() []string
}

// SQLStore is the shared SQL repository half of the dual backend (CR-CHAT-006):
// ONE implementation of the session view running on any engine that speaks
// database/sql, parameterised by a Dialect that supplies only the genuinely
// engine-specific parts (placeholder syntax, DDL, connection setup).
//
// SQLite and PostgreSQL are therefore two ADAPTERS over this one store, not two
// copies of it:
//
//   - NewSQLiteStore — the pure-Go modernc.org/sqlite driver: no CGO, no
//     server, no service to run, which is the "a user does not have to run
//     PostgreSQL" path;
//   - NewPostgresSQLStore — the pgx database/sql adapter, the same repository
//     over a server-backed engine for multi-user / scale deployments.
//
// The pre-existing pgxpool-backed PostgresStore is untouched: it remains the
// projection the shipped registry pool path uses, and this type is the
// engine-agnostic repository that makes a second ENGINE an adapter. Both target
// the same §5.2 view names, so a deployment selects ONE engine per database.
//
// The write direction is the spec's one direction (§2.2/§2.3):
// record → JSONL line → projection → view. Append projects one record-version
// and is keep-LAST per (session_id, seq), so replaying a record at the same seq
// REPLACES the row (how fan-out outcomes are written back onto a message) and
// re-appending a byte-identical record is a no-op (§4.1). No component ever
// writes this store and then back-fills a log from it.
//
// Timestamps are stored as canonical RFC 3339 UTC TEXT on BOTH engines. That is
// deliberate and load-bearing: it is what lets one query text and one scan path
// serve both engines, and the transcript's ordering authority is `seq`, never
// `ts` (§3.1) — so no engine's native timestamp type is relied upon here.
type SQLStore struct {
	db      *sql.DB
	dialect Dialect
	owned   bool
}

var _ Store = (*SQLStore)(nil)

// newSQLStore wraps an open handle with the shared repository. owned says
// whether Close should close the handle (a borrowed handle is the caller's).
func newSQLStore(db *sql.DB, d Dialect, owned bool) *SQLStore {
	return &SQLStore{db: db, dialect: d, owned: owned}
}

// Backend names the engine this store runs on ("sqlite" or "postgres").
func (s *SQLStore) Backend() string { return s.dialect.Name() }

// DB exposes the underlying handle. It exists so an engine-specific read (a
// schema probe, a backup, a bundle export) needs no engine branch in this
// store; nothing in the projection path uses it.
func (s *SQLStore) DB() *sql.DB { return s.db }

// Close releases the database handle when this store opened it.
func (s *SQLStore) Close() error {
	if s.db != nil && s.owned {
		return s.db.Close()
	}
	return nil
}

// ApplySchema runs the engine's session setup and creates the view tables and
// indexes when they are absent. It is idempotent and safe to run at every boot.
func (s *SQLStore) ApplySchema(ctx context.Context) error {
	for _, stmt := range s.dialect.InitStatements() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("session %s store: init: %w", s.dialect.Name(), err)
		}
	}
	for _, stmt := range s.dialect.SchemaStatements() {
		if _, err := s.db.ExecContext(ctx, s.dialect.Rebind(stmt)); err != nil {
			return fmt.Errorf("session %s store: apply schema: %w", s.dialect.Name(), err)
		}
	}
	return nil
}

// exec / query / queryRow are the ONE place the canonical `?` query text meets
// the engine's placeholder syntax.
func (s *SQLStore) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *SQLStore) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *SQLStore) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.dialect.Rebind(query), args...)
}

// Append projects one record-version into the view — §2.3's second step. It is
// keep-LAST per (session_id, seq) (§5.1), so a later line at the same seq wins
// and an identical replay is a no-op.
func (s *SQLStore) Append(ctx context.Context, rec *Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	if err := s.project(ctx, rec); err != nil {
		return err
	}
	return s.recordSeq(ctx, rec)
}

// project applies one record-version to the view tables — §2.3's second step.
func (s *SQLStore) project(ctx context.Context, rec *Record) error {
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
		_, err := s.exec(ctx,
			`UPDATE chat_sessions SET state = 'closed', closed_at = ? WHERE id = ?`,
			encTime(rec.TS), rec.SessionID)
		return err
	case RecordReopen:
		_, err := s.exec(ctx,
			`UPDATE chat_sessions SET state = 'open', closed_at = NULL WHERE id = ?`,
			rec.SessionID)
		return err
	default:
		return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, rec.Type)
	}
}

// recordSeq advances the session's seq allocator (chat_seq) to at least this
// record's seq. It is the ONE place the SQL view remembers how far a session's
// record stream has run, which is what makes NextSeq exact on an engine whose
// projection tables carry seq only for message rows (CR-CHAT-019).
func (s *SQLStore) recordSeq(ctx context.Context, rec *Record) error {
	_, err := s.exec(ctx, `
		INSERT INTO chat_seq (session_id, last_seq) VALUES (?, ?)
		ON CONFLICT (session_id) DO UPDATE SET last_seq =
			CASE WHEN excluded.last_seq > chat_seq.last_seq THEN excluded.last_seq ELSE chat_seq.last_seq END`,
		rec.SessionID, rec.Seq)
	if err != nil {
		return fmt.Errorf("project chat_seq: %w", err)
	}
	return nil
}

// NextSeq returns the seq the next record for a session should carry: one past
// the highest seq projected for it, or 1 when the session has no records yet.
// It reads the chat_seq allocator, so it agrees with the JSONL log's NextSeq —
// one ordering authority per backend, both derived from the record stream. A
// session that is not in the view is ErrSessionNotFound, matching Load.
func (s *SQLStore) NextSeq(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("%w: session id is empty", ErrInvalidRecord)
	}
	var last int64
	err := s.queryRow(ctx, `SELECT last_seq FROM chat_seq WHERE session_id = ?`, sessionID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		// No seq row yet. Distinguish "the session exists but has no record
		// stream" (a session must have a create record to exist, §1.3) from
		// "no such session": the latter is reported, never a fresh seq 1 for
		// a session nobody opened.
		if _, err := s.loadSession(ctx, sessionID); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("session %s store: next seq: %w", s.dialect.Name(), err)
	}
	return last + 1, nil
}

// Sessions lists the session ids in the view, sorted — the SQL half of the
// enumeration GET /sessions is a view over (CR-CHAT-019). It reads the same
// chat_sessions rows every other read serves from, so the list cannot disagree
// with them.
func (s *SQLStore) Sessions(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT id FROM chat_sessions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("session %s store: list sessions: %w", s.dialect.Name(), err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("session %s store: scan session id: %w", s.dialect.Name(), err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session %s store: list sessions: %w", s.dialect.Name(), err)
	}
	return ids, nil
}

func (s *SQLStore) projectSessionCreate(ctx context.Context, rec *Record) error {
	createdBy, err := json.Marshal(rec.CreatedBy)
	if err != nil {
		return fmt.Errorf("marshal created_by: %w", err)
	}
	kind := rec.Kind
	if kind == "" {
		kind = DefaultKind
	}
	_, err = s.exec(ctx, `
		INSERT INTO chat_sessions
			(id, namespace, kind, title, created_by, created_at, state, closed_at, retention_seconds, group_id, visibility)
		VALUES (?,?,?,?,?,?,'open',NULL,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			namespace         = excluded.namespace,
			kind              = excluded.kind,
			title             = excluded.title,
			created_by        = excluded.created_by,
			retention_seconds = excluded.retention_seconds,
			group_id          = excluded.group_id,
			visibility        = excluded.visibility`,
		rec.SessionID, rec.Namespace, string(kind), rec.Title, string(createdBy), encTime(rec.TS),
		nullInt(rec.RetentionSeconds), rec.Group, rec.Visibility)
	if err != nil {
		return fmt.Errorf("project session.create: %w", err)
	}
	return nil
}

func (s *SQLStore) projectMemberAdd(ctx context.Context, rec *Record) error {
	_, err := s.exec(ctx, `
		INSERT INTO chat_session_members
			(session_id, member_type, member_id, role, added_at, removed_at, added_by, removed_by)
		VALUES (?,?,?,?,?,NULL,?,'')
		ON CONFLICT (session_id, member_type, member_id) DO UPDATE SET
			role       = excluded.role,
			added_at   = excluded.added_at,
			removed_at = NULL,
			added_by   = excluded.added_by,
			removed_by = ''`,
		rec.SessionID, string(rec.MemberType), rec.MemberID, string(rec.Role), encTime(rec.TS), actorID(rec.Actor))
	if err != nil {
		return fmt.Errorf("project session.member.add: %w", err)
	}
	if rec.ContextShare != nil {
		return s.projectContextShare(ctx, rec, rec.ContextShare)
	}
	return nil
}

func (s *SQLStore) projectMemberContext(ctx context.Context, rec *Record) error {
	if rec.ContextShare == nil {
		return fmt.Errorf("%w: session.member.context without context_share", ErrInvalidRecord)
	}
	return s.projectContextShare(ctx, rec, rec.ContextShare)
}

func (s *SQLStore) projectContextShare(ctx context.Context, rec *Record, cs *ContextShare) error {
	_, err := s.exec(ctx, `
		INSERT INTO chat_context_shares
			(session_id, member_type, member_id, mode, boundary_message_id, set_by, set_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (session_id, member_type, member_id) DO UPDATE SET
			mode                = excluded.mode,
			boundary_message_id = excluded.boundary_message_id,
			set_by              = excluded.set_by,
			set_at              = excluded.set_at`,
		rec.SessionID, string(rec.MemberType), rec.MemberID, string(cs.Mode),
		cs.BoundaryMessageID, actorID(rec.Actor), encTime(rec.TS))
	if err != nil {
		return fmt.Errorf("project context share: %w", err)
	}
	return nil
}

func (s *SQLStore) projectMemberRemove(ctx context.Context, rec *Record) error {
	_, err := s.exec(ctx, `
		UPDATE chat_session_members
		SET removed_at = ?, removed_by = ?
		WHERE session_id = ? AND member_type = ? AND member_id = ?`,
		encTime(rec.TS), actorID(rec.Actor), rec.SessionID, string(rec.MemberType), rec.MemberID)
	if err != nil {
		return fmt.Errorf("project session.member.remove: %w", err)
	}
	return nil
}

func (s *SQLStore) projectMessage(ctx context.Context, rec *Record) error {
	var audience any
	if rec.Audience != nil {
		b, err := json.Marshal(rec.Audience)
		if err != nil {
			return fmt.Errorf("marshal audience: %w", err)
		}
		audience = string(b)
	}
	authorType, authorID, principalID := splitAuthor(rec.Author)
	_, err := s.exec(ctx, `
		INSERT INTO chat_transcript
			(session_id, seq, message_id, thread_id, parent_id, message_kind,
			 author_type, author_id, principal_id, payload, guard, audience, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)
		ON CONFLICT (session_id, seq) DO UPDATE SET
			message_id   = excluded.message_id,
			thread_id    = excluded.thread_id,
			parent_id    = excluded.parent_id,
			message_kind = excluded.message_kind,
			author_type  = excluded.author_type,
			author_id    = excluded.author_id,
			principal_id = excluded.principal_id,
			payload      = excluded.payload,
			audience     = excluded.audience,
			created_at   = excluded.created_at`,
		rec.SessionID, rec.Seq, rec.MessageID, rec.ThreadID, rec.ParentID, string(rec.MessageKind),
		authorType, authorID, principalID, encJSON(rec.Payload), audience, encTime(rec.TS))
	if err != nil {
		return fmt.Errorf("project message: %w", err)
	}

	// A thread ROOT defines its thread (§4.3: a root's thread_id is its own
	// message id), so the view gets the root row here. DO NOTHING keeps a
	// branch row that already named a parent — the branch record is
	// authoritative for the tree, this insert only materialises the node.
	if rec.Type == RecordMessage {
		var createdBy any
		if rec.Author != nil {
			b, err := json.Marshal(rec.Author)
			if err != nil {
				return fmt.Errorf("marshal thread created_by: %w", err)
			}
			createdBy = string(b)
		}
		if _, err := s.exec(ctx, `
			INSERT INTO chat_threads
				(thread_id, session_id, parent_thread_id, root_message_id, anchor_message_id, created_by, created_at)
			VALUES (?,?,NULL,?,NULL,?,?)
			ON CONFLICT (thread_id) DO NOTHING`,
			rec.ThreadID, rec.SessionID, rec.MessageID, createdBy, encTime(rec.TS)); err != nil {
			return fmt.Errorf("project root thread: %w", err)
		}
	}

	// The deliveries are a PROJECTION of the record's outcomes, not a queue:
	// replace this message's rows so the view matches the record exactly on
	// every re-projection (the "outcomes written back onto the same record"
	// case, §3.2).
	if _, err := s.exec(ctx,
		`DELETE FROM chat_deliveries WHERE session_id = ? AND message_id = ?`,
		rec.SessionID, rec.MessageID); err != nil {
		return fmt.Errorf("project message deliveries: %w", err)
	}
	for _, o := range rec.Outcomes {
		updated := o.UpdatedAt
		if updated.IsZero() {
			updated = rec.TS
		}
		if _, err := s.exec(ctx, `
			INSERT INTO chat_deliveries
				(session_id, message_id, target_agent_id, inbox_entry_id, outcome, updated_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT (session_id, message_id, target_agent_id) DO UPDATE SET
				inbox_entry_id = excluded.inbox_entry_id,
				outcome        = excluded.outcome,
				updated_at     = excluded.updated_at`,
			rec.SessionID, rec.MessageID, o.Target, o.InboxEntryID, string(o.Outcome), encTime(updated)); err != nil {
			return fmt.Errorf("project delivery outcome: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) projectThreadBranch(ctx context.Context, rec *Record) error {
	var createdBy any
	if rec.Actor != nil {
		b, err := json.Marshal(rec.Actor)
		if err != nil {
			return fmt.Errorf("marshal thread created_by: %w", err)
		}
		createdBy = string(b)
	}
	_, err := s.exec(ctx, `
		INSERT INTO chat_threads
			(thread_id, session_id, parent_thread_id, root_message_id, anchor_message_id, created_by, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (thread_id) DO UPDATE SET
			session_id        = excluded.session_id,
			parent_thread_id  = excluded.parent_thread_id,
			root_message_id   = excluded.root_message_id,
			anchor_message_id = excluded.anchor_message_id,
			created_by        = excluded.created_by,
			created_at        = excluded.created_at`,
		rec.ThreadID, rec.SessionID, nullIfEmpty(rec.ParentThreadID), rec.RootMessageID,
		nullIfEmpty(rec.AnchorMessageID), createdBy, encTime(rec.TS))
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
func (s *SQLStore) Records(ctx context.Context, sessionID string) ([]*Record, error) {
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

// Load assembles the reduced State from the view tables — the same State
// Replay produces from the log, which is what makes the two backends comparable
// as one value.
func (s *SQLStore) Load(ctx context.Context, sessionID string) (*State, error) {
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
	// A transcript read from the view is completed the same way the log's is:
	// a row that carries no thread key — thread_id '' from a generation before
	// the field was stored (CR-FEAT-004), or a dump made by one — gets its key
	// DERIVED from the transcript, exactly as Replay derives it from the log
	// (§4.3). One shared pass keeps the two projections of the same facts ONE
	// value.
	st.resolveThreads()
	st.normalize()
	return st, nil
}

func (s *SQLStore) loadSession(ctx context.Context, sessionID string) (*Session, error) {
	var (
		sess            Session
		kind, state     string
		createdBy       string
		createdAt       string
		closedAt        sql.NullString
		retention       sql.NullInt64
		grp, visibility string
	)
	err := s.queryRow(ctx, `
		SELECT id, namespace, kind, title, created_by, created_at, state, closed_at,
		       retention_seconds, group_id, visibility
		FROM chat_sessions WHERE id = ?`, sessionID).Scan(
		&sess.ID, &sess.Namespace, &kind, &sess.Title, &createdBy, &createdAt, &state,
		&closedAt, &retention, &grp, &visibility)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	sess.Kind = Kind(kind)
	if sess.Kind == "" {
		sess.Kind = DefaultKind
	}
	sess.State = SessionState(state)
	sess.Group = grp
	sess.Visibility = visibility
	if sess.CreatedAt, err = decTime(createdAt); err != nil {
		return nil, err
	}
	if sess.ClosedAt, err = decTimePtr(closedAt); err != nil {
		return nil, err
	}
	if retention.Valid {
		v := int(retention.Int64)
		sess.RetentionSeconds = &v
	}
	if createdBy != "" && createdBy != "null" {
		if err := json.Unmarshal([]byte(createdBy), &sess.CreatedBy); err != nil {
			return nil, fmt.Errorf("load session created_by: %w", err)
		}
	}
	return &sess, nil
}

func (s *SQLStore) loadMembers(ctx context.Context, sessionID string) ([]*Member, error) {
	rows, err := s.query(ctx, `
		SELECT session_id, member_type, member_id, role, added_at, removed_at, added_by, removed_by
		FROM chat_session_members WHERE session_id = ? ORDER BY member_type, member_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Member
	for rows.Next() {
		m := &Member{}
		var mt, role, addedAt string
		var removedAt sql.NullString
		if err := rows.Scan(&m.SessionID, &mt, &m.MemberID, &role, &addedAt, &removedAt, &m.AddedBy, &m.RemovedBy); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.MemberType = MemberType(mt)
		m.Role = MemberRole(role)
		if m.AddedAt, err = decTime(addedAt); err != nil {
			return nil, err
		}
		if m.RemovedAt, err = decTimePtr(removedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SQLStore) loadMessages(ctx context.Context, sessionID string) ([]*Message, error) {
	rows, err := s.query(ctx, `
		SELECT seq, message_id, thread_id, parent_id, message_kind,
		       author_type, author_id, principal_id, payload, audience, created_at
		FROM chat_transcript WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Message
	for rows.Next() {
		m := &Message{SessionID: sessionID}
		var kind, authorType, authorID, principalID, createdAt string
		var payload, audience sql.NullString
		if err := rows.Scan(&m.Seq, &m.ID, &m.ThreadID, &m.ParentID, &kind,
			&authorType, &authorID, &principalID, &payload, &audience, &createdAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Kind = MessageKind(kind)
		m.Author = joinAuthor(authorType, authorID, principalID)
		if m.CreatedAt, err = decTime(createdAt); err != nil {
			return nil, err
		}
		if payload.Valid && payload.String != "" && payload.String != "null" {
			m.Payload = json.RawMessage(payload.String)
		}
		if audience.Valid && audience.String != "" && audience.String != "null" {
			if err := json.Unmarshal([]byte(audience.String), &m.Audience); err != nil {
				return nil, fmt.Errorf("load message audience: %w", err)
			}
		}
		out = append(out, m)
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

func (s *SQLStore) loadDeliveries(ctx context.Context, sessionID, messageID string) ([]DeliveryOutcome, error) {
	rows, err := s.query(ctx, `
		SELECT target_agent_id, inbox_entry_id, outcome, updated_at
		FROM chat_deliveries WHERE session_id = ? AND message_id = ?
		ORDER BY target_agent_id`, sessionID, messageID)
	if err != nil {
		return nil, fmt.Errorf("load deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryOutcome
	for rows.Next() {
		var o DeliveryOutcome
		var outcome, updatedAt string
		if err := rows.Scan(&o.Target, &o.InboxEntryID, &outcome, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		o.Outcome = Outcome(outcome)
		if o.UpdatedAt, err = decTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLStore) loadThreads(ctx context.Context, sessionID string) ([]*Thread, error) {
	rows, err := s.query(ctx, `
		SELECT thread_id, session_id, parent_thread_id, root_message_id,
		       anchor_message_id, created_by, created_at
		FROM chat_threads WHERE session_id = ? ORDER BY thread_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load threads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Thread
	for rows.Next() {
		t := &Thread{}
		var parent, anchor, createdBy, createdAt sql.NullString
		if err := rows.Scan(&t.ID, &t.SessionID, &parent, &t.RootMessageID, &anchor, &createdBy, &createdAt); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		t.ParentThreadID = parent.String
		t.AnchorMessageID = anchor.String
		if createdBy.Valid && createdBy.String != "" && createdBy.String != "null" {
			if err := json.Unmarshal([]byte(createdBy.String), &t.CreatedBy); err != nil {
				return nil, fmt.Errorf("load thread created_by: %w", err)
			}
		}
		if t.CreatedAt, err = decTime(createdAt.String); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLStore) loadContextShares(ctx context.Context, sessionID string) ([]*MemberContext, error) {
	rows, err := s.query(ctx, `
		SELECT session_id, member_type, member_id, mode, boundary_message_id, set_by, set_at
		FROM chat_context_shares WHERE session_id = ?
		ORDER BY member_type, member_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load context shares: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*MemberContext
	for rows.Next() {
		mc := &MemberContext{}
		var mt, mode, setAt string
		if err := rows.Scan(&mc.SessionID, &mt, &mc.MemberID, &mode, &mc.BoundaryMessageID, &mc.SetBy, &setAt); err != nil {
			return nil, fmt.Errorf("scan context share: %w", err)
		}
		mc.MemberType = MemberType(mt)
		mc.Mode = ContextShareMode(mode)
		if mc.SetAt, err = decTime(setAt); err != nil {
			return nil, err
		}
		out = append(out, mc)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Session CRUD + member management: the same method set JSONLStore and
// PostgresStore expose, so a caller drives any backend identically (§1–§2).
//
// Every record carries a monotonic per-session `seq` (§3.1). Allocation is the
// SINGLE APPENDER's (CHAT-STORAGE.md §7.2 open question 2) and this store never
// mints one behind the caller's back: the log is the allocator
// (JSONLStore.NextSeq), and the caller supplies the value.
// ---------------------------------------------------------------------------

// CreateSession appends the session.create record (§1.3: a session that is not
// in the log does not exist).
func (s *SQLStore) CreateSession(ctx context.Context, sess *Session, seq int64) error {
	return s.Append(ctx, sess.CreateRecord(seq, sess.CreatedAt))
}

// Session returns one session object, or ErrSessionNotFound.
func (s *SQLStore) Session(ctx context.Context, id string) (*Session, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Session, nil
}

// AddMember records a join, and — for a join to a thread in flight — the
// late-join context answer on the SAME event (§4.6 rule 1).
func (s *SQLStore) AddMember(ctx context.Context, m *Member, seq int64, share *ContextShare) error {
	return s.Append(ctx, m.AddRecord(seq, m.AddedAt, share))
}

// SetMemberContext records a LATER change to a member's context share — a new
// record, never a rewrite of the add (§4.6 rule 3).
func (s *SQLStore) SetMemberContext(ctx context.Context, mc *MemberContext, seq int64) error {
	return s.Append(ctx, mc.Record(seq, mc.SetAt))
}

// RemoveMember records a removal (§2.4).
func (s *SQLStore) RemoveMember(ctx context.Context, m *Member, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, m.RemoveRecord(seq, at, actor, reason))
}

// PostMessage appends a message record — session.message for a thread root and
// session.thread.reply for a reply. Posting the SAME message again at the same
// seq after a fan-out writes the delivery outcomes back onto the record (§3.2).
func (s *SQLStore) PostMessage(ctx context.Context, m *Message) error {
	return s.Append(ctx, m.Record())
}

// Branch records a deliberate branch: the only record that creates a new
// thread_id, and it issues no fan-out by itself (§4.5).
func (s *SQLStore) Branch(ctx context.Context, t *Thread, seq int64, reason string) error {
	return s.Append(ctx, t.BranchRecord(seq, t.CreatedAt, reason))
}

// CloseSession appends a session.close record; the transcript is retained.
func (s *SQLStore) CloseSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, CloseRecord(id, seq, at, actor, reason))
}

// ReopenSession appends a session.reopen record — an event, not a field
// mutation.
func (s *SQLStore) ReopenSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef) error {
	return s.Append(ctx, ReopenRecord(id, seq, at, actor))
}

// Members returns the session's membership, ordered deterministically.
func (s *SQLStore) Members(ctx context.Context, id string) ([]*Member, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Members, nil
}

// Messages returns the transcript, in seq order.
func (s *SQLStore) Messages(ctx context.Context, id string) ([]*Message, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Messages, nil
}

// Threads returns the thread tree.
func (s *SQLStore) Threads(ctx context.Context, id string) ([]*Thread, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Threads, nil
}

// ContextShares returns the latest context-share row per member.
func (s *SQLStore) ContextShares(ctx context.Context, id string) ([]*MemberContext, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.ContextShares, nil
}

// ---------------------------------------------------------------------------
// The dialect-neutral value codec: one encoding for both engines.
// ---------------------------------------------------------------------------

// encTime renders a timestamp as the canonical RFC 3339 UTC text both engines
// store (§3.1: the ordering authority is seq, not ts).
func encTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// decTime parses the stored text back. Empty maps to the zero time.
func decTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// decTimePtr is decTime for a nullable column.
func decTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := decTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// encJSON stores a JSON blob as text, mapping an absent/`null` body to SQL NULL
// so a message with no payload reads back with none.
func encJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

// nullInt maps an optional integer to SQL NULL when unset.
func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
