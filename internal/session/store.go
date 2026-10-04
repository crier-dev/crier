package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Errors returned by the session stores and by record validation.
var (
	// ErrSessionNotFound is returned when a session has no record in the
	// store (a session that is not in the log does not exist, §1.3).
	ErrSessionNotFound = errors.New("session not found")
	// ErrInvalidRecord is returned for a log line that does not satisfy the
	// §5.1 shape (unknown version, unknown type, missing required field).
	ErrInvalidRecord = errors.New("invalid session record")
	// ErrSessionClosed is returned when a record other than close/reopen is
	// applied to a closed session (§1.3). Closing does not delete; it stops
	// admission.
	ErrSessionClosed = errors.New("session is closed")
	// ErrMessageNotFound is returned by the request→thread flow (OpenRequest /
	// Reply) when a record names more than nothing: a reply whose parent_id is
	// not in the session's transcript.
	ErrMessageNotFound = errors.New("message not found")
	// ErrThreadMismatch is returned when a reply names a thread other than its
	// parent's. A reply never carries a new thread_id and never moves out of its
	// thread (§4.5, D11), so the contradiction is refused, not repaired.
	ErrThreadMismatch = errors.New("thread mismatch")
)

// RecordFormatVersion is the envelope version this package writes and the only
// version it reads. A line whose `v` is unknown is refused loudly rather than
// skipped (§5.1).
const RecordFormatVersion = 1

// RecordType is the closed set of session log line types (§5.1). One line per
// record, `v` first, so a reader can dispatch on a version it knows.
type RecordType string

const (
	RecordSessionCreate RecordType = "session.create"
	RecordMemberAdd     RecordType = "session.member.add"
	RecordMemberContext RecordType = "session.member.context"
	RecordMemberRemove  RecordType = "session.member.remove"
	RecordMessage       RecordType = "session.message"
	RecordThreadReply   RecordType = "session.thread.reply"
	RecordThreadBranch  RecordType = "session.thread.branch"
	RecordClose         RecordType = "session.close"
	RecordReopen        RecordType = "session.reopen"
)

// Record is ONE line of the JSONL ordered append log (§5.1) — the transport
// form and the ordering authority. Fields are shared where the spec shares
// them (`v`, `type`, `session_id`, `seq`, `ts`) and type-specific otherwise;
// every type-specific field is omitempty so a line carries only what its type
// defines.
type Record struct {
	V         int        `json:"v"`
	Type      RecordType `json:"type"`
	SessionID string     `json:"session_id"`
	// Seq is the monotonic per-session ordering authority (§3.1). Allocation
	// authority is an OPEN QUESTION owned by CHAT-STORAGE.md §6.1; this
	// package assumes the spec's single-appender posture and takes the seq
	// from the caller, never minting one behind its back.
	Seq int64     `json:"seq"`
	TS  time.Time `json:"ts"`

	// --- session.create ---
	Namespace        string     `json:"namespace,omitempty"`
	Kind             Kind       `json:"kind,omitempty"`
	Title            string     `json:"title,omitempty"`
	CreatedBy        *AuthorRef `json:"created_by,omitempty"`
	RetentionSeconds *int       `json:"retention_seconds,omitempty"`
	Group            string     `json:"group,omitempty"`
	Visibility       string     `json:"visibility,omitempty"`

	// --- member.add / member.context / member.remove ---
	MemberType   MemberType    `json:"member_type,omitempty"`
	MemberID     string        `json:"member_id,omitempty"`
	Role         MemberRole    `json:"role,omitempty"`
	Actor        *AuthorRef    `json:"actor,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	ContextShare *ContextShare `json:"context_share,omitempty"`

	// --- session.message / session.thread.reply ---
	MessageID      string            `json:"message_id,omitempty"`
	ThreadID       string            `json:"thread_id,omitempty"`
	ParentID       string            `json:"parent_id,omitempty"`
	MessageKind    MessageKind       `json:"message_kind,omitempty"`
	Author         *AuthorRef        `json:"author,omitempty"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	Audience       *Audience         `json:"audience,omitempty"`
	Outcomes       []DeliveryOutcome `json:"outcomes,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`

	// --- session.thread.branch ---
	ParentThreadID  string `json:"parent_thread_id,omitempty"`
	AnchorMessageID string `json:"anchor_message_id,omitempty"`
	RootMessageID   string `json:"root_message_id,omitempty"`
}

// Validate checks the record against the §5.1 WRITE contract: the shape every
// line this package writes must satisfy. It is the same check every store
// applies before a line is written or projected, so a malformed record is
// refused at the boundary rather than becoming a corrupt line.
//
// A message record MUST carry thread_id here: that is the shape this package
// writes (CR-CHAT-005). A reader is more forgiving — see validateReadable.
func (r *Record) Validate() error { return r.validate(true) }

// validateReadable is Validate as a READER applies it: identical, except that
// a message record with NO thread_id is accepted rather than refused.
//
// thread_id shipped as a wire tag (CR-FEAT-004) long before it was stored on
// the record, so a transcript line written by that generation — or a bundle
// imported from one — carries no thread_id. Such a record is not malformed:
// §4.3 makes the thread reconstructable from the transcript ALONE, so the
// reader derives the key (State.resolveThreads) instead of refusing the
// record. Nothing on the write path uses this check, which is what keeps the
// tolerance strictly one-directional: the package still never WRITES a
// thread-less message.
func (r *Record) validateReadable() error { return r.validate(false) }

func (r *Record) validate(requireThreadID bool) error {
	if r == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidRecord)
	}
	if r.V != RecordFormatVersion {
		return fmt.Errorf("%w: unsupported record version %d (want %d)", ErrInvalidRecord, r.V, RecordFormatVersion)
	}
	if r.SessionID == "" {
		return fmt.Errorf("%w: session_id is empty", ErrInvalidRecord)
	}
	if r.Seq <= 0 {
		return fmt.Errorf("%w: seq must be positive, got %d", ErrInvalidRecord, r.Seq)
	}
	if r.TS.IsZero() {
		return fmt.Errorf("%w: ts is zero", ErrInvalidRecord)
	}

	switch r.Type {
	case RecordSessionCreate:
		if r.CreatedBy == nil || r.CreatedBy.IsZero() {
			return fmt.Errorf("%w: session.create without created_by", ErrInvalidRecord)
		}
	case RecordMemberAdd:
		if !validMemberType(r.MemberType) {
			return fmt.Errorf("%w: session.member.add with member_type %q", ErrInvalidRecord, r.MemberType)
		}
		if r.MemberID == "" || r.Role == "" {
			return fmt.Errorf("%w: session.member.add without member_id/role", ErrInvalidRecord)
		}
		if r.ContextShare != nil {
			return validateContextShare(r.ContextShare)
		}
	case RecordMemberContext:
		if !validMemberType(r.MemberType) || r.MemberID == "" {
			return fmt.Errorf("%w: session.member.context without member_type/member_id", ErrInvalidRecord)
		}
		if r.ContextShare == nil {
			return fmt.Errorf("%w: session.member.context without context_share", ErrInvalidRecord)
		}
		return validateContextShare(r.ContextShare)
	case RecordMemberRemove:
		if !validMemberType(r.MemberType) || r.MemberID == "" {
			return fmt.Errorf("%w: session.member.remove without member_type/member_id", ErrInvalidRecord)
		}
	case RecordMessage, RecordThreadReply:
		if r.MessageID == "" {
			return fmt.Errorf("%w: %s without message_id", ErrInvalidRecord, r.Type)
		}
		if requireThreadID && r.ThreadID == "" {
			return fmt.Errorf("%w: %s without thread_id", ErrInvalidRecord, r.Type)
		}
		if !validMessageKind(r.MessageKind) {
			return fmt.Errorf("%w: %s with message_kind %q", ErrInvalidRecord, r.Type, r.MessageKind)
		}
		if r.Audience == nil {
			return fmt.Errorf("%w: %s without audience", ErrInvalidRecord, r.Type)
		}
		if r.Type == RecordMessage {
			if r.ParentID != "" {
				return fmt.Errorf("%w: session.message carries parent_id %q (a reply is session.thread.reply)",
					ErrInvalidRecord, r.ParentID)
			}
			// A root's thread_id IS its own message id (§4.3). A legacy record
			// that carries NONE is tolerated on the read path (and derived); a
			// record carrying a DIFFERENT id is malformed either way.
			if r.ThreadID != "" && r.ThreadID != r.MessageID {
				return fmt.Errorf("%w: a thread root's thread_id %q must equal its message_id %q (§4.3)",
					ErrInvalidRecord, r.ThreadID, r.MessageID)
			}
		}
		if r.Type == RecordThreadReply && r.ParentID == "" {
			return fmt.Errorf("%w: session.thread.reply without parent_id", ErrInvalidRecord)
		}
	case RecordThreadBranch:
		if r.ThreadID == "" || r.ParentThreadID == "" || r.AnchorMessageID == "" {
			return fmt.Errorf("%w: session.thread.branch without thread_id/parent_thread_id/anchor_message_id", ErrInvalidRecord)
		}
		if r.RootMessageID == "" {
			return fmt.Errorf("%w: session.thread.branch without root_message_id", ErrInvalidRecord)
		}
	case RecordClose, RecordReopen:
		// actor is optional in the shape (§5.1 shows it, but a system close
		// with no named actor is still a valid record).
	default:
		return fmt.Errorf("%w: unknown record type %q", ErrInvalidRecord, r.Type)
	}
	return nil
}

func validMessageKind(k MessageKind) bool {
	return k == MessagePlain || k == MessageAddressed || k == MessageTask
}

// validateContextShare checks the §4.6 share answer: a known mode, and a
// boundary message id exactly when the mode names one.
func validateContextShare(cs *ContextShare) error {
	switch cs.Mode {
	case ShareNone, ShareSummary, ShareFull:
		return nil
	case ShareSince:
		if cs.BoundaryMessageID == "" {
			return fmt.Errorf("%w: context_share mode %q without boundary_message_id", ErrInvalidRecord, ShareSince)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown context_share mode %q", ErrInvalidRecord, cs.Mode)
	}
}

// MarshalLine renders the record as ONE JSONL line (trailing newline
// included), the only byte form this package appends or imports.
func (r *Record) MarshalLine() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal session record: %w", err)
	}
	return append(b, '\n'), nil
}

// ParseRecord parses one JSONL line into a Record and validates it against the
// §5.1 READ contract (validateReadable): a message record that carries no
// thread_id — the pre-persisted shape CR-FEAT-004 wrote — is accepted, because
// §4.3 makes the thread reconstructable from the transcript alone and the
// reader derives the key. Everything else is checked exactly as Validate
// checks it. A caller that sees ErrInvalidRecord has an unknown or malformed
// line; the log is the log of record, so it is reported, never silently
// dropped.
func ParseRecord(line []byte) (*Record, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if err := rec.validateReadable(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Log is the ordered append log half of the dual backend (§2.1, §5.1):
// append one record-version line, read the lines back in seq order. Both
// stores implement it, which is what lets the same record stream feed the
// JSONL log and the PostgreSQL projection.
type Log interface {
	// Append writes one record-version. Implementations fsync before they
	// return (§2.3): a message is durable for the room when its transcript
	// record is in the log.
	Append(ctx context.Context, rec *Record) error
	// Records returns the session's records ordered by seq, with keep-LAST
	// per (session_id, seq) applied.
	Records(ctx context.Context, sessionID string) ([]*Record, error)
}

// View is the query-view half of the dual backend (§2.1, §5.2): the reduced
// State assembled from the records. It is what every read path serves from.
type View interface {
	Load(ctx context.Context, sessionID string) (*State, error)
}

// Store is both halves of the dual-backend contract plus Close.
type Store interface {
	Log
	View
	Close() error
}

// Record builders. One per log type, so a caller composes the §5.1 line
// through the domain object instead of hand-assembling a union struct.

// CreateRecord renders the session.create line (§5.1).
func (s *Session) CreateRecord(seq int64, ts time.Time) *Record {
	cb := s.CreatedBy
	return &Record{
		V:                RecordFormatVersion,
		Type:             RecordSessionCreate,
		SessionID:        s.ID,
		Seq:              seq,
		TS:               ts.UTC(),
		Namespace:        s.Namespace,
		Kind:             s.Kind,
		Title:            s.Title,
		CreatedBy:        &cb,
		RetentionSeconds: s.RetentionSeconds,
		Group:            s.Group,
		Visibility:       s.Visibility,
	}
}

// AddRecord renders the session.member.add line (§5.1) and records the
// late-join context answer when one is given (§4.6 rule 1).
func (m *Member) AddRecord(seq int64, ts time.Time, share *ContextShare) *Record {
	actor := m.AddedBy
	rec := &Record{
		V:            RecordFormatVersion,
		Type:         RecordMemberAdd,
		SessionID:    m.SessionID,
		Seq:          seq,
		TS:           ts.UTC(),
		MemberType:   m.MemberType,
		MemberID:     m.MemberID,
		Role:         m.Role,
		ContextShare: share,
	}
	if actor != "" {
		rec.Actor = &AuthorRef{Agent: actor}
	}
	return rec
}

// RemoveRecord renders the session.member.remove line (§5.1).
func (m *Member) RemoveRecord(seq int64, ts time.Time, actor AuthorRef, reason string) *Record {
	return &Record{
		V:          RecordFormatVersion,
		Type:       RecordMemberRemove,
		SessionID:  m.SessionID,
		Seq:        seq,
		TS:         ts.UTC(),
		MemberType: m.MemberType,
		MemberID:   m.MemberID,
		Actor:      &actor,
		Reason:     reason,
	}
}

// Record renders the session.member.context line — a LATER change to a
// member's context share (§4.6 rule 3): a new record, never a rewrite.
func (mc *MemberContext) Record(seq int64, ts time.Time) *Record {
	rec := &Record{
		V:            RecordFormatVersion,
		Type:         RecordMemberContext,
		SessionID:    mc.SessionID,
		Seq:          seq,
		TS:           ts.UTC(),
		MemberType:   mc.MemberType,
		MemberID:     mc.MemberID,
		ContextShare: &ContextShare{Mode: mc.Mode, BoundaryMessageID: mc.BoundaryMessageID},
	}
	if mc.SetBy != "" {
		rec.Actor = &AuthorRef{Agent: mc.SetBy}
	}
	return rec
}

// Record renders a message line: session.message for a thread root and
// session.thread.reply for a reply (§5.1). It does NOT mint a new thread_id —
// a reply carries the thread it is already in (D11, §4.5) — and a root's
// thread_id is forced to its own message id (§4.3).
func (m *Message) Record() *Record {
	typ := RecordMessage
	if !m.IsRoot() {
		typ = RecordThreadReply
	}
	threadID := m.ThreadID
	if typ == RecordMessage && threadID == "" {
		threadID = m.ID
	}
	author := m.Author
	aud := m.Audience
	return &Record{
		V:              RecordFormatVersion,
		Type:           typ,
		SessionID:      m.SessionID,
		Seq:            m.Seq,
		TS:             m.CreatedAt.UTC(),
		MessageID:      m.ID,
		ThreadID:       threadID,
		ParentID:       m.ParentID,
		MessageKind:    m.Kind,
		Author:         &author,
		Payload:        m.Payload,
		Audience:       &aud,
		Outcomes:       m.Outcomes,
		IdempotencyKey: m.IdempotencyKey,
	}
}

// BranchRecord renders the session.thread.branch line (§5.1): the ONE record
// that creates a new thread_id, carrying the parent thread and the anchor
// message the branch hangs from (§4.5 rule 3). Nothing about the parent
// thread changes, and branching issues no fan-out by itself.
func (t *Thread) BranchRecord(seq int64, ts time.Time, reason string) *Record {
	actor := t.CreatedBy
	return &Record{
		V:               RecordFormatVersion,
		Type:            RecordThreadBranch,
		SessionID:       t.SessionID,
		Seq:             seq,
		TS:              ts.UTC(),
		ThreadID:        t.ID,
		ParentThreadID:  t.ParentThreadID,
		AnchorMessageID: t.AnchorMessageID,
		RootMessageID:   t.RootMessageID,
		Actor:           &actor,
		Reason:          reason,
	}
}

// CloseRecord renders the session.close line (§5.1).
func CloseRecord(sessionID string, seq int64, ts time.Time, actor AuthorRef, reason string) *Record {
	return &Record{
		V:         RecordFormatVersion,
		Type:      RecordClose,
		SessionID: sessionID,
		Seq:       seq,
		TS:        ts.UTC(),
		Actor:     &actor,
		Reason:    reason,
	}
}

// ReopenRecord renders the session.reopen line (§5.1): an event, not a field
// mutation — a record is never rewritten, so "was this open on date T" is
// answerable by replay.
func ReopenRecord(sessionID string, seq int64, ts time.Time, actor AuthorRef) *Record {
	return &Record{
		V:         RecordFormatVersion,
		Type:      RecordReopen,
		SessionID: sessionID,
		Seq:       seq,
		TS:        ts.UTC(),
		Actor:     &actor,
	}
}

// NextSeq returns the seq a single appender should use next for a session:
// the highest seq already present plus one. It exists so a caller does not
// invent a different allocation rule than the log's; a caller that has already
// read the records passes them instead of the store.
func NextSeq(recs []*Record) int64 {
	var max int64
	for _, r := range recs {
		if r != nil && r.Seq > max {
			max = r.Seq
		}
	}
	return max + 1
}

// Conflict is two records that claim the same (session_id, seq) with DIFFERENT
// content. §3.1 makes that a finding and not a silent last-write-wins; §5.1's
// replay rule (keep-LAST per (session_id, seq)) is what resolves it, because
// the "outcomes written back onto the same record" step of §3.2 writes a
// second line at the same seq by design.
//
// The two are not contradictory once stated separately: keep-LAST decides
// WHICH line wins, and Conflicts is how a caller sees that a decision was made
// at all rather than losing the fact.
type Conflict struct {
	SessionID string
	Seq       int64
	Types     [2]RecordType
}

// Conflicts reports every (session_id, seq) claimed by records whose content
// differs, in first-seen order. Identical duplicates are no-ops (§5.1) and are
// not reported.
func Conflicts(recs []*Record) []Conflict {
	type key struct {
		sessionID string
		seq       int64
	}
	first := map[key]*Record{}
	var out []Conflict
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		k := key{rec.SessionID, rec.Seq}
		prev, ok := first[k]
		if !ok {
			first[k] = rec
			continue
		}
		if recordContentEqual(prev, rec) || writeBackEquivalent(prev, rec) {
			continue
		}
		out = append(out, Conflict{SessionID: rec.SessionID, Seq: rec.Seq, Types: [2]RecordType{prev.Type, rec.Type}})
	}
	return out
}

// recordContentEqual compares two records by canonical JSON, which is what
// "identical content" means for a replay (§5.1): a whitespace or map-ordering
// difference is not a conflict.
func recordContentEqual(a, b *Record) bool {
	ab, err := CanonicalJSON(a)
	if err != nil {
		return false
	}
	bb, err := CanonicalJSON(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

// writeBackEquivalent reports whether two lines are the §3.2 write-back pair:
// the SAME message record differing only in its fan-out outcomes. Writing the
// outcomes back onto the record is required by D1/§3.2, so it is a known,
// expected second line at the same seq — a decision the log made, not a
// divergence to report.
func writeBackEquivalent(a, b *Record) bool {
	if a.MessageID == "" || a.MessageID != b.MessageID || a.Type != b.Type {
		return false
	}
	x, y := *a, *b
	x.Outcomes, y.Outcomes = nil, nil
	return recordContentEqual(&x, &y)
}
