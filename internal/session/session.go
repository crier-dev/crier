// Package session implements the crier session data model — the session
// object, its explicit membership, its ordered transcript and the thread tree
// — per specs/CHAT-SESSIONS.md (CR-CHAT-002 + CR-CHAT-005). The storage
// shapes are fixed by that spec's §5: the JSONL ordered append log (§5.1) and
// the PostgreSQL query view (§5.2).
//
// Two load-bearing decisions this package encodes:
//
//   - D1: a session is an AGGREGATE over the durable inbox deliveries that
//     already exist. It is not a transport, not a second write path and not a
//     parallel mailbox; a message travels through the shipped
//     POST /agents/{id}/inbox machinery (see fanout.go).
//   - D11 (the depth rule, §4.5): a reply STAYS IN THREAD. parent_id is reply
//     ATTRIBUTION, never depth; a level is created only by a deliberate
//     branch. thread_id never changes on a reply.
//
// The package is self-contained: it does not extend or reinterpret
// internal/registry, it only consumes registry.Store for fan-out.
package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind is the session's SHAPE (§1.2, CR-CHAT-013). A channel (a
// project-scoped top-level room) and a direct message (a 1:1) are the SAME
// object with different defaults — one membership model, one transcript, one
// fan-out rule. A record with no kind reads as channel, so a record written
// before the field existed is not a third shape.
type Kind string

const (
	KindChannel Kind = "channel"
	KindDirect  Kind = "direct"
)

// DefaultKind is the shape a session record with no kind reads as (§1.2).
const DefaultKind = KindChannel

// SessionState is the lifecycle vocabulary (§1.3): open accepts records,
// closed admits no new message, reply or membership change. Closed does not
// delete — the transcript is retained. There is no deleted state.
type SessionState string

const (
	SessionOpen   SessionState = "open"
	SessionClosed SessionState = "closed"
)

// MemberType distinguishes the two kinds of participant (§2.1): a Principal
// (a human) and an Agent.
type MemberType string

const (
	MemberAgent     MemberType = "agent"
	MemberPrincipal MemberType = "principal"
)

// MemberRole is the per-member role. Every member has exactly ONE role and
// membership is a recorded event, not a derived property (§2.1). The role
// VOCABULARY is owned by specs/CHAT-PERMISSIONS.md (CR-CHAT-003); the set
// below is the minimum this data model carries and is stored as data, so a
// permissions vocabulary can grow without a schema change.
type MemberRole string

const (
	RoleOwner    MemberRole = "owner"
	RoleMember   MemberRole = "member"
	RoleObserver MemberRole = "observer"
)

// MessageKind is the three-way structural distinction of §4.4 / D12. A tag is
// ADDRESSING, not an action: only `task` may create work. The kinds are data,
// not inference — a reader can tell an addressed message from a task without
// reading its text. This field is NOT the shipped envelope `kind`
// (message | configure | configure_ack) and must never be conflated with it.
type MessageKind string

const (
	MessagePlain     MessageKind = "plain"
	MessageAddressed MessageKind = "addressed"
	MessageTask      MessageKind = "task"
)

// Outcome is the per-target fan-out outcome recorded on a message record
// (§3.2). A refused outcome is SHOWN, never swallowed.
type Outcome string

const (
	OutcomeDelivered Outcome = "delivered"
	OutcomeLeased    Outcome = "leased"
	OutcomeAcked     Outcome = "acked"
	OutcomeExpired   Outcome = "expired"
	OutcomeRefused   Outcome = "refused"
)

// ContextShareMode is the late-join context decision (§4.6, D10). `summary`
// is the default and is a GENERATED INDEX, not a replacement for the record.
type ContextShareMode string

const (
	ShareNone    ContextShareMode = "none"
	ShareSummary ContextShareMode = "summary"
	ShareFull    ContextShareMode = "full"
	ShareSince   ContextShareMode = "since"
)

// AudienceRule names which rule produced a message's audience, so the record
// is auditable and a later reader does not re-derive it from membership that
// has since changed (§4.2).
type AudienceRule string

const (
	AudienceSession      AudienceRule = "session"
	AudienceReplyDefault AudienceRule = "reply-default"
	AudienceExplicit     AudienceRule = "explicit"
)

// AudienceTargetKind is an address kind (§2.1, §3.4 rule 7). `agent` and
// `group` fan out; `capability` is a SELECTOR, not a broadcast, and is never
// fanned out (D8).
type AudienceTargetKind string

const (
	TargetAgent      AudienceTargetKind = "agent"
	TargetGroup      AudienceTargetKind = "group"
	TargetCapability AudienceTargetKind = "capability"
	TargetPrincipal  AudienceTargetKind = "principal"
)

// AuthorRef names who produced a record (§5.1 `author` / `actor` /
// `created_by`). An agent's own write is {Agent}; a human through a binding is
// {Principal, AsAgent}; a system actor is {System}.
type AuthorRef struct {
	Agent     string `json:"agent,omitempty"`
	Principal string `json:"principal,omitempty"`
	AsAgent   string `json:"as_agent,omitempty"`
	System    string `json:"system,omitempty"`
}

// AgentID returns the agent identity that acted: the explicit agent, or the
// agent a principal spoke as. Empty for a system-only author.
func (a AuthorRef) AgentID() string {
	if a.Agent != "" {
		return a.Agent
	}
	return a.AsAgent
}

// IsZero reports whether the reference names nobody.
func (a AuthorRef) IsZero() bool {
	return a == AuthorRef{}
}

// Session is the session object (§1.2). The transcript and membership are NOT
// fields on it: they are ordered event sequences owned by the append log,
// because a mutable list field on a room record is exactly the hidden state
// that would make "reconstructable from the transcript alone" unprovable.
type Session struct {
	ID        string       `json:"id"`
	Namespace string       `json:"namespace"`
	Kind      Kind         `json:"kind"`
	Title     string       `json:"title,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	CreatedBy AuthorRef    `json:"created_by"`
	State     SessionState `json:"state"`
	ClosedAt  *time.Time   `json:"closed_at,omitempty"`
	// RetentionSeconds is the session's message-lifetime default. Bounded by
	// the realm's retention: a session may declare a SHORTER lifetime, never
	// a longer one (§3.5). nil = inherit the namespace policy.
	RetentionSeconds *int `json:"retention_seconds,omitempty"`
	// Group and Visibility are reserved, NOT BUILT fields (§1.2, CR-CHAT-010 /
	// CR-CHAT-003). They ride the record so adding a team is data, not schema
	// surgery.
	Group      string `json:"group,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

// Member is one participant with exactly one role (§2.1, §2.3). Membership is
// an append-only event sequence: AddedAt/RemovedAt are a PROJECTION of the
// add/remove events, and membership at time T is
// added_at <= T AND (removed_at IS NULL OR removed_at > T).
type Member struct {
	SessionID  string     `json:"session_id"`
	MemberType MemberType `json:"member_type"`
	MemberID   string     `json:"member_id"`
	Role       MemberRole `json:"role"`
	AddedAt    time.Time  `json:"added_at"`
	RemovedAt  *time.Time `json:"removed_at,omitempty"`
	AddedBy    string     `json:"added_by,omitempty"`
	RemovedBy  string     `json:"removed_by,omitempty"`
}

// ActiveAt reports whether the member is present at time t (§2.3).
func (m *Member) ActiveAt(t time.Time) bool {
	if m.AddedAt.After(t) {
		return false
	}
	return m.RemovedAt == nil || m.RemovedAt.After(t)
}

// ContextShare is the late-join answer (§4.6, D10): the mode plus the
// boundary message id the mode names. It is recorded on the member-add event,
// or on the later session.member.context event — never as a rewrite.
type ContextShare struct {
	Mode ContextShareMode `json:"mode"`
	// BoundaryMessageID is present for `since <message-id>`; empty otherwise.
	BoundaryMessageID string `json:"boundary_message_id,omitempty"`
}

// MemberContext is the latest-state context-share row for one member
// (§5.2 chat_context_shares). The EVENTS remain the record of how it changed.
type MemberContext struct {
	SessionID         string           `json:"session_id"`
	MemberType        MemberType       `json:"member_type"`
	MemberID          string           `json:"member_id"`
	Mode              ContextShareMode `json:"mode"`
	BoundaryMessageID string           `json:"boundary_message_id,omitempty"`
	SetBy             string           `json:"set_by,omitempty"`
	SetAt             time.Time        `json:"set_at"`
}

// AudienceTarget is one resolved address (§4.2). The RESOLVED tag is stored,
// not the typed one, so a transcript can answer "who was this sent to" years
// later when a group has been renamed.
type AudienceTarget struct {
	Kind AudienceTargetKind `json:"kind"`
	ID   string             `json:"id"`
}

// Audience is the resolved delivery set recorded on a message record (§4.2),
// plus the rule that produced it.
type Audience struct {
	Rule    AudienceRule     `json:"rule"`
	Targets []AudienceTarget `json:"targets"`
}

// DeliveryOutcome is the per-target fan-out result (§3.2, §5.2
// chat_deliveries). It is a PROJECTION of the delivery, not a queue: the
// authoritative delivery is the agent's inbox entry, and this is how the room
// reads its outcome back.
type DeliveryOutcome struct {
	Target       string    `json:"target"`
	Outcome      Outcome   `json:"outcome"`
	InboxEntryID string    `json:"inbox_entry_id,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Reason names why a `refused` delivery was refused (an unknown agent, a
	// namespace mismatch, ...). It is deliberately NOT persisted: §5.2's
	// chat_deliveries has no such column and the durable record keeps the
	// vocabulary value; this is the caller's immediate explanation.
	Reason string `json:"-"`
}

// Message is one transcript record (§4.2, §5.2 chat_transcript).
type Message struct {
	ID string `json:"id"`
	// ThreadID is the id of the thread's ROOT message — one key per thread. A
	// reply NEVER carries a new thread_id (D11, §4.5). A thread root's
	// thread_id equals its own message id.
	ThreadID string `json:"thread_id"`
	// ParentID is the immediate parent's message id — reply ATTRIBUTION, not
	// depth (§4.2, D11). Present on a reply, absent on a thread root.
	ParentID       string            `json:"parent_id,omitempty"`
	SessionID      string            `json:"session_id"`
	Kind           MessageKind       `json:"kind"`
	Author         AuthorRef         `json:"author"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	Audience       Audience          `json:"audience"`
	Outcomes       []DeliveryOutcome `json:"outcomes,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	// Seq is the ordering authority within the session; ts is display and
	// correlation (§3.1). A client must never order by created_at alone.
	Seq       int64     `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// IsRoot reports whether this message is a thread root: a root has no
// parent_id and its thread_id is its own message id (§4.3).
func (m *Message) IsRoot() bool { return m.ParentID == "" }

// Thread is one node of the thread tree (§4.5, §5.2 chat_threads). It is the
// projection of `session.thread.branch`. A ROOT thread has no parent and no
// anchor; a SUB-THREAD names both. Depth is a query over this tree — never a
// parent_id hop-count (§4.7 rule 1).
type Thread struct {
	ID              string    `json:"id"`
	SessionID       string    `json:"session_id"`
	ParentThreadID  string    `json:"parent_thread_id,omitempty"`
	RootMessageID   string    `json:"root_message_id"`
	AnchorMessageID string    `json:"anchor_message_id,omitempty"`
	CreatedBy       AuthorRef `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
}

// State is the reduced session view: the content a reader gets from a
// session's records, in seq order, with no side table. Both backends are
// measured against it — the JSONL log reduces to it by Replay and the
// PostgreSQL view assembles to it — which is what makes CR-CHAT-002's
// round-trip acceptance checkable as ONE comparison.
type State struct {
	Session       *Session
	Members       []*Member
	Messages      []*Message
	Threads       []*Thread
	ContextShares []*MemberContext
}

// Replay reconstructs a session's State from its transcript records ALONE
// (§4.3). It orders by (session_id, seq) with keep-LAST per (session_id, seq)
// (§5.1: identical duplicates are no-ops), and requires no membership lookup,
// side table or client state.
func Replay(sessionID string, recs []*Record) (*State, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is empty", ErrInvalidRecord)
	}

	// keep-LAST per (session_id, seq): a later line with the same seq wins,
	// an identical replay is a no-op, and the surviving record keeps the
	// FILE ORDER position of its last occurrence so ties are stable.
	lastIdx := make(map[int64]int, len(recs))
	kept := make([]*Record, 0, len(recs))
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if rec.SessionID != sessionID {
			return nil, fmt.Errorf("%w: record seq %d names session %q, want %q",
				ErrInvalidRecord, rec.Seq, rec.SessionID, sessionID)
		}
		if err := rec.Validate(); err != nil {
			return nil, err
		}
		if idx, ok := lastIdx[rec.Seq]; ok {
			kept[idx] = rec
			continue
		}
		lastIdx[rec.Seq] = len(kept)
		kept = append(kept, rec)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })

	st := &State{}
	for _, rec := range kept {
		if err := st.apply(rec); err != nil {
			return nil, err
		}
	}
	if st.Session == nil {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	st.normalize()
	return st, nil
}

// apply folds one record into the state.
func (st *State) apply(rec *Record) error {
	// §1.3: closed admits no new message, reply or membership change; close
	// and reopen are the only records a closed room accepts. Closing does not
	// delete — the transcript stays readable.
	if st.Session != nil && st.Session.State == SessionClosed {
		switch rec.Type {
		case RecordMessage, RecordThreadReply, RecordMemberAdd, RecordMemberContext,
			RecordMemberRemove, RecordThreadBranch:
			return fmt.Errorf("%w: %s refused on a closed session", ErrSessionClosed, rec.Type)
		}
	}
	switch rec.Type {
	case RecordSessionCreate:
		s := &Session{
			ID:               rec.SessionID,
			Namespace:        rec.Namespace,
			Kind:             rec.Kind,
			Title:            rec.Title,
			CreatedAt:        rec.TS,
			State:            SessionOpen,
			RetentionSeconds: rec.RetentionSeconds,
		}
		if rec.Kind == "" {
			s.Kind = DefaultKind
		}
		if rec.CreatedBy != nil {
			s.CreatedBy = *rec.CreatedBy
		}
		st.Session = s
	case RecordMemberAdd:
		st.upsertMember(rec)
		if rec.ContextShare != nil {
			st.upsertContextShare(rec, rec.ContextShare)
		}
	case RecordMemberContext:
		if rec.ContextShare == nil {
			return fmt.Errorf("%w: session.member.context without context_share", ErrInvalidRecord)
		}
		st.upsertContextShare(rec, rec.ContextShare)
	case RecordMemberRemove:
		m := st.findMember(rec.MemberType, rec.MemberID)
		if m == nil {
			return fmt.Errorf("%w: session.member.remove for %s %q that was never added",
				ErrInvalidRecord, rec.MemberType, rec.MemberID)
		}
		at := rec.TS
		m.RemovedAt = &at
		m.RemovedBy = actorID(rec.Actor)
	case RecordMessage, RecordThreadReply:
		st.upsertMessage(rec)
		if rec.Type == RecordMessage {
			st.ensureRootThread(rec)
		}
	case RecordThreadBranch:
		st.upsertThread(rec)
	case RecordClose:
		if st.Session == nil {
			return fmt.Errorf("%w: session.close before session.create", ErrInvalidRecord)
		}
		at := rec.TS
		st.Session.State = SessionClosed
		st.Session.ClosedAt = &at
	case RecordReopen:
		if st.Session == nil {
			return fmt.Errorf("%w: session.reopen before session.create", ErrInvalidRecord)
		}
		st.Session.State = SessionOpen
		st.Session.ClosedAt = nil
	default:
		return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, rec.Type)
	}
	return nil
}

func (st *State) findMember(mt MemberType, id string) *Member {
	for _, m := range st.Members {
		if m.MemberType == mt && m.MemberID == id {
			return m
		}
	}
	return nil
}

func (st *State) upsertMember(rec *Record) {
	m := st.findMember(rec.MemberType, rec.MemberID)
	if m == nil {
		m = &Member{SessionID: rec.SessionID, MemberType: rec.MemberType, MemberID: rec.MemberID}
		st.Members = append(st.Members, m)
	}
	m.Role = rec.Role
	m.AddedAt = rec.TS
	m.AddedBy = actorID(rec.Actor)
	// A re-add is a NEW event, never a rollback (§2.4 rule 5): it clears the
	// removal so the join/leave/join sequence is the honest record.
	m.RemovedAt = nil
	m.RemovedBy = ""
}

func (st *State) upsertContextShare(rec *Record, cs *ContextShare) {
	mc := st.findContextShare(rec.MemberType, rec.MemberID)
	if mc == nil {
		mc = &MemberContext{
			SessionID:  rec.SessionID,
			MemberType: rec.MemberType,
			MemberID:   rec.MemberID,
		}
		st.ContextShares = append(st.ContextShares, mc)
	}
	mc.Mode = cs.Mode
	mc.BoundaryMessageID = cs.BoundaryMessageID
	mc.SetBy = actorID(rec.Actor)
	mc.SetAt = rec.TS
}

func (st *State) findContextShare(mt MemberType, id string) *MemberContext {
	for _, c := range st.ContextShares {
		if c.MemberType == mt && c.MemberID == id {
			return c
		}
	}
	return nil
}

func (st *State) upsertMessage(rec *Record) {
	m := &Message{
		ID:             rec.MessageID,
		ThreadID:       rec.ThreadID,
		ParentID:       rec.ParentID,
		SessionID:      rec.SessionID,
		Kind:           rec.MessageKind,
		Payload:        rec.Payload,
		Outcomes:       rec.Outcomes,
		IdempotencyKey: rec.IdempotencyKey,
		Seq:            rec.Seq,
		CreatedAt:      rec.TS,
	}
	if rec.Author != nil {
		m.Author = *rec.Author
	}
	if rec.Audience != nil {
		m.Audience = *rec.Audience
	}
	for i, existing := range st.Messages {
		if existing.ID == rec.MessageID {
			st.Messages[i] = m
			return
		}
	}
	st.Messages = append(st.Messages, m)
}

// ensureRootThread materialises the thread a root message defines. A thread
// root's thread_id IS its own message id (§4.3), and the §5.2 view keeps one
// chat_threads row per thread — so a root thread gets a row with no parent and
// no anchor, which is exactly how §5.2 describes it ("A root thread has
// parent_thread_id NULL and anchor_message_id NULL").
func (st *State) ensureRootThread(rec *Record) {
	if st.Thread(rec.ThreadID) != nil {
		return
	}
	t := &Thread{
		ID:            rec.ThreadID,
		SessionID:     rec.SessionID,
		RootMessageID: rec.MessageID,
		CreatedAt:     rec.TS,
	}
	if rec.Author != nil {
		t.CreatedBy = *rec.Author
	}
	st.Threads = append(st.Threads, t)
}

func (st *State) upsertThread(rec *Record) {
	t := &Thread{
		ID:              rec.ThreadID,
		SessionID:       rec.SessionID,
		ParentThreadID:  rec.ParentThreadID,
		RootMessageID:   rec.RootMessageID,
		AnchorMessageID: rec.AnchorMessageID,
		CreatedAt:       rec.TS,
	}
	if rec.Actor != nil {
		t.CreatedBy = *rec.Actor
	}
	for i, existing := range st.Threads {
		if existing.ID == rec.ThreadID {
			st.Threads[i] = t
			return
		}
	}
	st.Threads = append(st.Threads, t)
}

// normalize orders every slice deterministically so two projections of the
// same facts compare equal.
func (st *State) normalize() {
	sort.SliceStable(st.Members, func(i, j int) bool {
		if st.Members[i].MemberType != st.Members[j].MemberType {
			return st.Members[i].MemberType < st.Members[j].MemberType
		}
		return st.Members[i].MemberID < st.Members[j].MemberID
	})
	sort.SliceStable(st.Messages, func(i, j int) bool { return st.Messages[i].Seq < st.Messages[j].Seq })
	sort.SliceStable(st.Threads, func(i, j int) bool { return st.Threads[i].ID < st.Threads[j].ID })
	sort.SliceStable(st.ContextShares, func(i, j int) bool {
		if st.ContextShares[i].MemberType != st.ContextShares[j].MemberType {
			return st.ContextShares[i].MemberType < st.ContextShares[j].MemberType
		}
		return st.ContextShares[i].MemberID < st.ContextShares[j].MemberID
	})
	for _, m := range st.Messages {
		sort.SliceStable(m.Outcomes, func(i, j int) bool { return m.Outcomes[i].Target < m.Outcomes[j].Target })
	}
}

// MembersAt returns the membership as of time t (§2.3 consequence 1) — the
// question "who could read this message when it was sent" answered by replay.
func (st *State) MembersAt(t time.Time) []*Member {
	var out []*Member
	for _, m := range st.Members {
		if m.ActiveAt(t) {
			out = append(out, m)
		}
	}
	return out
}

// Thread returns the thread with the given id, or nil.
func (st *State) Thread(id string) *Thread {
	for _, t := range st.Threads {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Message returns the message with the given id, or nil.
func (st *State) Message(id string) *Message {
	for _, m := range st.Messages {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// ThreadDepth returns a thread's level in the thread tree: a root thread is 0
// and each deliberate branch adds one (§4.5, §4.7 rule 1). Depth is a walk of
// parent_thread_id — NEVER a parent_id hop-count. A cycle (which a valid
// transcript cannot contain) is reported as a depth of -1 rather than looping.
func (st *State) ThreadDepth(threadID string) int {
	seen := map[string]bool{}
	depth := 0
	for id := threadID; id != ""; {
		if seen[id] {
			return -1
		}
		seen[id] = true
		t := st.Thread(id)
		if t == nil || t.ParentThreadID == "" {
			return depth
		}
		depth++
		id = t.ParentThreadID
	}
	return depth
}

// ThreadMessages returns the messages of one thread in seq order — the
// grouping §4.3 requires, which is what makes a subtree a single lookup.
func (st *State) ThreadMessages(threadID string) []*Message {
	var out []*Message
	for _, m := range st.Messages {
		if m.ThreadID == threadID {
			out = append(out, m)
		}
	}
	return out
}

// CanonicalJSON renders v with sorted object keys and no insignificant
// whitespace, so two projections of the same content compare as bytes. It is
// the equality the CR-CHAT-002 round-trip test asserts with.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// actorID renders an AuthorRef as the single actor id the projections store.
func actorID(a *AuthorRef) string {
	if a == nil {
		return ""
	}
	if a.Agent != "" {
		return a.Agent
	}
	if a.AsAgent != "" {
		return a.AsAgent
	}
	if a.Principal != "" {
		return a.Principal
	}
	return a.System
}

// validMemberType reports whether mt is a known participant kind.
func validMemberType(mt MemberType) bool {
	return mt == MemberAgent || mt == MemberPrincipal
}

// trimSessionID is the filename-safe guard for the JSONL store: the spec
// fixes session ids as opaque, URL-safe tokens, so a path separator or a
// traversal sequence is refused rather than escaped.
func trimSessionID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: session id is empty", ErrInvalidRecord)
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("%w: session id %q is not path-safe", ErrInvalidRecord, id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("%w: session id %q contains %q", ErrInvalidRecord, id, r)
	}
	return id, nil
}
