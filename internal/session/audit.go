// audit.go — the session + principal AUDIT read surface (CR-CHAT-021,
// specs/CHAT-INTERFACE.md §4 row 21).
//
// What this is, stated so nobody has to guess: an audit event here is a
// RE-RENDER of a record the session log already appends (CHAT-SESSIONS.md §2.3
// — membership is a recorded event, not a derived property). Nothing new is
// written by this surface: the audit read is READ-ONLY over events already
// recorded, which is the honest answer to the opt-in posture the delivery-log
// precedent sets (row 21 is explicit that GET /delivery-log is an OPT-IN
// dead-letter/moderation surface and NOT a session audit — this file never
// touches it).
//
// The event vocabulary is CLOSED. A record type the renderer does not
// recognise is refused with ErrUnknownAuditEvent, never skipped and never
// rendered as a guessed kind — the same loud refusal discipline Record.Validate
// applies at the write boundary. Message records are deliberately NOT audit
// events (they are the transcript, row 14) and are classified as such; a
// REFUSAL is reserved for a type that is neither audit-significant nor a known
// transcript type, i.e. a version skew the caller must see.
package session

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// AuditEventKind is the closed audit vocabulary (row 21's Time / Principal /
// Event / Details panel). The names are the ones the spec draws: session
// lifecycle, membership, and — when the CR-CHAT-003 ACL is armed — the
// permission decisions recorded on the session's subject.
type AuditEventKind string

const (
	AuditSessionStart     AuditEventKind = "session.start"
	AuditSessionClose     AuditEventKind = "session.close"
	AuditSessionReopen    AuditEventKind = "session.reopen"
	AuditMemberAdded      AuditEventKind = "member.added"
	AuditMemberRemoved    AuditEventKind = "member.removed"
	AuditMemberContext    AuditEventKind = "member.context"
	AuditThreadBranched   AuditEventKind = "thread.branched"
	AuditPermissionGrant  AuditEventKind = "permission.grant"
	AuditPermissionRevoke AuditEventKind = "permission.revoke"
)

// AuditVocabulary is the closed set, in a stable order. It is returned by the
// read endpoint so a client can never learn the vocabulary by guessing, and it
// is the list ValidAuditKind accepts.
var AuditVocabulary = []AuditEventKind{
	AuditSessionStart,
	AuditSessionClose,
	AuditSessionReopen,
	AuditMemberAdded,
	AuditMemberRemoved,
	AuditMemberContext,
	AuditThreadBranched,
	AuditPermissionGrant,
	AuditPermissionRevoke,
}

// ValidAuditKind reports whether k is a member of the closed vocabulary.
func ValidAuditKind(k AuditEventKind) bool {
	for _, want := range AuditVocabulary {
		if k == want {
			return true
		}
	}
	return false
}

// ErrUnknownAuditEvent is returned when a record type cannot be classified as
// either an audit event or a known transcript type. It is a REFUSAL, not a
// skip: an audit trail that silently drops what it cannot name is not an audit
// trail.
var ErrUnknownAuditEvent = fmt.Errorf("unknown audit event kind")

// AuditEvent is one row of the audit panel: a timestamp, the principal who
// acted, the event kind, and the type-specific details the kind defines. Every
// detail field is omitempty so a row carries only what its kind means.
type AuditEvent struct {
	Kind AuditEventKind `json:"kind"`
	// Seq is the session record's seq when the event came from the session
	// log (the ordering authority, §3.1); 0 for a permission event, which is
	// recorded in the permission log, not the session log.
	Seq int64     `json:"seq"`
	TS  time.Time `json:"ts"`

	// Actor is who acted (the record's actor, or the creator for
	// session.start). Permission events carry the granting/revoking principal.
	Actor AuthorRef `json:"actor,omitempty"`

	// Member details (member.added / member.removed / member.context).
	MemberType MemberType `json:"member_type,omitempty"`
	MemberID   string     `json:"member_id,omitempty"`
	Role       MemberRole `json:"role,omitempty"`
	Reason     string     `json:"reason,omitempty"`

	// ContextShare rides member.context (the §4.6 recorded change).
	ContextShare *ContextShare `json:"context_share,omitempty"`

	// Thread details (thread.branched).
	ThreadID       string `json:"thread_id,omitempty"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`

	// Permission details (permission.grant / permission.revoke): the grant's
	// principal and id in the permission log, and the tombstone fields of a
	// revocation. Both live in the CR-CHAT-003 store, not the session log.
	GrantPrincipal string     `json:"grant_principal,omitempty"`
	GrantID        string     `json:"grant_id,omitempty"`
	GrantedBy      string     `json:"granted_by,omitempty"`
	GrantedAt      *time.Time `json:"granted_at,omitempty"`
	RevokedBy      string     `json:"revoked_by,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	// GrantActions is the action set the grant carries, copied verbatim.
	GrantActions []string `json:"grant_actions,omitempty"`
}

// AuditEventSource is the OPTIONAL capability a store can carry to serve the
// audit read directly. The JSONL log serves the audit trail from its records
// (RenderAuditEvents); the SQL view serves it from its projected tables — the
// view cannot reconstruct member.context / thread.branch events, and a
// close/reopen PAIR collapses to the latest state, so its close event is the
// durable fact only. A store that implements neither this nor a record stream
// richer than messages yields the trail its backend can honestly serve.
type AuditEventSource interface {
	AuditEvents(ctx context.Context, sessionID string) ([]AuditEvent, error)
}

// AuditEvents over the JSONL log renders the log's records (keep-LAST
// applied) into the closed-vocabulary events.
func (s *JSONLStore) AuditEvents(ctx context.Context, sessionID string) ([]AuditEvent, error) {
	recs, err := s.Records(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return RenderAuditEvents(recs)
}

// AuditEvents over the SQL view renders the projected membership/lifecycle
// tables. Stated limits: the view is LATEST STATE per member and per session,
// so a member's re-join reads as one add, and a close+reopen pair reads as
// neither (the session is open). The JSONL log is the audit of record when a
// deployment needs the full event history (§2.3 consequence 1).
func (s *SQLStore) AuditEvents(ctx context.Context, sessionID string) ([]AuditEvent, error) {
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return StateAuditEvents(st), nil
}

// StateAuditEvents renders a session State's membership/lifecycle facts as
// audit events, ordered by ts. It is the view-side source: every event it
// names is derivable from the projected tables alone.
func StateAuditEvents(st *State) []AuditEvent {
	if st == nil || st.Session == nil {
		return nil
	}
	var out []AuditEvent
	out = append(out, AuditEvent{
		Kind:  AuditSessionStart,
		TS:    st.Session.CreatedAt,
		Actor: st.Session.CreatedBy,
	})
	for _, m := range st.Members {
		ev := AuditEvent{
			Kind:       AuditMemberAdded,
			TS:         m.AddedAt,
			MemberType: m.MemberType,
			MemberID:   m.MemberID,
			Role:       m.Role,
		}
		if m.AddedBy != "" {
			ev.Actor = AuthorRef{Agent: m.AddedBy}
		}
		out = append(out, ev)
		if m.RemovedAt != nil {
			rem := AuditEvent{
				Kind:       AuditMemberRemoved,
				TS:         *m.RemovedAt,
				MemberType: m.MemberType,
				MemberID:   m.MemberID,
				Reason:     m.RemovedBy,
			}
			if m.RemovedBy != "" {
				rem.Actor = AuthorRef{Agent: m.RemovedBy}
			}
			out = append(out, rem)
		}
	}
	if st.Session.State == SessionClosed && st.Session.ClosedAt != nil {
		out = append(out, AuditEvent{Kind: AuditSessionClose, TS: *st.Session.ClosedAt})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// recordAuditKind classifies a session record type. The second return is false
// for a record that is deliberately NOT an audit event (the transcript types);
// an error is returned ONLY for a type that is neither — the refusal case.
func recordAuditKind(t RecordType) (AuditEventKind, bool, error) {
	switch t {
	case RecordSessionCreate:
		return AuditSessionStart, true, nil
	case RecordMemberAdd:
		return AuditMemberAdded, true, nil
	case RecordMemberRemove:
		return AuditMemberRemoved, true, nil
	case RecordMemberContext:
		return AuditMemberContext, true, nil
	case RecordThreadBranch:
		return AuditThreadBranched, true, nil
	case RecordClose:
		return AuditSessionClose, true, nil
	case RecordReopen:
		return AuditSessionReopen, true, nil
	case RecordMessage, RecordThreadReply:
		// The transcript (row 14), not the audit panel. Skipped by design.
		return "", false, nil
	default:
		return "", false, fmt.Errorf("%w: session record type %q is neither an audit event nor a known transcript type", ErrUnknownAuditEvent, t)
	}
}

// RenderAuditEvents renders a session's records (in seq order, keep-LAST
// applied — exactly what Records returns) into audit events. Message records
// are skipped; any record type outside BOTH the audit vocabulary and the
// transcript types is REFUSED with ErrUnknownAuditEvent — a version skew is
// reported, never absorbed.
func RenderAuditEvents(recs []*Record) ([]AuditEvent, error) {
	var out []AuditEvent
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		kind, isAudit, err := recordAuditKind(rec.Type)
		if err != nil {
			return nil, err
		}
		if !isAudit {
			continue
		}
		ev := AuditEvent{
			Kind: kind,
			Seq:  rec.Seq,
			TS:   rec.TS,
		}
		if rec.Actor != nil {
			ev.Actor = *rec.Actor
		}
		switch rec.Type {
		case RecordSessionCreate:
			if rec.CreatedBy != nil {
				ev.Actor = *rec.CreatedBy
			}
		case RecordMemberAdd, RecordMemberRemove, RecordMemberContext:
			ev.MemberType = rec.MemberType
			ev.MemberID = rec.MemberID
			ev.Role = rec.Role
			ev.Reason = rec.Reason
			ev.ContextShare = rec.ContextShare
		case RecordThreadBranch:
			ev.ThreadID = rec.ThreadID
			ev.ParentThreadID = rec.ParentThreadID
			ev.Reason = rec.Reason
		}
		out = append(out, ev)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The permission matrix (row 23) and the per-principal role (row 22).
// ---------------------------------------------------------------------------

// MatrixActions is the permission matrix's COLUMNS: the capability axes the
// approved image draws (§4 row 23: send / read / invite / admin / mint). A
// session's matrix carries the four SESSION axes; `mint` is a namespace/
// capability axis (§6.2) and is deliberately not a session column, and the
// shipped envelope carries no per-session mint surface.
var MatrixActions = []string{"send", "read", "invite", "admin"}

// roleMatrix is the row-23 matrix as data: what each session role may do.
// Reading the table row by row:
//
//	send   owner yes  member yes  observer no
//	read   owner yes  member yes  observer YES
//	invite owner yes  member yes  observer no
//	admin  owner yes  member no   observer no
//
// It mirrors the §5.2 role-bundle shape of specs/CHAT-PERMISSIONS.md at the
// session scale: `read` never implies `send` — an observer is a first-class
// participant with exactly one axis.
var roleMatrix = map[MemberRole]map[string]bool{
	RoleOwner:    {"send": true, "read": true, "invite": true, "admin": true},
	RoleMember:   {"send": true, "read": true, "invite": true, "admin": false},
	RoleObserver: {"send": false, "read": true, "invite": false, "admin": false},
}

// matrixRow is one principal's row of the matrix (row 22's role badge plus
// row 23's tick/dash cells).
type matrixRow struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	// Role is the per-principal role recorded on membership (§2.1: exactly
	// one role, a recorded event). It is the row-22 badge's data.
	Role MemberRole `json:"role"`
	// Allowed carries one cell per column, in MatrixActions order. False is a
	// dash, never an error: the matrix is a read of what IS.
	Allowed map[string]bool `json:"allowed"`
	// Source names where each granted cell came from: `role` (the role
	// bundle) or `grant` (a live CR-CHAT-003 grant on this session). Cells
	// the role does not allow carry the role's value; a live grant ADDS.
	Source map[string]string `json:"source"`
}

// BuildPermissionMatrix derives the matrix from a session's active membership
// and, when the ACL is armed and holds live grants on the session's subject,
// overlays them. A member with no role recorded takes the `member` default —
// stated here rather than guessed at by a client.
func BuildPermissionMatrix(st *State, now time.Time, grants []PermissionsGrantView) []matrixRow {
	rows := make([]matrixRow, 0, len(st.Members))
	for _, m := range st.Members {
		if !m.ActiveAt(now) {
			continue
		}
		role := m.Role
		if role == "" {
			role = RoleMember
		}
		bundle := roleMatrix[role]
		row := matrixRow{
			MemberType: string(m.MemberType),
			MemberID:   m.MemberID,
			Role:       role,
			Allowed:    map[string]bool{},
			Source:     map[string]string{},
		}
		for _, col := range MatrixActions {
			if bundle[col] {
				row.Allowed[col] = true
				row.Source[col] = "role"
			}
		}
		for _, g := range grants {
			if g.Principal != m.MemberID {
				continue
			}
			for _, a := range g.Actions {
				col := a
				if !row.Allowed[col] {
					row.Allowed[col] = true
					row.Source[col] = "grant"
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].MemberType != rows[j].MemberType {
			return rows[i].MemberType < rows[j].MemberType
		}
		return rows[i].MemberID < rows[j].MemberID
	})
	return rows
}
