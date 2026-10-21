package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/crier-dev/crier/internal/permissions"
)

// lifecycleSchema retains the original event, including full timestamp precision,
// in the same transaction as the session state. Shared by both SQL adapters.
const lifecycleSchema = `CREATE TABLE IF NOT EXISTS chat_session_lifecycle (
 session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
 seq BIGINT NOT NULL,
 record_json TEXT NOT NULL,
 PRIMARY KEY (session_id, seq)
)`

func isLifecycleRecord(rec *Record) bool {
	return rec.Type == RecordClose || rec.Type == RecordReopen
}

func lifecycleState(typ RecordType) SessionState {
	if typ == RecordClose {
		return SessionClosed
	}
	return SessionOpen
}

// isSystemEventRecord retains context decisions in the existing durable system
// event channel. Keep lifecycle predicates separate: a join is not a transition.
func isSystemEventRecord(rec *Record) bool {
	return isLifecycleRecord(rec) || ((rec.Type == RecordMemberAdd || rec.Type == RecordMemberContext) && rec.ContextShare != nil)
}

// LifecycleEvent is a system row, not a delivered message or a new thread.
type LifecycleEvent struct {
	Type                RecordType    `json:"type"`
	SessionID           string        `json:"session_id"`
	Seq                 int64         `json:"seq"`
	TS                  time.Time     `json:"ts"`
	Actor               AuthorRef     `json:"actor"`
	State               SessionState  `json:"state,omitempty"`
	SenderIdentityBasis string        `json:"sender_identity_basis,omitempty"`
	Text                string        `json:"text"`
	MemberType          MemberType    `json:"member_type,omitempty"`
	MemberID            string        `json:"member_id,omitempty"`
	ContextShare        *ContextShare `json:"context_share,omitempty"`
	Reason              string        `json:"reason,omitempty"`
}

func lifecycleEvents(records []*Record) []LifecycleEvent {
	out := make([]LifecycleEvent, 0, len(records))
	for _, rec := range records {
		actor := AuthorRef{}
		if rec.Actor != nil {
			actor = *rec.Actor
		}
		text := rec.Reason
		if text == "" {
			text = "Session " + rec.SessionID + " reopened"
			if rec.Type == RecordClose {
				text = "Session " + rec.SessionID + " marked complete"
			}
		}
		out = append(out, LifecycleEvent{Type: rec.Type, SessionID: rec.SessionID,
			Seq: rec.Seq, TS: rec.TS, Actor: actor, State: lifecycleState(rec.Type),
			SenderIdentityBasis: rec.SenderIdentityBasis, Text: text})
	}
	return out
}

// systemEvents renders recorded facts for the transcript, including the full
// decision history, and merges by the same seq as messages (never by timestamp).
func systemEvents(st *State) []LifecycleEvent {
	out := lifecycleEvents(st.Lifecycle)
	for _, rec := range st.ContextEvents {
		verb := "joined"
		if rec.Type == RecordMemberContext {
			verb = "context changed"
		}
		text := fmt.Sprintf("%s %s %s · context %s", rec.MemberType, rec.MemberID, verb, rec.ContextShare.Mode)
		if rec.ContextShare.BoundaryMessageID != "" {
			text += " · boundary message " + rec.ContextShare.BoundaryMessageID
		}
		if rec.Reason != "" {
			text += " · " + rec.Reason
		}
		actor := AuthorRef{}
		if rec.Actor != nil {
			actor = *rec.Actor
		}
		out = append(out, LifecycleEvent{Type: rec.Type, SessionID: rec.SessionID,
			Seq: rec.Seq, TS: rec.TS, Actor: actor, MemberType: rec.MemberType,
			MemberID: rec.MemberID, ContextShare: rec.ContextShare, Reason: rec.Reason, Text: text})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func parseLifecycle(raw string) (*Record, error) {
	rec, err := ParseRecord([]byte(raw))
	if err != nil {
		return nil, err
	}
	if !isSystemEventRecord(rec) {
		return nil, fmt.Errorf("%w: non-system-event record in chat_session_lifecycle", ErrInvalidRecord)
	}
	return rec, nil
}

func (s *SQLStore) projectLifecycle(ctx context.Context, rec *Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO chat_session_lifecycle (session_id, seq, record_json)
 VALUES (?, ?, ?) ON CONFLICT (session_id, seq) DO UPDATE SET record_json = excluded.record_json`,
		rec.SessionID, rec.Seq, string(raw))
	return err
}

func (s *SQLStore) loadLifecycle(ctx context.Context, id string) ([]*Record, error) {
	rows, err := s.query(ctx, `SELECT record_json FROM chat_session_lifecycle WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		rec, err := parseLifecycle(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) projectLifecycle(ctx context.Context, rec *Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.requestExecutor().Exec(ctx, `INSERT INTO chat_session_lifecycle (session_id, seq, record_json)
 VALUES ($1, $2, $3) ON CONFLICT (session_id, seq) DO UPDATE SET record_json = excluded.record_json`,
		rec.SessionID, rec.Seq, string(raw))
	return err
}

func (s *PostgresStore) loadLifecycle(ctx context.Context, id string) ([]*Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT record_json FROM chat_session_lifecycle WHERE session_id = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		rec, err := parseLifecycle(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

type sessionStateRequest struct {
	State       SessionState `json:"state"`
	Sender      string       `json:"sender,omitempty"`
	PrincipalID string       `json:"principal_id,omitempty"`
	AsAgent     string       `json:"as_agent,omitempty"`
}

// HandleSessionState serves POST /sessions/{id}/state. Authentication is the
// send ingress's decision, not an identity asserted by this handler.
func (h *Handler) HandleSessionState(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	var req sessionStateRequest
	if !decodeSendBody(w, r, &req) {
		return
	}
	if req.State != SessionOpen && req.State != SessionClosed {
		writeAPIError(w, http.StatusBadRequest, "INVALID_SESSION_STATE", `state must be "open" or "closed"`)
		return
	}
	actor, ok := resolveAuthor(nil, req.Sender, req.PrincipalID, req.AsAgent)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "AUTHOR_REQUIRED", "sender (an agent id), or principal_id (optionally with as_agent), is required: a session transition records its actor")
		return
	}
	basis, allowed := h.authorizeSender(w, r, actor)
	if !allowed {
		return
	}
	if h.opts.Permissions != nil && h.opts.Permissions.Armed() {
		res, err := h.opts.Permissions.MayDeliver(r.Context(), permissions.CheckInput{
			Sender: senderOf(actor), Action: permissions.ActionAdmin,
			Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID},
		})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "PERMISSIONS_ERROR", err.Error())
			return
		}
		if !res.Allowed {
			writeAPIError(w, http.StatusForbidden, "SESSION_STATE_FORBIDDEN", "admin on the session is required to close or reopen it")
			return
		}
	}
	// Empty request key is reserved for lifecycle transitions. Keyed sends only
	// lock nonempty keys, so no user-supplied send key can alias this lock.
	release, err := h.store.LockMessage(r.Context(), sess.ID, "")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	defer release()
	st, _, ok := h.loadScoped(w, r) // fresh state while holding the cross-handle lock
	if !ok {
		return
	}
	if st.Session.State == req.State {
		code, detail := "SESSION_ALREADY_OPEN", fmt.Sprintf("session %q is already open; cannot reopen it", sess.ID)
		if req.State == SessionClosed {
			code, detail = "SESSION_ALREADY_CLOSED", fmt.Sprintf("session %q is already closed; cannot close it again", sess.ID)
		}
		writeAPIError(w, http.StatusConflict, code, detail)
		return
	}
	seq, err := h.store.NextSeq(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	// PostgreSQL timestamptz has microsecond precision. Stamp the event at the
	// common precision so its ts and the projected closed_at stay identical.
	rec := ReopenRecord(sess.ID, seq, h.now().UTC().Truncate(time.Microsecond), actor)
	rec.Reason = "Session " + sess.ID + " reopened"
	if req.State == SessionClosed {
		rec = CloseRecord(sess.ID, seq, rec.TS, actor, "Session "+sess.ID+" marked complete")
	}
	rec.SenderIdentityBasis = basis
	if err := h.store.Append(r.Context(), rec); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	st, err = h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.viewOf(st))
}
