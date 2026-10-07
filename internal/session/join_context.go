package session

import (
	"context"
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
func explainShare(cs *ContextShare, givenCount int) string {
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

// materializedShare computes what the member was actually GIVEN at its join:
// the messages recorded before its AddedAt, narrowed by the share mode. For
// `since`, only messages strictly after the boundary (and still before the
// join) count — the boundary itself is withheld.
func materializedShare(st *State, cs *ContextShare, addedAt time.Time) []*Message {
	var given []*Message
	if cs.Mode == ShareNone {
		// `none` is a first-class answer (§4.6 rule 6): the joiner is given
		// NOTHING, by decision.
		return nil
	}
	for _, m := range st.Messages {
		if !m.CreatedAt.Before(addedAt) {
			continue
		}
		if cs.Mode == ShareSince && cs.BoundaryMessageID != "" {
			bm := st.Message(cs.BoundaryMessageID)
			if bm != nil && m.Seq <= bm.Seq {
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
	view := memberContextView{
		SessionID:   sess.ID,
		MemberType:  string(m.MemberType),
		MemberID:    m.MemberID,
		Decided:     true,
		Share:       cs,
		SetBy:       mc.SetBy,
		SetAt:       mc.SetAt,
		GivenCount:  len(given),
		Explanation: explainShare(cs, len(given)),
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
	if cs.Mode == ShareSince {
		if bm := st.Message(cs.BoundaryMessageID); bm == nil {
			writeAPIError(w, http.StatusBadRequest, "CONTEXT_BOUNDARY_NOT_FOUND",
				fmt.Sprintf("boundary message %q does not exist in session %q", cs.BoundaryMessageID, sess.ID))
			return
		}
	}
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
