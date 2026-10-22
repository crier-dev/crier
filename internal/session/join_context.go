package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/registry"
)

// join_context.go — the late-join context share SURFACE (§4.6, D10,
// CR-CHAT-016). The record layer (ContextShare, the session.member.add /
// session.member.context records, and the chat_context_shares projection)
// already exists; this file adds what the spec owes on top of it:
//
//   - the DEFAULT: a join whose request names no share answers `summary`
//     (D10) — the answer is still recorded, never left undecided;
//   - PUT /sessions/{id}/participants/{member_type}/{member_id}/context —
//     a LATER change of the share mode, itself recorded as a
//     session.member.context event (a new record, never a rewrite, §4.6
//     rule 3);
//   - GET /sessions/{id}/participants/{member_type}/{member_id}/context —
//     the joiner's MATERIALIZED view: what it was given, the exact
//     boundary, and an explanation tying its reply state to what it holds
//     (§4.6: "why it answered as it did" must stay answerable).
//
// A share mode never widens the audience (§4.6 rule 5): nothing here
// touches fan-out or delivery.

// UnmarshalJSON accepts the concise join form while preserving the existing
// object API. Strict decoding is retained, including for nested share fields.
func (req *addParticipantRequest) UnmarshalJSON(data []byte) error {
	type plain addParticipantRequest
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if raw := bytes.TrimSpace(wire["context_share"]); len(raw) > 0 && raw[0] == '"' {
		var mode ContextShareMode
		if err := json.Unmarshal(raw, &mode); err != nil {
			return err
		}
		converted, err := json.Marshal(ContextShare{Mode: mode})
		if err != nil {
			return err
		}
		wire["context_share"] = converted
	}
	converted, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(converted))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(req))
}

// shareBoundaryProblem reports, for the ONE field both write paths record, why
// a carried boundary cannot be stored — or "" when it can. A boundary is a
// citation of a message of THIS session: an id the session does not hold is
// refused whatever the mode says, because `summary` and `full` also honour the
// recorded boundary as the upper edge of what the joiner was given
// (materializedShare), so an unverified id there is asserted as history just as
// surely as under `since` (DF-CRIER-472).
//
// Both the add path (http.go) and the later-change path
// (HandleSetMemberContext) call this and it is the ONLY boundary-existence
// check in the package: one predicate, so the two write paths of the same field
// cannot drift apart again — which is exactly how the defect got in (the PUT
// path validated only `since` while the add path validated any non-empty id).
func shareBoundaryProblem(st *State, sessID string, cs *ContextShare) string {
	if cs == nil || cs.BoundaryMessageID == "" {
		return ""
	}
	if st.Message(cs.BoundaryMessageID) != nil {
		return ""
	}
	return fmt.Sprintf("context boundary message %q does not exist in session %q", cs.BoundaryMessageID, sessID)
}

// resolveJoinShare pins the upper edge of a generated summary/full history so
// its substance stays citable. No messages is a recorded none, with a reason.
func resolveJoinShare(st *State, cs *ContextShare) string {
	if len(st.Messages) == 0 {
		cs.Mode, cs.BoundaryMessageID = ShareNone, ""
		return "nothing to share: session has no messages"
	}
	if cs.Mode == ShareNone {
		cs.BoundaryMessageID = ""
	} else if (cs.Mode == ShareSummary || cs.Mode == ShareFull) && cs.BoundaryMessageID == "" {
		var edge *Message
		for _, m := range st.Messages {
			if edge == nil || m.Seq > edge.Seq {
				edge = m
			}
		}
		cs.BoundaryMessageID = edge.ID
	}
	return ""
}

// DefaultContextShareMode is the answer recorded when a joiner does not
// choose (D10): a generated digest — an index, never a replacement for the
// raw messages (§4.7 rule 2).
const DefaultContextShareMode = ShareSummary

// contextShareModeAccepted is the human-readable summary rendered for the
// `summary` mode. It is GENERATED (§4.6 rule 2): the raw message ids stay on
// the view so the joiner can always reach underneath the digest.
func summaryDigest(given []*Message) string {
	if len(given) == 0 {
		return "generated summary: the session held no messages before the join — nothing to digest"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "generated summary of %d message(s) recorded before the join:", len(given))
	for _, m := range given {
		fmt.Fprintf(&b, "\n  #%d %s (%s)", m.Seq, m.Author.AgentID(), m.ID)
	}
	return b.String()
}

// explainShare states, in one sentence, why the joiner's reply state is what
// it is: what it was given, and what was withheld (§4.6, D10).
//
// boundaryUnresolved reports that the share carries a boundary id this session
// cannot resolve. The sentence then asserts the JOIN INSTANT alone and names the
// boundary it could not honour, so the view never states a resolved edge — and
// never says "recorded before the join" of a message that was not
// (DF-CRIER-469).
func explainShare(cs *ContextShare, givenCount int, boundaryUnresolved bool) string {
	if boundaryUnresolved && cs.Mode != ShareNone {
		return fmt.Sprintf("the recorded boundary message %q names no message of this session, so it could not be resolved and was neither honoured nor used to widen the view (mode %q): what the joiner was given is bounded by the join instant alone — %d message(s) recorded before it joined, and NONE recorded at or after it", cs.BoundaryMessageID, cs.Mode, givenCount)
	}
	switch cs.Mode {
	case ShareNone:
		return "the joiner was given NOTHING from before the join (mode `none`, §4.6 rule 6) — a deliberate fresh start, recorded like any other answer; what it says next reflects only what arrived after it joined"
	case ShareSummary:
		return fmt.Sprintf("the joiner was given a GENERATED digest of the %d message(s) recorded before it joined (mode `summary`, D10) — the digest is an index and the raw messages remain reachable underneath it (§4.6 rule 2)", givenCount)
	case ShareSince:
		return fmt.Sprintf("the joiner was given the message(s) recorded AFTER boundary %q (mode `since`) — %d message(s) in total; everything at or before the boundary was withheld", cs.BoundaryMessageID, givenCount)
	case ShareFull:
		return fmt.Sprintf("the joiner was given the ENTIRE history recorded before it joined (mode `full`) — %d message(s)", givenCount)
	default:
		return fmt.Sprintf("the joiner was given the share mode %q", cs.Mode)
	}
}

// shareBoundaryEdge resolves the boundary recorded on a share into the seq it
// cites. ok is false when the boundary is empty OR when the id names no message
// of this session — a record written before the write paths began refusing one
// (DF-CRIER-472). A boundary that cannot be resolved yields NO edge: it can
// neither widen nor narrow what counts as given.
func shareBoundaryEdge(st *State, boundaryMessageID string) (int64, bool) {
	if boundaryMessageID == "" {
		return 0, false
	}
	if edge := st.Message(boundaryMessageID); edge != nil {
		return edge.Seq, true
	}
	return 0, false
}

// materializedShare computes what the member was actually GIVEN at its join:
// the messages recorded before its AddedAt, narrowed by the share mode. For
// `since`, only messages strictly after the boundary (and still before the
// join) count — the boundary itself is withheld.
//
// The two inputs are combined, not chosen between (DF-CRIER-469). The JOIN
// INSTANT is the OUTER bound of what any share could have handed this member —
// it did not exist in the session before AddedAt, so nothing recorded after it
// can have been given by the share, whatever the recorded boundary says. A
// boundary may only NARROW inside that bound. The previous shape took the
// boundary branch for `summary`/`full` and skipped only messages after the
// boundary, so the join-instant guard was never applied there: an unresolvable
// boundary skipped nothing and every message in the session counted as given,
// and a boundary recorded later than the join admitted post-join messages —
// history the share never handed over, presented as recorded fact.
func materializedShare(st *State, cs *ContextShare, addedAt time.Time) []*Message {
	if cs == nil || cs.Mode == ShareNone {
		// `none` is a first-class answer (§4.6 rule 6): the joiner is given
		// NOTHING, by decision.
		return nil
	}
	// A recorded edge is seq-based, not wall-clock-based: equal or skewed
	// timestamps must not drop the very message the share cites.
	edgeSeq, edgeOK := shareBoundaryEdge(st, cs.BoundaryMessageID)
	var given []*Message
	for _, m := range st.Messages {
		// The join instant: applied to EVERY mode and every branch below. A
		// message RECORDED AFTER the join was never handed over by the share.
		// Equality is not "after": a tie must not drop the very message a
		// boundary cites (TestLateJoinSummaryEdgeUsesSeq), because a join and
		// the message it shares are stamped by the same clock at the same
		// resolution.
		if m.CreatedAt.After(addedAt) {
			continue
		}
		switch cs.Mode {
		case ShareSummary, ShareFull:
			// The recorded boundary is the share's upper edge, and it can only
			// narrow the join-instant bound. An edge this session cannot
			// resolve contributes nothing (and the view reports it, so the
			// unverifiable boundary is never silently honoured).
			if edgeOK && m.Seq > edgeSeq {
				continue
			}
		case ShareSince:
			// The boundary itself is withheld; everything after it (and before
			// the join) was given.
			if edgeOK && m.Seq <= edgeSeq {
				continue
			}
		}
		given = append(given, m)
	}
	return given
}

// memberContextView is the GET response body: the recorded share (the latest
// state), the materialized view of what the joiner holds, and the
// explanation tying its reply state to what it was given (§4.6).
type memberContextView struct {
	SessionID  string `json:"session_id"`
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	// Decided is false only for a member whose join predates the share
	// record and was never given one — an UNDECIDED join, which §4.6 rule 6
	// distinguishes from an explicit `none`. New joins are always decided
	// (the summary default).
	Decided bool `json:"decided"`
	// Share is the latest recorded share answer (nil while undecided).
	Share *ContextShare `json:"share,omitempty"`
	// SetBy/SetAt name the event that last set the share (the join, or a
	// later session.member.context change).
	SetBy string    `json:"set_by,omitempty"`
	SetAt time.Time `json:"set_at,omitempty"`

	// Materialized is what the joiner was GIVEN at its join: for `summary`
	// the GENERATED digest text (§4.6 rule 2), for `none` empty, for `since`
	// and `full` the given messages' ids in seq order.
	GeneratedSummary string   `json:"generated_summary,omitempty"`
	GivenMessageIDs  []string `json:"given_message_ids,omitempty"`
	GivenCount       int      `json:"given_count"`
	// Explanation ties the joiner's reply state to what it was given.
	Explanation string `json:"explanation"`
	// BoundaryUnresolved names the share's recorded boundary when this session
	// holds no such message. A share's boundary is a citation of a message: one
	// that cannot be resolved can neither widen nor narrow the given set, so the
	// view REPORTS it rather than silently honouring — or silently dropping — it
	// (DF-CRIER-469). Empty when the boundary resolved (or none was recorded).
	BoundaryUnresolved string `json:"boundary_unresolved,omitempty"`
}

// setMemberContextRequest is the PUT body: the new share answer (§4.6 rule 3
// — a later change is permitted and is itself recorded).
type setMemberContextRequest struct {
	Mode              ContextShareMode `json:"mode"`
	BoundaryMessageID string           `json:"boundary_message_id,omitempty"`
	Actor             string           `json:"actor,omitempty"`
}

// resolveMemberContextVars loads the session and the addressed member from
// the route vars, refusing a session the realm does not hold (404) and a
// member that was never added to it.
func (h *Handler) resolveMemberContextVars(w http.ResponseWriter, r *http.Request) (*State, *Session, *Member, bool) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	vars := mux.Vars(r)
	mt := MemberType(strings.TrimSpace(vars["member_type"]))
	mid := strings.TrimSpace(vars["member_id"])
	m := st.findMember(mt, mid)
	if m == nil {
		writeAPIError(w, http.StatusNotFound, "MEMBER_NOT_FOUND",
			fmt.Sprintf("member %q (%s) was never added to session %q", mid, mt, sess.ID))
		return nil, nil, nil, false
	}
	return st, sess, m, true
}

// HandleMemberContextView serves GET
// /sessions/{id}/participants/{member_type}/{member_id}/context — the
// joiner's materialized view and its explanation (§4.6, D10).
func (h *Handler) HandleMemberContextView(w http.ResponseWriter, r *http.Request) {
	st, sess, m, ok := h.resolveMemberContextVars(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	mc := contextShareFor(st, m.MemberType, m.MemberID)
	if mc == nil {
		// An undecided join (a member added before shares were recorded and
		// never given one). §4.6 rule 6 makes this DISTINCT from an explicit
		// `none`, so it is reported, never folded into a mode.
		writeJSON(w, http.StatusOK, memberContextView{
			SessionID:   sess.ID,
			MemberType:  string(m.MemberType),
			MemberID:    m.MemberID,
			Decided:     false,
			Explanation: "no context-share record exists for this member — what it was given was never decided (distinct from an explicit `none`, §4.6 rule 6)",
		})
		return
	}
	cs := &ContextShare{Mode: mc.Mode, BoundaryMessageID: mc.BoundaryMessageID}
	given := materializedShare(st, cs, m.AddedAt)
	// A boundary the session cannot resolve is REPORTED, never silently
	// honoured: the given set above is bounded by the join instant alone, and
	// the view names the citation it could not follow (DF-CRIER-469).
	boundaryUnresolved := cs.Mode != ShareNone && shareBoundaryProblem(st, sess.ID, cs) != ""
	view := memberContextView{
		SessionID:   sess.ID,
		MemberType:  string(m.MemberType),
		MemberID:    m.MemberID,
		Decided:     true,
		Share:       cs,
		SetBy:       mc.SetBy,
		SetAt:       mc.SetAt,
		GivenCount:  len(given),
		Explanation: explainShare(cs, len(given), boundaryUnresolved),
	}
	if boundaryUnresolved {
		view.BoundaryUnresolved = cs.BoundaryMessageID
	}
	if len(given) > 0 {
		view.GivenMessageIDs = make([]string, 0, len(given))
		for _, g := range given {
			view.GivenMessageIDs = append(view.GivenMessageIDs, g.ID)
		}
	}
	if cs.Mode == ShareSummary {
		view.GeneratedSummary = summaryDigest(given)
	}
	writeJSON(w, http.StatusOK, view)
}

// HandleSetMemberContext serves PUT
// /sessions/{id}/participants/{member_type}/{member_id}/context — a LATER
// change of the share mode, recorded as a session.member.context event (a new
// record, never a rewrite of the add, §4.6 rule 3).
func (h *Handler) HandleSetMemberContext(w http.ResponseWriter, r *http.Request) {
	_, sess, m, ok := h.resolveMemberContextVars(w, r)
	if !ok {
		return
	}
	if sess.State == SessionClosed {
		writeAPIError(w, http.StatusConflict, "SESSION_CLOSED",
			fmt.Sprintf("session %q is closed and admits no context change (§1.3)", sess.ID))
		return
	}
	var req setMemberContextRequest
	if !decodeBody(w, r, &req) {
		return
	}
	cs := &ContextShare{Mode: req.Mode, BoundaryMessageID: strings.TrimSpace(req.BoundaryMessageID)}
	if err := validateContextShare(cs); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_CONTEXT_SHARE", err.Error())
		return
	}
	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	// Validate the boundary for EVERY mode that can carry one — not just
	// `since` (DF-CRIER-472). The recorded event must never assert a boundary
	// id the session does not hold, and `summary`/`full` honour the boundary as
	// the upper edge of what the joiner was given, so an unverified id there is
	// asserted as history exactly as it is under `since`. Same predicate and
	// same error as the add path (shareBoundaryProblem).
	if detail := shareBoundaryProblem(st, sess.ID, cs); detail != "" {
		writeAPIError(w, http.StatusBadRequest, "CONTEXT_BOUNDARY_NOT_FOUND", detail)
		return
	}
	resolveJoinShare(st, cs)
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = strings.TrimSpace(r.Header.Get(registry.HeaderAgentID))
	}
	mc := &MemberContext{
		SessionID:         sess.ID,
		MemberType:        m.MemberType,
		MemberID:          m.MemberID,
		Mode:              cs.Mode,
		BoundaryMessageID: cs.BoundaryMessageID,
		SetBy:             actor,
		SetAt:             h.now(),
	}
	seq, err := h.store.NextSeq(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	setter, ok := h.store.(interface {
		SetMemberContext(ctx context.Context, mc *MemberContext, seq int64) error
	})
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR",
			"the session backend does not support recording a context-share change")
		return
	}
	if err := setter.SetMemberContext(r.Context(), mc, seq); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mc)
}
