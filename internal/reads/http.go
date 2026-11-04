// http.go — the read-state HTTP surface (spec §3.1):
//
//	POST /sessions/{id}/reads         mark a read cursor
//	GET  /sessions/{id}/reads         who has read what (participants' rows)
//	GET  /sessions/{id}/reads/unread  the unread messages for one member
//
// The handlers live in this package rather than internal/session because the
// dependency runs one way: reads consumes session.State, and having session
// import reads would close the cycle. Everything the handlers need from the
// session surface is behind SessionAccess (one exported accessor on the session
// Handler), so the realm wall and the session's visibility rule are still
// decided in exactly ONE place.
//
// Identity is the shipped convention the other session reads use
// (session.CallerIdentity): the X-Agent-ID header for an agent, principal_id /
// X-Crier-Principal with an optional as_agent for a human. A caller that names
// no identity at all is the trust-by-reach posture the rest of the surface
// keeps when no ACL is armed — it may mark for the member it names and may read
// every row, because it could already read the whole transcript. With an ACL
// armed (or an operator scope configured) an anonymous caller gets nothing but
// what it can prove, which is its own (empty) row.
package reads

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/session"
	"github.com/crier-dev/crier/internal/strictjson"
)

// maxBodyBytes bounds a mark request body. A mark carries a member identity,
// one seq and a short id list; 64 KiB is generous and a larger body is refused
// rather than buffered.
const maxBodyBytes = 64 << 10

// SessionAccess is the session surface the read-state handlers need: resolve
// the session in the caller's realm (404 for another realm, never 403) and
// apply the session's own read-visibility rule, writing its refusal itself.
// *session.Handler satisfies it.
type SessionAccess interface {
	ReadStateAccess(w http.ResponseWriter, r *http.Request) (*session.State, *session.Session, bool)
}

// HTTPHandler serves the three read-state routes.
type HTTPHandler struct {
	svc    *Service
	access SessionAccess
	auth   *Authorizer
	now    func() time.Time
}

// NewHTTPHandler builds the read-state HTTP handler. access and auth may be
// nil: a nil access makes every request a 503 (the surface is not wired), and a
// nil auth is an unarmed deployment with no operators.
func NewHTTPHandler(svc *Service, access SessionAccess, auth *Authorizer) *HTTPHandler {
	return &HTTPHandler{
		svc:    svc,
		access: access,
		auth:   auth,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// markReadRequest is the POST /sessions/{id}/reads body (§3.1). Unknown fields
// are refused: a misspelled `read_through_seq` must not silently mean "read
// everything".
type markReadRequest struct {
	MemberType     string   `json:"member_type"`
	MemberID       string   `json:"member_id"`
	ReadThroughSeq int64    `json:"read_through_seq"`
	MessageIDs     []string `json:"message_ids"`
}

// markReadResponse is the POST 200 body (§3.1).
type markReadResponse struct {
	MemberType     string     `json:"member_type"`
	MemberID       string     `json:"member_id"`
	ReadThroughSeq int64      `json:"read_through_seq"`
	Clamped        bool       `json:"clamped"`
	Unread         int64      `json:"unread"`
	UpdatedAt      *time.Time `json:"updated_at"`
}

// readsResponse is the GET /sessions/{id}/reads body (§3.1).
type readsResponse struct {
	Members []memberReadRow `json:"members"`
	LastSeq int64           `json:"last_seq"`
}

// memberReadRow is one participant's read state. `last_read_at` is null for a
// member that has never marked anything — never 0 and never the session's
// creation time (§7: absent means 0, and the response says so honestly).
type memberReadRow struct {
	MemberType     string     `json:"member_type"`
	MemberID       string     `json:"member_id"`
	ReadThroughSeq int64      `json:"read_through_seq"`
	Unread         int64      `json:"unread"`
	LastReadAt     *time.Time `json:"last_read_at"`
}

// unreadResponse is the GET /sessions/{id}/reads/unread body (§3.1).
type unreadResponse struct {
	Unread []unreadMessage `json:"unread"`
	Count  int             `json:"count"`
}

// unreadMessage is one unread transcript entry. `tags` is present (possibly
// empty) because the contract names it; see messageTags for why it is empty in
// this tree (CRIER-TAGS-1 not landed).
type unreadMessage struct {
	MessageID string            `json:"message_id"`
	Seq       int64             `json:"seq"`
	Tags      []string          `json:"tags"`
	Author    session.AuthorRef `json:"author"`
}

// HandleMarkRead serves POST /sessions/{id}/reads.
func (h *HTTPHandler) HandleMarkRead(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil || h.access == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "READS_UNCONFIGURED",
			"the read-cursor service is not wired in this deployment")
		return
	}
	if _, _, ok := h.access.ReadStateAccess(w, r); !ok {
		return
	}
	var req markReadRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.MemberType = strings.TrimSpace(req.MemberType)
	req.MemberID = strings.TrimSpace(req.MemberID)
	if req.MemberType == "" || req.MemberID == "" {
		writeAPIError(w, http.StatusBadRequest, "READS_MEMBER_REQUIRED",
			"member_type and member_id are required: a read receipt says who read what")
		return
	}
	if !h.authorizeMark(w, r, req.MemberType, req.MemberID) {
		return
	}
	cursor, unread, clamped, err := h.svc.MarkClamped(r.Context(), muxVar(r, "id"),
		req.MemberType, req.MemberID, req.ReadThroughSeq, req.MessageIDs)
	if err != nil {
		writeReadsError(w, err)
		return
	}
	out := markReadResponse{
		MemberType:     cursor.MemberType,
		MemberID:       cursor.MemberID,
		ReadThroughSeq: cursor.ReadThroughSeq,
		Clamped:        clamped,
		Unread:         unread,
	}
	if !cursor.UpdatedAt.IsZero() {
		at := cursor.UpdatedAt.UTC()
		out.UpdatedAt = &at
	}
	writeJSON(w, http.StatusOK, out)
}

// HandleReadState serves GET /sessions/{id}/reads.
func (h *HTTPHandler) HandleReadState(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil || h.access == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "READS_UNCONFIGURED",
			"the read-cursor service is not wired in this deployment")
		return
	}
	if _, _, ok := h.access.ReadStateAccess(w, r); !ok {
		return
	}
	rows, last, err := h.svc.StateRows(r.Context(), muxVar(r, "id"))
	if err != nil {
		writeReadsError(w, err)
		return
	}
	caller := session.CallerIdentity(r)
	privileged := h.holds(w, r, PermReadAll, caller, muxVar(r, "id"))
	all := privileged || (anonymous(caller) && !h.auth.Armed())

	out := readsResponse{Members: make([]memberReadRow, 0, len(rows)), LastSeq: last}
	for _, row := range rows {
		// §3.1: a caller without session.read_state.all sees only its own row
		// — the rest are OMITTED, never zeroed, so a client cannot mistake
		// "hidden" for "has read nothing".
		if !all && !isCaller(caller, row.Cursor.MemberType, row.Cursor.MemberID) {
			continue
		}
		view := memberReadRow{
			MemberType:     row.Cursor.MemberType,
			MemberID:       row.Cursor.MemberID,
			ReadThroughSeq: row.Cursor.ReadThroughSeq,
			Unread:         row.Unread,
		}
		if row.HasCursor && !row.Cursor.UpdatedAt.IsZero() {
			at := row.Cursor.UpdatedAt.UTC()
			view.LastReadAt = &at
		}
		out.Members = append(out.Members, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// HandleUnread serves GET /sessions/{id}/reads/unread?member_id=alpha&tag=build.
func (h *HTTPHandler) HandleUnread(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil || h.access == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "READS_UNCONFIGURED",
			"the read-cursor service is not wired in this deployment")
		return
	}
	if _, _, ok := h.access.ReadStateAccess(w, r); !ok {
		return
	}
	caller := session.CallerIdentity(r)
	memberID := strings.TrimSpace(r.URL.Query().Get("member_id"))
	if memberID == "" {
		memberID = callerID(caller)
	}
	if memberID == "" {
		writeAPIError(w, http.StatusBadRequest, "READS_MEMBER_REQUIRED",
			"member_id is required (or name the caller with X-Agent-ID): unread is asked about one member")
		return
	}
	msgs, err := h.svc.Unread(r.Context(), muxVar(r, "id"), memberID, tagFilter(r))
	if err != nil {
		writeReadsError(w, err)
		return
	}
	out := unreadResponse{Unread: make([]unreadMessage, 0, len(msgs)), Count: len(msgs)}
	for i := range msgs {
		m := msgs[i]
		tags := messageTags(&m)
		if tags == nil {
			tags = []string{}
		}
		sort.Strings(tags)
		out.Unread = append(out.Unread, unreadMessage{
			MessageID: m.ID,
			Seq:       m.Seq,
			Tags:      tags,
			Author:    m.Author,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Authorization decisions.
// ---------------------------------------------------------------------------

// authorizeMark applies §3.1/§6's "marking on behalf of another member" rule.
//
// A caller marking ITSELF needs no privilege: a read receipt is a claim about
// its own progress. A caller marking SOMEONE ELSE needs
// session.read_state.write_all. A caller that names no identity at all is the
// trust-by-reach posture — allowed while nothing is armed, refused
// READS_FOR_OTHER_MEMBER_DENIED once an ACL or an operator scope exists.
func (h *HTTPHandler) authorizeMark(w http.ResponseWriter, r *http.Request, memberType, memberID string) bool {
	caller := session.CallerIdentity(r)
	if isCaller(caller, memberType, memberID) {
		return true
	}
	if anonymous(caller) {
		if !h.auth.Armed() {
			return true
		}
		writeAPIError(w, http.StatusForbidden, "READS_FOR_OTHER_MEMBER_DENIED",
			"marking another member's read cursor requires session.read_state.write_all, and an anonymous caller cannot prove it speaks for itself")
		return false
	}
	if h.holds(w, r, PermWriteAll, caller, muxVar(r, "id")) {
		return true
	}
	writeAPIError(w, http.StatusForbidden, "READS_FOR_OTHER_MEMBER_DENIED",
		"marking another member's read cursor requires session.read_state.write_all (a read receipt is a claim about one's own progress, §1)")
	return false
}

// holds answers one read-state permission question, treating a store failure as
// a refusal with its own code rather than a silent "no".
func (h *HTTPHandler) holds(w http.ResponseWriter, r *http.Request, perm Permission, caller session.AuthorRef, sessionID string) bool {
	ok, err := h.auth.Holds(r.Context(), perm, caller, sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "READS_STORE_ERROR", err.Error())
		return false
	}
	return ok
}

// isCaller reports whether the identity a request names IS the member a row
// describes, typed so an agent id and a principal id that collide are not the
// same person.
func isCaller(caller session.AuthorRef, memberType, memberID string) bool {
	if memberID == "" {
		return false
	}
	switch session.MemberType(memberType) {
	case session.MemberAgent:
		return caller.Agent == memberID || caller.AsAgent == memberID
	case session.MemberPrincipal:
		return caller.Principal == memberID
	}
	return false
}

func callerID(caller session.AuthorRef) string {
	if caller.Agent != "" {
		return caller.Agent
	}
	if caller.AsAgent != "" {
		return caller.AsAgent
	}
	return caller.Principal
}

func anonymous(caller session.AuthorRef) bool {
	return caller.Agent == "" && caller.Principal == "" && caller.AsAgent == "" && caller.System == ""
}

// tagFilter reads the ?tag= filter. The parameter may repeat and may carry a
// comma-separated list, so both `?tag=build&tag=ci` and `?tag=build,ci` mean
// the same thing.
func tagFilter(r *http.Request) []string {
	var out []string
	for _, raw := range r.URL.Query()["tag"] {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Errors and plumbing.
// ---------------------------------------------------------------------------

// writeReadsError maps the service's sentinels onto the §6 catalog. It is the
// ONLY place the status/code pair is decided.
func writeReadsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMemberRequired):
		writeAPIError(w, http.StatusBadRequest, "READS_MEMBER_REQUIRED", err.Error())
	case errors.Is(err, ErrSeqInvalid):
		writeAPIError(w, http.StatusBadRequest, "READS_SEQ_INVALID", err.Error())
	case errors.Is(err, ErrMemberNotParticipant):
		writeAPIError(w, http.StatusForbidden, "READS_MEMBER_NOT_PARTICIPANT", err.Error())
	case errors.Is(err, ErrMessageNotInSession):
		writeAPIError(w, http.StatusNotFound, "READS_MESSAGE_NOT_IN_SESSION", err.Error())
	case errors.Is(err, ErrRewindRefused):
		writeAPIError(w, http.StatusConflict, "READS_REWIND_REFUSED", err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "SESSION_NOT_FOUND", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "READS_STORE_ERROR", err.Error())
	}
}

// decodeStrict decodes one JSON body with unknown fields refused, answering
// false having already written the 400.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "could not read the request body")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "a JSON body is required")
		return false
	}
	if err := strictjson.Decode(strings.NewReader(string(body)), dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// muxVar reads one path variable, trimmed.
func muxVar(r *http.Request, name string) string {
	return strings.TrimSpace(mux.Vars(r)[name])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}
