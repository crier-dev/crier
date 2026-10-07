// audit_http.go — the HTTP surface of the audit + permission reads
// (CR-CHAT-021, specs/CHAT-INTERFACE.md §4 rows 21, 22, 23).
//
// Three routes, all READ-ONLY, all realm-scoped through the same loadScoped /
// authorizeRead convention every other session read uses:
//
//	GET /sessions/{id}/audit           — the closed-vocabulary audit trail
//	GET /sessions/{id}/permissions     — the permission matrix
//	GET /sessions/{id}/roles           — the per-principal role badge rows
//
// The routes exist only over the session store's OWN records; nothing is
// written by a GET here (see audit.go for the read-only posture statement).
package session

import (
	"net/http"
	"time"

	"github.com/gorilla/mux"
)

// AuditTrail is the GET /sessions/{id}/audit body.
type AuditTrail struct {
	SessionID string `json:"session_id"`
	// Vocabulary is the CLOSED event set this endpoint can return. A client
	// branches on it; the endpoint refuses to render anything outside it.
	Vocabulary []AuditEventKind `json:"vocabulary"`
	Events     []AuditEvent     `json:"events"`
	Count      int              `json:"count"`
}

// permissionMatrixResponse is the GET /sessions/{id}/permissions body
// (row 23): the column labels and one row per active participant.
type permissionMatrixResponse struct {
	SessionID string      `json:"session_id"`
	Actions   []string    `json:"actions"`
	Rows      []matrixRow `json:"rows"`
	Count     int         `json:"count"`
}

// roleBadgesResponse is the GET /sessions/{id}/roles body (row 22).
type roleBadgesResponse struct {
	SessionID string      `json:"session_id"`
	Roles     []roleBadge `json:"roles"`
}

// roleBadge is one principal's role pill (row 22): the role recorded on the
// membership event. The `member` default is applied and RECORDED at join time
// (§2.1: every member's add record names exactly one role, so the record
// validates) — the default is therefore part of the durable event, not a
// client-side substitution, and a badge never guesses.
type roleBadge struct {
	MemberType string     `json:"member_type"`
	MemberID   string     `json:"member_id"`
	Role       MemberRole `json:"role"`
	// Active mirrors the participant list: a removed member keeps its badge
	// but is not active.
	Active bool `json:"active"`
}

// roleBadgeRows renders the row-22 badges from a session's membership.
func roleBadgeRows(st *State, now time.Time) []roleBadge {
	out := make([]roleBadge, 0, len(st.Members))
	for _, m := range st.Members {
		role := m.Role
		if role == "" {
			// A legacy record written before the role was required: the
			// badge applies the default and the READ says so via the same
			// default the join would have recorded.
			role = RoleMember
		}
		out = append(out, roleBadge{
			MemberType: string(m.MemberType),
			MemberID:   m.MemberID,
			Role:       role,
			Active:     m.ActiveAt(now),
		})
	}
	return out
}

// PermissionsGrantView is one grant as the audit/matrix surfaces read it.
// It mirrors permissions.Grant's fields this package needs without importing
// the store implementation. Exported because cmd/server maps the CR-CHAT-003
// store's grants onto it when wiring HTTPOptions.GrantEvents.
type PermissionsGrantView struct {
	ID        string     `json:"id"`
	Principal string     `json:"principal"`
	Actions   []string   `json:"actions"`
	GrantedBy string     `json:"granted_by"`
	GrantedAt time.Time  `json:"granted_at"`
	RevokedBy string     `json:"revoked_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// HandleSessionAudit serves GET /sessions/{id}/audit — row 21's Time /
// Principal / Event / Details panel.
func (h *Handler) HandleSessionAudit(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}

	// The trail is served by the store's audit source when it has one (both
	// backends do): the JSONL log renders its full record stream, the SQL
	// view its projected membership/lifecycle tables. Read-only either way.
	var events []AuditEvent
	if src, ok := h.store.(AuditEventSource); ok {
		var err error
		events, err = src.AuditEvents(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
	} else {
		recs, err := h.store.Records(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		// A record type that is neither audit nor transcript is a version
		// skew: refused loudly, never skipped (§5.1's discipline).
		events, err = RenderAuditEvents(recs)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "AUDIT_RENDER_REFUSED", err.Error())
			return
		}
	}

	// Permission events overlay from the ACL when it is wired and holds
	// grants on this session's subject. Read-only: the panel shows what the
	// permission log already recorded, including tombstones.
	if h.opts.GrantEvents != nil {
		grants, err := h.opts.GrantEvents(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		for _, g := range grants {
			events = append(events, grantAuditEvent(g))
		}
		sortAuditEvents(events)
	}

	out := AuditTrail{
		SessionID:  sess.ID,
		Vocabulary: AuditVocabulary,
		Events:     events,
		Count:      len(events),
	}
	writeJSON(w, http.StatusOK, out)
}

// grantAuditEvent renders one grant record as the audit events it produced: a
// live grant is permission.grant; a tombstoned one is BOTH (the grant when it
// was made, the revoke when it died) — an audit trail that lost the grant line
// when the revoke landed would not be able to answer "who held this".
func grantAuditEvent(g PermissionsGrantView) AuditEvent {
	ev := AuditEvent{
		Kind:           AuditPermissionGrant,
		TS:             g.GrantedAt,
		Actor:          AuthorRef{Principal: g.GrantedBy},
		GrantPrincipal: g.Principal,
		GrantID:        g.ID,
		GrantedBy:      g.GrantedBy,
		GrantedAt:      tsPtr(g.GrantedAt),
		GrantActions:   append([]string(nil), g.Actions...),
	}
	if g.RevokedAt != nil {
		ev.Kind = AuditPermissionRevoke
		ev.RevokedBy = g.RevokedBy
		revokedAt := *g.RevokedAt
		ev.RevokedAt = &revokedAt
	}
	return ev
}

// sortAuditEvents orders by ts, then seq (0 for permission events), so the
// panel reads chronologically when the two sources merge.
func sortAuditEvents(events []AuditEvent) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0; j-- {
			a, b := events[j-1], events[j]
			if a.TS.Before(b.TS) || (a.TS.Equal(b.TS) && a.Seq <= b.Seq) {
				break
			}
			events[j-1], events[j] = events[j], events[j-1]
		}
	}
}

func tsPtr(t time.Time) *time.Time { return &t }

// HandleSessionPermissions serves GET /sessions/{id}/permissions — row 23's
// matrix: send / read / invite / admin columns against the session's active
// principals.
func (h *Handler) HandleSessionPermissions(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	rows := h.matrixRows(r, st)
	writeJSON(w, http.StatusOK, permissionMatrixResponse{
		SessionID: sess.ID,
		Actions:   matrixActionLabels(),
		Rows:      rows,
		Count:     len(rows),
	})
}

// HandleSessionRoles serves GET /sessions/{id}/roles — row 22's role badges:
// the per-principal role recorded on membership, with its stated default.
func (h *Handler) HandleSessionRoles(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	out := roleBadgesResponse{SessionID: sess.ID, Roles: roleBadgeRows(st, h.now())}
	writeJSON(w, http.StatusOK, out)
}

// matrixRows builds the matrix rows, overlaying live grants when the ACL is
// wired. A store error from the ACL is not a matrix error: the matrix is a
// read of what IS, and a grant overlay that cannot be read degrades to the
// role bundle alone rather than failing the read.
func (h *Handler) matrixRows(r *http.Request, st *State) []matrixRow {
	var grants []PermissionsGrantView
	if h.opts.GrantEvents != nil {
		// A permission-store read error degrades to role-bundle truth, never
		// to a 500: the matrix still states the roles it can see. The error
		// is dropped deliberately — there is no cell that could carry it.
		grants, _ = h.opts.GrantEvents(r.Context(), st.Session.ID)
	}
	return BuildPermissionMatrix(st, h.now(), grants)
}

// matrixActionLabels returns the column labels in MatrixActions order.
func matrixActionLabels() []string {
	out := make([]string, 0, len(MatrixActions))
	for _, a := range MatrixActions {
		out = append(out, string(a))
	}
	return out
}

// RegisterAuditRoutes wires the three read routes. cmd/server calls it beside
// the other session registrations.
func RegisterAuditRoutes(r *mux.Router, h *Handler) {
	r.HandleFunc("/sessions/{id}/audit", h.HandleSessionAudit).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/permissions", h.HandleSessionPermissions).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/roles", h.HandleSessionRoles).Methods(http.MethodGet)
}
