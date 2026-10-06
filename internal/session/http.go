// http.go — the session API surface (CR-CHAT-019).
//
// It implements specs/CHAT-INTERFACE.md §4's core object rows — the session
// list (rows 8, 9), session creation (row 10), the session object (rows 11,
// 12, 13), the ordered cross-agent transcript (row 14, and the ancestry rows
// 42, 43), membership (rows 24, 37) and session fan-out (row 31) — as a
// CLIENT of the shipped primitives:
//
//   - the session data model and its ordering authority live in
//     internal/session (CR-CHAT-002/005), so every read here is the same
//     State the JSONL log and the SQL view already reduce to;
//   - a message reaches its recipients through the SHIPPED inbox path —
//     session.Fanout issues one delivery per participant through
//     registry.Store.Deliver — so there is no second delivery path (D1, §3.4);
//   - every surface is REALM-SCOPED (§4 row 38): a session carries the
//     namespace it was created in, its reads and writes resolve the same
//     X-Crier-Namespace / X-Crier-Namespace-Token convention the relay and the
//     registry use, and a target in another realm is refused rather than
//     fanned out.
//
// The handler depends on interfaces, not on the registry handler, because
// internal/session imports internal/registry (for InboxEntry) and the reverse
// would be an import cycle. This file adds no route of its own: cmd/server
// registers the methods, exactly as it does for the dagger control surface.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
)

// AgentLookup is the one registry read this surface needs beyond delivery: the
// stored row of a member agent, whose namespace is the authority for whether a
// fan-out would cross a realm boundary (§4 row 38).
type AgentLookup interface {
	Get(id string) (*registry.Agent, error)
}

// HTTPOptions wires the optional collaborators of the session surface. The
// zero value is a valid, usable handler over the default realm with no
// permissions deployment — which is exactly the shipped posture.
type HTTPOptions struct {
	// Deliverer is the shipped durable-inbox write every fan-out uses. Nil
	// means fan-out is unconfigured and a send is refused with
	// 503 SESSION_DELIVERY_UNCONFIGURED rather than silently dropped.
	Deliverer InboxDeliverer
	// Agents resolves a member agent's stored namespace so a fan-out cannot
	// leave its realm. Nil skips the realm cross-check (every target is
	// treated as in-realm) — the pre-CR-FEAT-029 posture.
	Agents AgentLookup
	// Namespaces is the realm policy set. Nil is one implicit namespace,
	// exactly as the registry handler treats it.
	Namespaces *namespace.Registry
	// Permissions is the CR-CHAT-003 delivery ACL. Nil (the default) means
	// the ACL is not deployed; a restricted session's reads then fall back to
	// membership as the only evidence available.
	Permissions *permissions.Checker
	// Groups is the NAMED-group roster store (CR-CHAT-013, §1.4). Nil means
	// the group surface is unconfigured: the routes answer 503
	// GROUPS_UNCONFIGURED and a `group` audience target on a send is a
	// RECORDED skip (FanoutRecipients), never a guessed delivery.
	Groups GroupStore
}

// Handler serves the session API. It is safe for concurrent callers when its
// store is.
type Handler struct {
	store Repository
	opts  HTTPOptions
	now   func() time.Time
	newID func() (string, error)
}

// NewHTTPHandler builds a session API handler over a Repository.
func NewHTTPHandler(store Repository, opts HTTPOptions) *Handler {
	return &Handler{
		store: store,
		opts:  opts,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newHexID,
	}
}

// newHexID mints a path-safe opaque id (the JSONL store refuses anything but
// [A-Za-z0-9._-] in a session id, and a message id travels as one).
func newHexID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// The wire shapes (§4 rows 11-15, 24).
// ---------------------------------------------------------------------------

// sessionView is the session OBJECT (row 11): title, id, start timestamp,
// audience count, lifecycle state and visibility, plus the counts the list
// rows (8, 9) and the header render.
type sessionView struct {
	ID        string     `json:"id"`
	Namespace string     `json:"namespace,omitempty"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy AuthorRef  `json:"created_by"`
	State     string     `json:"state"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	// Visibility is the session's privacy field (row 13). "" and "realm" are
	// the realm default; "private" restricts reads (see authorizeRead).
	Visibility string `json:"visibility,omitempty"`
	// AudienceCount is the number of ACTIVE AGENT participants — the size of
	// the fan-out set a send resolves to (§3.4 rule 6: cost is visible).
	AudienceCount int `json:"audience_count"`
	// ParticipantCount is every active participant, agents and principals.
	ParticipantCount int        `json:"participant_count"`
	MessageCount     int        `json:"message_count"`
	ThreadCount      int        `json:"thread_count"`
	LastMessageAt    *time.Time `json:"last_message_at,omitempty"`
	Preview          string     `json:"preview,omitempty"`
}

// sessionsResponse is the GET /sessions body.
type sessionsResponse struct {
	Sessions []sessionView `json:"sessions"`
	Count    int           `json:"count"`
}

// transcriptMessage is one transcript entry (row 14) with the ancestry the
// branch/timeline rows (42, 43) render: a resolvable parent and the thread it
// belongs to.
type transcriptMessage struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	ParentID string `json:"parent_id,omitempty"`
	// ParentResolvable is true when parent_id names a message in this
	// transcript, or when the message is a thread root (no parent). A false
	// value is the §4.3 broken-thread case, reported not hidden.
	ParentResolvable bool            `json:"parent_resolvable"`
	RootID           string          `json:"root_id,omitempty"`
	Seq              int64           `json:"seq"`
	MessageKind      string          `json:"message_kind"`
	Author           AuthorRef       `json:"author"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Audience         Audience        `json:"audience"`
	Outcomes         []outcomeView   `json:"outcomes,omitempty"`
	IdempotencyKey   string          `json:"idempotency_key,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	// ThreadDepth is the thread tree's level (a deliberate branch adds one);
	// ReplyDepth is the parent_id chain length (reply attribution, never
	// depth — D11).
	ThreadDepth int `json:"thread_depth"`
	ReplyDepth  int `json:"reply_depth"`
}

// outcomeView is one DeliveryOutcome as this API renders it. It carries the
// persisted vocabulary plus, when the caller is being told about a refusal in
// the response to their OWN send, the immediate reason: DeliveryOutcome.Reason
// is `json:"-"` because the durable shape has no reason column (§5.2, §3.2),
// so a refusal that is not explained here would be shown only as a bare
// `refused`. On a re-read from storage the detail is empty and the outcome is
// the durable fact.
type outcomeView struct {
	Target       string    `json:"target"`
	Outcome      string    `json:"outcome"`
	InboxEntryID string    `json:"inbox_entry_id,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func outcomeViews(outcomes []DeliveryOutcome) []outcomeView {
	if len(outcomes) == 0 {
		return nil
	}
	out := make([]outcomeView, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, outcomeView{
			Target:       o.Target,
			Outcome:      string(o.Outcome),
			InboxEntryID: o.InboxEntryID,
			Detail:       o.Reason,
			UpdatedAt:    o.UpdatedAt,
		})
	}
	return out
}

// transcriptResponse is the GET /sessions/{id}/messages body.
type transcriptResponse struct {
	Session  sessionView         `json:"session"`
	Messages []transcriptMessage `json:"messages"`
	Count    int                 `json:"count"`
	// Findings is what the reconstruction had to decide about a thread key —
	// a derived or broken thread is reported, never hidden (§4.3).
	Findings []ThreadFinding `json:"findings,omitempty"`
}

// participantView is one membership row (row 24). Removed members are returned
// with Active false rather than erased: membership is a recorded event, not a
// mutable set (§2.3, §2.4).
type participantView struct {
	MemberType   string        `json:"member_type"`
	MemberID     string        `json:"member_id"`
	Role         string        `json:"role"`
	AddedAt      time.Time     `json:"added_at"`
	RemovedAt    *time.Time    `json:"removed_at,omitempty"`
	Active       bool          `json:"active"`
	ContextShare *ContextShare `json:"context_share,omitempty"`
}

// participantsResponse is the GET /sessions/{id}/participants body.
type participantsResponse struct {
	SessionID    string            `json:"session_id"`
	Participants []participantView `json:"participants"`
	Count        int               `json:"count"`
}

// createSessionRequest is the POST /sessions body (§1.4).
type createSessionRequest struct {
	// ID is optional; absent mints one.
	ID               string                  `json:"id,omitempty"`
	Title            string                  `json:"title,omitempty"`
	Kind             string                  `json:"kind,omitempty"`
	Namespace        string                  `json:"namespace,omitempty"`
	Visibility       string                  `json:"visibility,omitempty"`
	RetentionSeconds *int                    `json:"retention_seconds,omitempty"`
	CreatedBy        *AuthorRef              `json:"created_by,omitempty"`
	PrincipalID      string                  `json:"principal_id,omitempty"`
	AsAgent          string                  `json:"as_agent,omitempty"`
	Members          []addParticipantRequest `json:"members,omitempty"`
}

// addParticipantRequest is the POST /sessions/{id}/participants body.
type addParticipantRequest struct {
	MemberType   string        `json:"member_type"`
	MemberID     string        `json:"member_id"`
	Role         string        `json:"role,omitempty"`
	ContextShare *ContextShare `json:"context_share,omitempty"`
	Actor        string        `json:"actor,omitempty"`
}

// postMessageRequest is the POST /sessions/{id}/messages body: composing INTO
// a session (row 31). The request body is the deliver body's fields plus the
// audience controls the session adds.
type postMessageRequest struct {
	Payload        json.RawMessage  `json:"payload"`
	Sender         string           `json:"sender,omitempty"`
	PrincipalID    string           `json:"principal_id,omitempty"`
	AsAgent        string           `json:"as_agent,omitempty"`
	MessageKind    string           `json:"message_kind,omitempty"`
	ThreadID       string           `json:"thread_id,omitempty"`
	ParentID       string           `json:"parent_id,omitempty"`
	Targets        []AudienceTarget `json:"targets,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	RequestID      string           `json:"request_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Declaration, derived from the store's own contract.
// ---------------------------------------------------------------------------

var (
	_ Repository = (*JSONLStore)(nil)
	_ Repository = (*SQLStore)(nil)
	_ Repository = (*PostgresStore)(nil)
)

// ---------------------------------------------------------------------------
// GET /sessions — the list, a VIEW over the same session objects (rows 8, 9).
// ---------------------------------------------------------------------------

// HandleListSessions serves GET /sessions. Filters:
//
//	state            open | closed       — lifecycle (row 12)
//	kind             channel | direct    — the channel/DM narrowing (row 39)
//	min_messages     N                   — count filter (row 9)
//	min_participants N                   — count filter (row 9)
//
// The realm is the request's namespace (X-Crier-Namespace), so the list can
// never show a session from another realm.
func (h *Handler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}

	q := r.URL.Query()
	stateFilter := SessionState(strings.TrimSpace(q.Get("state")))
	kindFilter := Kind(strings.TrimSpace(q.Get("kind")))
	minMessages, err := intQuery(q.Get("min_messages"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "min_messages must be a non-negative integer")
		return
	}
	minParticipants, err := intQuery(q.Get("min_participants"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "min_participants must be a non-negative integer")
		return
	}
	switch stateFilter {
	case "", SessionOpen, SessionClosed:
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("state must be %q or %q", SessionOpen, SessionClosed))
		return
	}
	switch kindFilter {
	case "", KindChannel, KindDirect:
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("kind must be %q or %q", KindChannel, KindDirect))
		return
	}

	ids, err := h.store.Sessions(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}

	out := sessionsResponse{Sessions: make([]sessionView, 0, len(ids))}
	for _, id := range ids {
		st, err := h.store.Load(r.Context(), id)
		if err != nil {
			// A session id the store lists but cannot load is a real
			// inconsistency; it is surfaced rather than skipped so the count
			// is never quietly wrong.
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR",
				fmt.Sprintf("load session %q: %v", id, err))
			return
		}
		if st.Session == nil || namespace.Canonical(st.Session.Namespace) != realm {
			continue
		}
		view := h.viewOf(st)
		if stateFilter != "" && SessionState(view.State) != stateFilter {
			continue
		}
		if kindFilter != "" && Kind(view.Kind) != kindFilter {
			continue
		}
		if view.MessageCount < minMessages || view.ParticipantCount < minParticipants {
			continue
		}
		out.Sessions = append(out.Sessions, view)
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].ID < out.Sessions[j].ID })
	out.Count = len(out.Sessions)
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /sessions — create a room (row 10, §1.4).
// ---------------------------------------------------------------------------

// HandleCreateSession serves POST /sessions. Creation records its creator
// (§1.4: a request that cannot name one is refused, never defaulted to a
// service identity) and, optionally, initial membership — but creating does
// not imply inviting, so the creator is NOT added automatically.
func (h *Handler) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decodeBody(w, r, &req) {
		return
	}

	realm, err := h.realm(r, req.Namespace)
	if err != nil {
		writeRealmError(w, err)
		return
	}

	creator, ok := resolveAuthor(req.CreatedBy, "", req.PrincipalID, req.AsAgent)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "CREATED_BY_REQUIRED",
			"created_by (or principal_id) is required: a session records who created it (§1.4) and is never defaulted to a service identity")
		return
	}

	kind := Kind(strings.TrimSpace(req.Kind))
	switch kind {
	case "":
		kind = DefaultKind
	case KindChannel, KindDirect:
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_KIND",
			fmt.Sprintf("kind must be %q or %q", KindChannel, KindDirect))
		return
	}

	visibility := strings.TrimSpace(req.Visibility)
	if !validVisibility(visibility) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_VISIBILITY",
			fmt.Sprintf("visibility must be %q or %q (or absent for the realm default)", VisibilityRealm, VisibilityPrivate))
		return
	}

	// §3.5: a session may declare a SHORTER lifetime than its realm's
	// retention, never a longer one. The realm is the request's, so this is
	// the same policy the deliveries into it will get.
	if err := h.checkRetention(realm, req.RetentionSeconds); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_RETENTION", err.Error())
		return
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		id, err = h.newID()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
			return
		}
	}
	if _, err := trimSessionID(id); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if _, err := h.store.Load(r.Context(), id); err == nil {
		writeAPIError(w, http.StatusConflict, "SESSION_EXISTS", fmt.Sprintf("session %q already exists", id))
		return
	}

	sess := &Session{
		ID:               id,
		Namespace:        realm,
		Kind:             kind,
		Title:            req.Title,
		CreatedAt:        h.now(),
		CreatedBy:        creator,
		State:            SessionOpen,
		RetentionSeconds: req.RetentionSeconds,
		Visibility:       visibility,
	}
	if err := h.store.CreateSession(r.Context(), sess, 1); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}

	for _, m := range req.Members {
		if err := h.addMember(r.Context(), sess, m, creator.AgentID()); err != nil {
			writeAPIError(w, statusForMemberError(err), codeForMemberError(err), err.Error())
			return
		}
	}

	st, err := h.store.Load(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, h.viewOf(st))
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/messages — the ordered cross-agent transcript (row 14).
// ---------------------------------------------------------------------------

// HandleTranscript serves GET /sessions/{id}/messages: ALL participants'
// messages in seq order, each carrying a resolvable parent and its thread.
func (h *Handler) HandleTranscript(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}

	out := transcriptResponse{
		Session:  h.viewOf(st),
		Messages: make([]transcriptMessage, 0, len(st.Messages)),
		Findings: st.Findings,
	}
	for _, m := range st.Messages {
		out.Messages = append(out.Messages, h.messageViewOf(st, m))
	}
	out.Count = len(out.Messages)
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET/POST /sessions/{id}/participants — membership (rows 24, 37).
// ---------------------------------------------------------------------------

// HandleListParticipants serves GET /sessions/{id}/participants.
func (h *Handler) HandleListParticipants(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	out := participantsResponse{SessionID: sess.ID, Participants: make([]participantView, 0, len(st.Members))}
	now := h.now()
	for _, m := range st.Members {
		view := participantView{
			MemberType: string(m.MemberType),
			MemberID:   m.MemberID,
			Role:       string(m.Role),
			AddedAt:    m.AddedAt,
			RemovedAt:  m.RemovedAt,
			Active:     m.ActiveAt(now),
		}
		if mc := contextShareFor(st, m.MemberType, m.MemberID); mc != nil {
			view.ContextShare = &ContextShare{Mode: mc.Mode, BoundaryMessageID: mc.BoundaryMessageID}
		}
		if view.Active {
			out.Count++
		}
		out.Participants = append(out.Participants, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// HandleAddParticipant serves POST /sessions/{id}/participants — the
// "Add participants" affordance (row 37).
func (h *Handler) HandleAddParticipant(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if sess.State == SessionClosed {
		writeAPIError(w, http.StatusConflict, "SESSION_CLOSED",
			fmt.Sprintf("session %q is closed and admits no membership change (§1.3)", sess.ID))
		return
	}
	var req addParticipantRequest
	if !decodeBody(w, r, &req) {
		return
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = strings.TrimSpace(r.Header.Get(registry.HeaderAgentID))
	}
	if err := h.addMember(r.Context(), sess, req, actor); err != nil {
		writeAPIError(w, statusForMemberError(err), codeForMemberError(err), err.Error())
		return
	}

	// Return the member just recorded, read back from the store so the
	// response is the recorded fact, not the request echoed.
	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	for _, m := range st.Members {
		if m.MemberType == MemberType(req.MemberType) && m.MemberID == req.MemberID && m.ActiveAt(h.now()) {
			writeJSON(w, http.StatusCreated, participantView{
				MemberType: string(m.MemberType),
				MemberID:   m.MemberID,
				Role:       string(m.Role),
				AddedAt:    m.AddedAt,
				Active:     true,
			})
			return
		}
	}
	writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", "the member was recorded but could not be read back")
}

// ---------------------------------------------------------------------------
// POST /sessions/{id}/messages — fan-out into a session (row 31).
// ---------------------------------------------------------------------------

// HandlePostMessage serves POST /sessions/{id}/messages. It follows §3.2's
// ordering rule exactly: the transcript record is written FIRST as the intent
// (message id + resolved audience), the fan-out follows through the shipped
// inbox path, and each target's outcome is written back onto the same record.
func (h *Handler) HandlePostMessage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.loadOpenScoped(w, r)
	if !ok {
		return
	}
	var req postMessageRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if h.opts.Deliverer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SESSION_DELIVERY_UNCONFIGURED",
			"no inbox deliverer is wired, so a session message cannot be fanned out")
		return
	}
	if err := validatePayload(req.Payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_PAYLOAD", err.Error())
		return
	}

	author, ok := resolveAuthor(nil, req.Sender, req.PrincipalID, req.AsAgent)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "AUTHOR_REQUIRED",
			"sender (an agent id), or principal_id (optionally with as_agent), is required: a message records its author")
		return
	}
	kind := MessageKind(strings.TrimSpace(req.MessageKind))
	switch kind {
	case "":
		kind = MessagePlain
	case MessagePlain, MessageAddressed, MessageTask:
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_MESSAGE_KIND",
			fmt.Sprintf("message_kind must be %q, %q or %q", MessagePlain, MessageAddressed, MessageTask))
		return
	}

	msgID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	now := h.now()
	msg := &Message{
		ID:             msgID,
		ParentID:       strings.TrimSpace(req.ParentID),
		SessionID:      sess.ID,
		Kind:           kind,
		Author:         author,
		Payload:        req.Payload,
		CreatedAt:      now,
		IdempotencyKey: req.IdempotencyKey,
	}

	// Thread resolution: a root's thread is its own id; a reply stays in its
	// parent's thread (D11, §4.5) — resolved from the transcript, never
	// fabricated.
	if msg.ParentID != "" {
		st, err := h.store.Load(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		parent := st.Message(msg.ParentID)
		if parent == nil {
			writeAPIError(w, http.StatusBadRequest, "PARENT_NOT_FOUND",
				fmt.Sprintf("parent_id %q is not in session %q", msg.ParentID, sess.ID))
			return
		}
		if strings.TrimSpace(req.ThreadID) != "" && req.ThreadID != parent.ThreadID {
			writeAPIError(w, http.StatusBadRequest, "THREAD_MISMATCH",
				fmt.Sprintf("thread_id %q does not match parent %q's thread %q (a reply never moves out of its thread)", req.ThreadID, msg.ParentID, parent.ThreadID))
			return
		}
		msg.ThreadID = parent.ThreadID
	} else {
		if strings.TrimSpace(req.ThreadID) != "" && req.ThreadID != msgID {
			writeAPIError(w, http.StatusBadRequest, "THREAD_MISMATCH",
				fmt.Sprintf("a thread root's thread_id must be its own message id %q", msgID))
			return
		}
		msg.ThreadID = msgID
	}

	// Resolved audience (§4.2): the caller's explicit targets, else every
	// active participant except the author.
	aud, err := h.resolveAudience(r.Context(), sess, &req, author.AgentID())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_AUDIENCE", err.Error())
		return
	}

	view, ok := h.sendMessage(w, r, sess, msg, aud)
	if !ok {
		return
	}
	// The DURABLE record keeps the outcome vocabulary (the §5.2
	// chat_deliveries projection has no reason column), so the immediate
	// REFUSAL reason is carried on the response — this is the caller's
	// explanation, and §3.2 requires a refused outcome to be shown, never
	// swallowed.
	view.Outcomes = outcomeViews(msg.Outcomes)
	writeJSON(w, http.StatusCreated, view)
}

// sendMessage performs §3.2's ordering for ONE new message record: the
// transcript record is written FIRST as the intent (message id + resolved
// audience), the fan-out follows through the shipped inbox path, and each
// target's outcome is written back onto the SAME record. It is the one place
// that ordering lives, so a sender — a session message (CR-CHAT-019) or a
// compiled bundle (CR-CHAT-028) — cannot drift from it.
//
// It answers false having already written the error, and returns the read-back
// view of the recorded message with the outcome vocabulary attached.
func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request, sess *Session, msg *Message, aud Audience) (transcriptMessage, bool) {
	msg.Audience = aud

	seq, err := h.store.NextSeq(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	msg.Seq = seq

	// The roster of every `group` target is resolved HERE, fresh, at send
	// time (CR-CHAT-013, §1.4 consequence 1): a roster edit routes the NEXT
	// send to the CURRENT members, never a cached set. A nil store leaves
	// every group target a recorded skip.
	recipients, skipped := FanoutRecipients(aud, h.resolveGroupRoster(r.Context(), aud))
	_ = skipped // deliberately-not-fanned-out targets are recorded in the audience
	crossRealm, deliverable := h.partitionRealm(recipients, sess.Namespace, h.now())
	msg.Outcomes = crossRealm

	// 1. The transcript record, as the intent.
	if err := h.store.PostMessage(r.Context(), msg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}

	// 2. One delivery per participant through the shipped inbox path.
	outcomes, err := Fanout(r.Context(), h.opts.Deliverer, msg, deliverable)
	if err != nil {
		// The intent is durable and the outcomes are incomplete — §3.2's
		// visible, repairable state, not a lost message. Report it as a 502
		// so the caller can retry the same message id without a re-send.
		writeAPIError(w, http.StatusBadGateway, "FANOUT_INCOMPLETE",
			fmt.Sprintf("the transcript record is durable (message %q) but the fan-out did not complete: %v", msg.ID, err))
		return transcriptMessage{}, false
	}

	// 3. Write the outcomes back onto the same record (same seq, keep-LAST).
	msg.Outcomes = append(crossRealm, outcomes...)
	if err := h.store.PostMessage(r.Context(), msg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}

	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	recorded := st.Message(msg.ID)
	if recorded == nil {
		recorded = msg
	}
	return h.messageViewOf(st, recorded), true
}

// ---------------------------------------------------------------------------
// Reconstruction helpers.
// ---------------------------------------------------------------------------

// viewOf reduces a State to the session object.
func (h *Handler) viewOf(st *State) sessionView {
	sess := st.Session
	v := sessionView{
		ID:           sess.ID,
		Namespace:    namespace.Canonical(sess.Namespace),
		Kind:         string(kindOrDefault(sess.Kind)),
		Title:        sess.Title,
		CreatedAt:    sess.CreatedAt,
		CreatedBy:    sess.CreatedBy,
		State:        string(stateOrDefault(sess.State)),
		ClosedAt:     sess.ClosedAt,
		Visibility:   sess.Visibility,
		MessageCount: len(st.Messages),
		ThreadCount:  len(st.Threads),
	}
	now := h.now()
	for _, m := range st.Members {
		if !m.ActiveAt(now) {
			continue
		}
		v.ParticipantCount++
		if m.MemberType == MemberAgent {
			v.AudienceCount++
		}
	}
	if n := len(st.Messages); n > 0 {
		last := st.Messages[n-1]
		at := last.CreatedAt
		v.LastMessageAt = &at
		v.Preview = payloadPreview(last.Payload)
	}
	return v
}

// messageViewOf renders one transcript message with its ancestry.
func (h *Handler) messageViewOf(st *State, m *Message) transcriptMessage {
	v := transcriptMessage{
		ID:             m.ID,
		ThreadID:       m.ThreadID,
		ParentID:       m.ParentID,
		Seq:            m.Seq,
		MessageKind:    string(m.Kind),
		Author:         m.Author,
		Payload:        m.Payload,
		Audience:       m.Audience,
		Outcomes:       outcomeViews(m.Outcomes),
		IdempotencyKey: m.IdempotencyKey,
		CreatedAt:      m.CreatedAt,
		ThreadDepth:    st.ThreadDepth(m.ThreadID),
	}
	v.ParentResolvable = m.ParentID == "" || st.Message(m.ParentID) != nil
	if t := st.Thread(m.ThreadID); t != nil {
		v.RootID = t.RootMessageID
	}
	if chain, _ := st.ReplyChain(m.ID); len(chain) > 0 {
		v.ReplyDepth = len(chain) - 1
	}
	return v
}

// resolveAudience builds the resolved audience of a send (§4.2). An explicit
// target list is used as given; otherwise the audience is the session's active
// participants minus the author.
func (h *Handler) resolveAudience(ctx context.Context, sess *Session, req *postMessageRequest, authorAgent string) (Audience, error) {
	if len(req.Targets) > 0 {
		seen := map[string]bool{}
		targets := make([]AudienceTarget, 0, len(req.Targets))
		for _, t := range req.Targets {
			if t.ID == "" {
				return Audience{}, errors.New("every audience target needs an id")
			}
			key := string(t.Kind) + "\x00" + t.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, t)
		}
		sort.SliceStable(targets, func(i, j int) bool {
			if targets[i].Kind != targets[j].Kind {
				return targets[i].Kind < targets[j].Kind
			}
			return targets[i].ID < targets[j].ID
		})
		return Audience{Rule: AudienceExplicit, Targets: targets}, nil
	}

	st, err := h.store.Load(ctx, sess.ID)
	if err != nil {
		return Audience{}, err
	}
	now := h.now()
	var targets []AudienceTarget
	for _, m := range st.Members {
		if !m.ActiveAt(now) {
			continue
		}
		if m.MemberType == MemberAgent && m.MemberID == authorAgent {
			continue
		}
		targets = append(targets, AudienceTarget{Kind: memberTargetKind(m.MemberType), ID: m.MemberID})
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		return targets[i].ID < targets[j].ID
	})
	return Audience{Rule: AudienceSession, Targets: targets}, nil
}

// partitionRealm splits the fan-out recipients into those whose stored agent
// row is in another realm (refused, never delivered — §4 row 38) and those that
// may receive the message. An unknown agent is left to the deliver path, which
// reports it as a refused outcome with its own reason.
func (h *Handler) partitionRealm(recipients []string, realm string, now time.Time) (crossRealm []DeliveryOutcome, deliverable []string) {
	for _, id := range recipients {
		if h.opts.Agents != nil {
			ag, err := h.opts.Agents.Get(id)
			if err == nil && ag != nil && namespace.Canonical(ag.Namespace) != realm {
				crossRealm = append(crossRealm, DeliveryOutcome{
					Target:    id,
					Outcome:   OutcomeRefused,
					Reason:    "target is registered in a different namespace than the session (NAMESPACE_MISMATCH)",
					UpdatedAt: now,
				})
				continue
			}
		}
		deliverable = append(deliverable, id)
	}
	return crossRealm, deliverable
}

// addMember records one membership event (§2.3). A join to a session in
// another realm is refused, and so is adding a KNOWN agent that lives in
// another realm: either would put two realms in one room.
func (h *Handler) addMember(ctx context.Context, sess *Session, req addParticipantRequest, actor string) error {
	mt := MemberType(strings.TrimSpace(req.MemberType))
	switch mt {
	case MemberAgent, MemberPrincipal:
	default:
		return &memberError{status: http.StatusBadRequest, code: "INVALID_MEMBER",
			msg: fmt.Sprintf("member_type must be %q or %q", MemberAgent, MemberPrincipal)}
	}
	id := strings.TrimSpace(req.MemberID)
	if id == "" {
		return &memberError{status: http.StatusBadRequest, code: "INVALID_MEMBER", msg: "member_id is required"}
	}
	role := MemberRole(strings.TrimSpace(req.Role))
	if role == "" {
		// §2.1: every member has exactly one role. The vocabulary is data
		// (CHAT-PERMISSIONS.md owns it), so an absent role takes the least
		// surprising default rather than inventing a new one.
		role = RoleMember
	}
	if req.ContextShare != nil {
		if err := validateContextShare(req.ContextShare); err != nil {
			return &memberError{status: http.StatusBadRequest, code: "INVALID_CONTEXT_SHARE", msg: err.Error()}
		}
	}
	if mt == MemberAgent && h.opts.Agents != nil {
		if ag, err := h.opts.Agents.Get(id); err == nil && ag != nil {
			if namespace.Canonical(ag.Namespace) != namespace.Canonical(sess.Namespace) {
				return &memberError{status: http.StatusConflict, code: "NAMESPACE_MISMATCH",
					msg: fmt.Sprintf("agent %q is in namespace %q, but session %q is in %q",
						id, namespace.Display(ag.Namespace), sess.ID, namespace.Display(sess.Namespace))}
			}
		}
	}

	seq, err := h.store.NextSeq(ctx, sess.ID)
	if err != nil {
		return err
	}
	m := &Member{
		SessionID:  sess.ID,
		MemberType: mt,
		MemberID:   id,
		Role:       role,
		AddedAt:    h.now(),
		AddedBy:    actor,
	}
	return h.store.AddMember(ctx, m, seq, req.ContextShare)
}

// ---------------------------------------------------------------------------
// Scoping, identity and validation.
// ---------------------------------------------------------------------------

// loadScoped resolves the realm, loads the session and refuses a session that
// belongs to another realm as 404 — never a 403, which would confirm that a
// session the caller cannot see exists.
func (h *Handler) loadScoped(w http.ResponseWriter, r *http.Request) (*State, *Session, bool) {
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return nil, nil, false
	}
	id := muxVar(r, "id")
	st, err := h.store.Load(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeAPIError(w, http.StatusNotFound, "SESSION_NOT_FOUND", fmt.Sprintf("session %q does not exist", id))
			return nil, nil, false
		}
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return nil, nil, false
	}
	if st.Session == nil || namespace.Canonical(st.Session.Namespace) != realm {
		writeAPIError(w, http.StatusNotFound, "SESSION_NOT_FOUND", fmt.Sprintf("session %q does not exist in this realm", id))
		return nil, nil, false
	}
	return st, st.Session, true
}

// loadOpenScoped is loadScoped plus the §1.3 lifecycle check.
func (h *Handler) loadOpenScoped(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return nil, false
	}
	if st.Session.State == SessionClosed {
		writeAPIError(w, http.StatusConflict, "SESSION_CLOSED",
			fmt.Sprintf("session %q is closed and admits no new message (§1.3)", sess.ID))
		return nil, false
	}
	return sess, true
}

// realm resolves the namespace a request acts in — the same
// X-Crier-Namespace / X-Crier-Namespace-Token convention the relay and the
// registry use, so the realm wall is one rule across surfaces (§4 row 38).
func (h *Handler) realm(r *http.Request, bodyNamespace string) (string, error) {
	name := strings.TrimSpace(bodyNamespace)
	if name == "" {
		name = strings.TrimSpace(r.Header.Get(namespace.HeaderNamespace))
	}
	canonical := namespace.Canonical(name)
	if canonical != "" {
		if _, ok := h.opts.Namespaces.Lookup(canonical); !ok {
			return "", namespace.ErrUnknownNamespace
		}
	}
	if err := h.opts.Namespaces.CheckAuth(canonical, r.Header.Get(namespace.HeaderNamespaceToken)); err != nil {
		return "", err
	}
	return canonical, nil
}

// checkRetention enforces §3.5: a session's declared lifetime may be shorter
// than its realm's, never longer.
func (h *Handler) checkRetention(realm string, want *int) error {
	if want == nil {
		return nil
	}
	if *want < 1 {
		return errors.New("retention_seconds must be >= 1")
	}
	pol := h.opts.Namespaces.Resolve(realm)
	if pol.RetentionSeconds != nil && int64(*want) > *pol.RetentionSeconds {
		return fmt.Errorf("retention_seconds %d exceeds the namespace's %d: a room cannot outlive the policy of the realm it lives in (§3.5)",
			*want, *pol.RetentionSeconds)
	}
	return nil
}

// authorizeRead enforces a restricted session's visibility (row 13). "" and
// "realm" are readable within the realm; "private" requires the caller to be a
// member, or — when the CR-CHAT-003 ACL is armed — to hold a read permission on
// the session. The caller names itself with the shipped X-Agent-ID header, or
// with principal_id / as_agent query parameters.
func (h *Handler) authorizeRead(ctx context.Context, r *http.Request, sess *Session, st *State) error {
	return h.authorizeReadAs(ctx, callerIdentity(r), sess, st)
}

// callerIdentity names the caller of a READ from the shipped identity
// convention: the X-Agent-ID header for an agent, and principal_id (or
// X-Crier-Principal) with an optional as_agent for a human. A read carries no
// body to name a sender in, so this is the only identity a GET has.
func callerIdentity(r *http.Request) AuthorRef {
	return AuthorRef{
		Agent:     strings.TrimSpace(r.Header.Get(registry.HeaderAgentID)),
		Principal: strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("principal_id"), r.Header.Get(headerPrincipal))),
		AsAgent:   strings.TrimSpace(r.URL.Query().Get("as_agent")),
	}
}

// authorizeReadAs is authorizeRead's rule with the caller named explicitly, so
// a POST (which names its caller in the BODY) can be held to the same rule a
// GET is — which is what CR-CHAT-028's compile needs when it reads a SOURCE
// session that belongs to another caller's request.
func (h *Handler) authorizeReadAs(ctx context.Context, who AuthorRef, sess *Session, st *State) error {
	if sess.Visibility != VisibilityPrivate {
		return nil
	}
	agent := who.Agent
	principal := who.Principal
	asAgent := who.AsAgent

	now := h.now()
	for _, m := range st.Members {
		if !m.ActiveAt(now) {
			continue
		}
		if agent != "" && m.MemberType == MemberAgent && m.MemberID == agent {
			return nil
		}
		if principal != "" && m.MemberType == MemberPrincipal && m.MemberID == principal {
			return nil
		}
	}

	if h.opts.Permissions != nil && h.opts.Permissions.Armed() {
		sender := permissions.EffectiveSender{Kind: permissions.SenderAnonymous}
		switch {
		case agent != "":
			sender = permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: agent}
		case principal != "":
			sender = permissions.EffectiveSender{Kind: permissions.SenderPrincipal, Principal: principal, AsAgent: asAgent}
		}
		res, err := h.opts.Permissions.MayDeliver(ctx, permissions.CheckInput{
			Sender:  sender,
			Action:  permissions.ActionRead,
			Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID},
		})
		if err == nil && res.Allowed {
			return nil
		}
	}

	return fmt.Errorf("session %q is private: the caller is neither a member nor granted a read on it (§4 row 13)", sess.ID)
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// headerPrincipal names the human principal a caller acts as, on a read that
// carries no body to put principal_id in.
const headerPrincipal = "X-Crier-Principal"

// Visibility values (row 13). "" is the realm default and is reported as
// absent, so a session created before the field existed reads as realm.
const (
	VisibilityRealm   = "realm"
	VisibilityPrivate = "private"
)

// memberError carries the HTTP status and error code one membership failure
// maps to, so the create path (which validates initial members) and the add
// path report it identically.
type memberError struct {
	status int
	code   string
	msg    string
}

func (e *memberError) Error() string { return e.msg }

func statusForMemberError(err error) int {
	var me *memberError
	if errors.As(err, &me) {
		return me.status
	}
	return http.StatusInternalServerError
}

func codeForMemberError(err error) string {
	var me *memberError
	if errors.As(err, &me) {
		return me.code
	}
	return "STORE_ERROR"
}

func validVisibility(v string) bool {
	switch v {
	case "", VisibilityRealm, VisibilityPrivate:
		return true
	}
	return false
}

// kindOrDefault reads a record written before the kind field existed as a
// channel (§1.2).
func kindOrDefault(k Kind) Kind {
	if k == "" {
		return DefaultKind
	}
	return k
}

func stateOrDefault(s SessionState) SessionState {
	if s == "" {
		return SessionOpen
	}
	return s
}

func memberTargetKind(mt MemberType) AudienceTargetKind {
	if mt == MemberPrincipal {
		return TargetPrincipal
	}
	return TargetAgent
}

func contextShareFor(st *State, mt MemberType, id string) *MemberContext {
	for _, c := range st.ContextShares {
		if c.MemberType == mt && c.MemberID == id {
			return c
		}
	}
	return nil
}

// resolveAuthor turns the identity fields of a request into an AuthorRef. It
// reports false when the request names nobody: an author is never defaulted.
func resolveAuthor(explicit *AuthorRef, sender, principalID, asAgent string) (AuthorRef, bool) {
	if explicit != nil && !explicit.IsZero() {
		return *explicit, true
	}
	if s := strings.TrimSpace(sender); s != "" {
		return AuthorRef{Agent: s}, true
	}
	if p := strings.TrimSpace(principalID); p != "" {
		return AuthorRef{Principal: p, AsAgent: strings.TrimSpace(asAgent)}, true
	}
	if s := strings.TrimSpace(asAgent); s != "" {
		return AuthorRef{Agent: s}, true
	}
	return AuthorRef{}, false
}

func intQuery(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errors.New("not a non-negative integer")
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000_000 {
			return 0, errors.New("too large")
		}
	}
	return n, nil
}

// validatePayload mirrors the deliver path's published contract: the payload
// is required and must be a JSON object (the request schema's `payload:
// type: object`, DF-CRIER-112).
func validatePayload(payload json.RawMessage) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return errors.New("payload is required")
	}
	if trimmed[0] != '{' {
		return errors.New("payload must be a JSON object")
	}
	return nil
}

// payloadPreview renders the human preview the list row shows: the `text`
// field of the (opaque) payload, truncated. It reads the existing convention —
// the payload is the sender's shape and `payload.text` is what a client
// renders (CHAT-INTERFACE.md §4 row 14) — and never fails a read.
func payloadPreview(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	const max = 120
	text := strings.TrimSpace(body.Text)
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// muxVar reads a gorilla/mux path variable (the router cmd/server registers
// these routes on). A plain net/http request with no mux vars answers "".
func muxVar(r *http.Request, name string) string {
	if v := mux.Vars(r); v != nil {
		return strings.TrimSpace(v[name])
	}
	return ""
}

// decodeBody decodes a JSON request body with unknown fields refused, so a misspelled key is a 400 rather than a silently ignored
// setting. It answers false having already written the error.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSessionBodyBytes))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "could not read the request body")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "a JSON body is required")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// maxSessionBodyBytes bounds a session API request body. It matches the
// delivered-payload bound the inbox routes use in spirit; unlike those, a
// session send carries no opaque binary, so a generous 1 MiB is enough and a
// larger body is refused rather than buffered.
const maxSessionBodyBytes = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

// writeRealmError renders a namespace failure with the same vocabulary the
// registry uses: an absent/wrong namespace credential is 401
// NAMESPACE_UNAUTHORIZED, an undeclared name is 400 UNKNOWN_NAMESPACE. Neither
// ever falls back to the default realm.
func writeRealmError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, namespace.ErrUnauthorized):
		writeAPIError(w, http.StatusUnauthorized, "NAMESPACE_UNAUTHORIZED", err.Error())
	case errors.Is(err, namespace.ErrUnknownNamespace):
		writeAPIError(w, http.StatusBadRequest, "UNKNOWN_NAMESPACE", err.Error())
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	}
}
