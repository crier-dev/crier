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
//     registry.SessionDeliverer.DispatchDelivery — so there is no unguarded
//     store fallback (D1, §3.4);
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
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/dispatchlive"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/strictjson"
)

// SetTaskTrigger installs the optional task-created hook (CR-CHAT-034). Nil
// (the default) leaves the task path unchanged.
func (h *Handler) SetTaskTrigger(fn TaskTriggerHook) {
	h.taskTrigger = fn
	if fn != nil && h.opts.TaskTrigger == nil {
		h.opts.TaskTrigger = fn
	}
}

// SetPeerPolicies arms the per-peer admission policy set (CR-CHAT-023) after
// construction, the same setter pattern the registry handler uses.
func (h *Handler) SetPeerPolicies(pp federation.PeerPolicies) { h.opts.PeerPolicies = pp }

// SetFedClient wires the shipped federation client for the remote-participant
// delivery leg (CR-CHAT-023 §3.4).
func (h *Handler) SetFedClient(c *federation.Client) { h.opts.Fed = c }

// AgentLookup is the one registry read this surface needs beyond delivery: the
// stored row of a member agent, whose namespace is the authority for whether a
// fan-out would cross a realm boundary (§4 row 38).
type AgentLookup interface {
	Get(id string) (*registry.Agent, error)
}

// HTTPOptions wires the optional collaborators of the session surface. The
// zero value is a standalone, unarmed handler over the default realm. Server
// boot explicitly supplies the deployment's signature requirement and verifier.
type HTTPOptions struct {
	// RequireAgentSignature is supplied by CR_REQUIRE_AGENT_SIG at server boot.
	// The embedded browser cannot sign; enforcement is never bypassed for it.
	RequireAgentSignature bool
	// AuthorizeSender reuses the live registry signature verifier for every
	// offered proof, including when enforcement is disabled.
	AuthorizeSender func(http.ResponseWriter, *http.Request, bool) (bool, bool)
	// Deliverer must expose the guarded DispatchDelivery capability; a legacy
	// Deliver-only implementation records refused / DELIVERY_UNGUARDED_UNAVAILABLE.
	// Nil means fan-out is unconfigured and a send is refused with
	// 503 SESSION_DELIVERY_UNCONFIGURED rather than silently dropped.
	Deliverer InboxDeliverer
	// DeliveryTimeoutMs is passed through DeliveryRequest.TimeoutMs to the
	// shared dispatcher. Zero uses the five-second session default.
	DeliveryTimeoutMs int
	// Agents resolves a member agent's stored namespace so a fan-out cannot
	// leave its realm and local agent membership can be validated. Nil refuses
	// local agent admission with 503 AGENT_LOOKUP_UNAVAILABLE; legacy sends
	// still rely on the deliverer for target resolution.
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
	// PeerPolicies is the per-peer admission policy set (CR-CHAT-023,
	// specs/CHAT-FEDERATION.md §6). Nil (the default, nothing configured)
	// means no remote participant can join a session and no fan-out can
	// cross to a peer: every remote-shaped member id is refused with
	// FED_PEER_UNTRUSTED, and every local member behaves exactly as before.
	PeerPolicies federation.PeerPolicies
	// Fed is the shipped federation client (CR-FEAT-006). When both Fed and
	// PeerPolicies are set, a message fanned out to a remote participant is
	// delivered to its home instance through the SHIPPED forward transport
	// (ForwardToURL) after the peer policy admits the session's namespace —
	// no second delivery path (§3.4). Nil skips the remote leg entirely and
	// the fan-out records the remote target as refused.
	Fed *federation.Client
	// GrantEvents (CR-CHAT-021) is the READ-ONLY bridge to the CR-CHAT-003
	// permission log: every grant recorded on a session subject, live AND
	// tombstoned, so the audit trail and the permission matrix can overlay
	// permission.grant / permission.revoke events. Nil (the default, no ACL
	// deployed) means the reads answer with session events and role bundles
	// only. It writes nothing — the same opt-in-posture answer the delivery
	// log precedent sets, applied to a read-only surface.
	GrantEvents func(ctx context.Context, sessionID string) ([]PermissionsGrantView, error)
	// InstanceName is this relay's name for the delivery LOCATION and the
	// location the fetch surfaces (CR-CHAT-029). Empty leaves the instance
	// field absent — "not configured", one spelling.
	InstanceName string
	// TaskTrigger is the optional task-created DAG trigger hook
	// (CR-CHAT-034). Nil (the default) leaves task creation exactly what it
	// was. It is set by the server wiring (SetTaskTrigger) after the handler
	// is built, so the session package never imports daggerctl.
	TaskTrigger TaskTriggerHook
	// TaskEvidence reads a successful run joined by the trigger journal. Nil
	// means completion requires an explicit non-empty evidence payload.
	TaskEvidence TaskEvidenceHook
}

// Handler serves the session API. It is safe for concurrent callers when its
// store is.
type Handler struct {
	store Repository
	opts  HTTPOptions
	now   func() time.Time
	newID func() (string, error)
	// live is this process's in-flight fan-out view (DF-CRIER-414). It is
	// process-local, never persisted, and never consulted for an outcome: it
	// exists so a reader can tell "dispatch in flight" from "no evidence".
	live *dispatchlive.Registry
	// taskTrigger is the optional task-created hook (CR-CHAT-034), set by
	// SetTaskTrigger after construction. Nil fires nothing.
	taskTrigger TaskTriggerHook
}

// NewHTTPHandler builds a session API handler over a Repository.
func NewHTTPHandler(store Repository, opts HTTPOptions) *Handler {
	return &Handler{
		store: store,
		opts:  opts,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newHexID,
		live:  dispatchlive.NewRegistry(),
	}
}

// LiveDispatch exposes this handler's in-flight fan-out view so the server
// wiring can feed it the runtime's own per-attempt observations (DF-CRIER-414,
// see webhook.Driver.SetAttemptObserver). It is always non-nil.
func (h *Handler) LiveDispatch() *dispatchlive.Registry { return h.live }

// deliveryClasses snapshots dispatch-path evidence, never a delivery verdict.
// Federation bypasses the webhook attempt observer. Unknown registry rows stay
// unclassified instead of implying that zero attempts means waiting.
func (h *Handler) deliveryClasses(local, remote []string) map[string]string {
	classes := make(map[string]string, len(local)+len(remote))
	for _, target := range remote {
		classes[target] = "federated"
	}
	if h.opts.Agents != nil {
		for _, target := range local {
			agent, err := h.opts.Agents.Get(target)
			if err != nil || agent == nil {
				continue
			}
			if agent.Webhook == nil {
				classes[target] = "inbox"
			} else if agent.Webhook.DeliveryMode == "blocking" {
				classes[target] = "blocking_webhook"
			}
		}
	}
	return classes
}

// deliveryBudgetMs is the per-target delivery budget this handler applies: the
// configured CR_SESSION_DELIVERY_TIMEOUT_MS, else the registry default — the
// same value fanoutAttemptWithReplies resolves, so the in-flight view can never
// publish a denominator the delivery is not actually bounded by.
func (h *Handler) deliveryBudgetMs() int {
	if h.opts.DeliveryTimeoutMs > 0 {
		return h.opts.DeliveryTimeoutMs
	}
	return registry.DefaultSessionDeliveryTimeoutMs
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
	ID              string `json:"id"`
	ThreadID        string `json:"thread_id"`
	ParentID        string `json:"parent_id,omitempty"`
	AnchorMessageID string `json:"anchor_message_id,omitempty"`
	// ParentResolvable is true when parent_id names a message in this
	// transcript, or when the message is a thread root (no parent). A false
	// value is the §4.3 broken-thread case, reported not hidden.
	ParentResolvable    bool            `json:"parent_resolvable"`
	RootID              string          `json:"root_id,omitempty"`
	Seq                 int64           `json:"seq"`
	MessageKind         string          `json:"message_kind"`
	Author              AuthorRef       `json:"author"`
	Payload             json.RawMessage `json:"payload,omitempty"`
	Audience            Audience        `json:"audience"`
	Outcomes            []outcomeView   `json:"outcomes,omitempty"`
	IdempotencyKey      string          `json:"idempotency_key,omitempty"`
	SenderIdentityBasis string          `json:"sender_identity_basis,omitempty"`
	// ReplayAction distinguishes a pure replay from a new delivery attempt of
	// the same durable message. Omitted on an original send/transcript GET.
	ReplayAction       string             `json:"replay_action,omitempty"`
	OutcomesIncomplete bool               `json:"outcomes_incomplete,omitempty"`
	DispatchEvidence   []DispatchEvidence `json:"dispatch_evidence,omitempty"`
	// DispatchLive is the fan-out pass THIS PROCESS is running for this message
	// right now (DF-CRIER-414): the pass boundaries the durable journal already
	// recorded (start, planned targets, budget) plus the attempts the runtime
	// has reported so far. It is present only while a pass is in flight, so its
	// presence is the in-flight state and its absence is the terminal record —
	// a reader never has to guess from a timer, and no client computes an
	// elapsed time or a countdown the server has not measured. It is never
	// persisted, never carries an outcome, and is gone after a restart.
	DispatchLive *dispatchlive.Pass `json:"dispatch_live,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	// Delivered / Accepted / Held / Skipped / SkippedAgents summarize ONE send.
	// Inbox/queue admission is accepted, an explicit peer hold is held, and only
	// a confirmed delivery is delivered. A target whose delivery is REFUSED
	// (an unregistered agent, a namespace mismatch, an unwired remote transport,
	// ...) is SKIPPED — the message is still recorded and other targets can
	// still accept it, so a group whose roster names an unregistered agent
	// succeeds for the rest instead of failing whole. SkippedAgents names
	// each refused target alongside its per-target reason. These send-only
	// counters are not persisted; transcript GET retains the durable outcomes.
	Delivered int `json:"delivered,omitempty"`
	Accepted  int `json:"accepted,omitempty"`
	Held      int `json:"held,omitempty"`
	// OutcomeCounts preserves every exact name, including future vocabulary.
	OutcomeCounts map[Outcome]int `json:"outcome_counts,omitempty"`
	Skipped       int             `json:"skipped,omitempty"`
	SkippedAgents []string        `json:"skipped_agents,omitempty"`
	// ThreadDepth is the thread tree's level (a deliberate branch adds one);
	// ReplyDepth is the parent_id chain length (reply attribution, never
	// depth — D11).
	ThreadDepth int `json:"thread_depth"`
	ReplyDepth  int `json:"reply_depth"`
	// SpawnedThread is present only on the send that SPAWNED a sub-thread
	// (CR-CHAT-017): the child thread's ids and the reason it was created.
	SpawnedThread *spawnedThreadView `json:"spawned_thread,omitempty"`
	// Task is the task lifecycle payload (CR-CHAT-030, §3.7) — present on
	// `task`-kind records only, so the kinds stay structurally
	// distinguishable on the wire: a reader never guesses from the text.
	Task map[string]any `json:"task,omitempty"`
}

// outcomeView renders the persisted outcome and its diagnostic as detail on
// both the send response and transcript re-reads. SQL retains reasons through
// the additive request projection; JSONL retains them directly in the record.
type outcomeView struct {
	Target       string    `json:"target"`
	Outcome      string    `json:"outcome"`
	InboxEntryID string    `json:"inbox_entry_id,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	Code         string    `json:"code,omitempty"`
	Retryable    bool      `json:"retryable"`
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
			Code:         o.Code,
			Retryable:    o.Retryable,
			UpdatedAt:    o.UpdatedAt,
		})
	}
	return out
}

// transcriptResponse is the GET /sessions/{id}/messages body.
type transcriptResponse struct {
	Session      sessionView         `json:"session"`
	Messages     []transcriptMessage `json:"messages"`
	SystemEvents []LifecycleEvent    `json:"system_events"`
	Count        int                 `json:"count"`
	// Findings is what the reconstruction had to decide about a thread key —
	// a derived or broken thread is reported, never hidden (§4.3).
	Findings []ThreadFinding `json:"findings,omitempty"`
}

// participantView is one membership row (row 24). Removed members are returned
// with Active false rather than erased: membership is a recorded event, not a
// mutable set (§2.3, §2.4).
//
// CR-CHAT-023: a member whose id is a remote-qualified identity
// (`remote:<peer>:<subject>`, spec §8.3) carries Remote true — the reader of
// the list can always tell a participant from a peer instance. The marker is
// derived from the RECORDED id, so it is reconstructable from storage alone.
type participantView struct {
	MemberType   string        `json:"member_type"`
	MemberID     string        `json:"member_id"`
	Role         string        `json:"role"`
	AddedAt      time.Time     `json:"added_at"`
	RemovedAt    *time.Time    `json:"removed_at,omitempty"`
	Active       bool          `json:"active"`
	Remote       bool          `json:"remote,omitempty"`
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
	MemberType               string        `json:"member_type"`
	MemberID                 string        `json:"member_id"`
	Role                     string        `json:"role,omitempty"`
	ContextShare             *ContextShare `json:"context_share,omitempty"`
	ContextBoundaryMessageID string        `json:"context_boundary_message_id,omitempty"`
	Actor                    string        `json:"actor,omitempty"`
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
	Branch         bool             `json:"branch,omitempty"`
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
	// A create is all-or-nothing up to its first record: every declared member is
	// composed AND validated — including its context boundary — against the EMPTY
	// state of the room that does not exist yet, and nothing is written until they
	// all pass. That is what keeps a 400 truthful: an unresolvable agent
	// (DF-CRIER-471) and a context boundary the new room's transcript cannot
	// contain (DF-CRIER-481) both refuse the request with no session and no
	// membership persisted, so a retry with the same id creates the room instead
	// of colliding with a durable ghost of the failed attempt.
	//
	// Rolling back instead is NOT available here, and that was measured, not
	// assumed: the store is an append-only record log — session.create,
	// session.member.add, message, thread.branch, session.close, session.reopen —
	// with no delete/tombstone record, and CreateSession on every backend is just
	// Append(sess.CreateRecord(seq, ...)). Validating before the first write is the
	// only way to reach the caller-visible outcome "nothing happened".
	pending := &State{Session: sess}
	plans := make([]*memberPlan, 0, len(req.Members))
	for _, m := range req.Members {
		plan, err := h.composeMember(sess, m, creator.AgentID())
		if err != nil {
			writeAPIError(w, statusForMemberError(err), codeForMemberError(err), err.Error())
			return
		}
		if err := checkMemberBoundary(pending, sess.ID, plan); err != nil {
			writeAPIError(w, statusForMemberError(err), codeForMemberError(err), err.Error())
			return
		}
		plans = append(plans, plan)
	}
	if err := h.store.CreateSession(r.Context(), sess, 1); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}

	// Past this line the room EXISTS. A failure now is reported as the partial
	// success it is, naming the session id and the members already recorded —
	// never as a bare "nothing happened" (DF-CRIER-481).
	added := make([]string, 0, len(plans))
	for _, plan := range plans {
		if err := h.appendMember(r.Context(), sess, plan); err != nil {
			writePartialCreateError(w, sess, added, err)
			return
		}
		added = append(added, plan.Member.MemberID)
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
		Session:      h.viewOf(st),
		Messages:     make([]transcriptMessage, 0, len(st.Messages)),
		SystemEvents: systemEvents(st),
		Findings:     st.Findings,
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
			// CR-CHAT-023 §8.3: a remote participant is named REMOTE, in
			// its remote-qualified recorded id, never as a local one.
			Remote: IsRemoteMember(m.MemberID),
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
	// response is the recorded fact, not the request echoed — including the
	// late-join context answer (its default included, §4.6 D10).
	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	for _, m := range st.Members {
		if m.MemberType == MemberType(req.MemberType) && m.MemberID == req.MemberID && m.ActiveAt(h.now()) {
			view := participantView{
				MemberType: string(m.MemberType),
				MemberID:   m.MemberID,
				Role:       string(m.Role),
				AddedAt:    m.AddedAt,
				Active:     true,
				Remote:     IsRemoteMember(m.MemberID),
			}
			if mc := contextShareFor(st, m.MemberType, m.MemberID); mc != nil {
				view.ContextShare = &ContextShare{Mode: mc.Mode, BoundaryMessageID: mc.BoundaryMessageID}
			}
			writeJSON(w, http.StatusCreated, view)
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
	if !decodeSendBody(w, r, &req) {
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
	basis, allowed := h.authorizeSender(w, r, author)
	if !allowed {
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

	if req.IdempotencyKey != "" {
		// The store lock, not a handler-local check, makes this lookup and the
		// complete send one critical section even across independent servers.
		release, err := h.store.LockMessage(r.Context(), sess.ID, req.IdempotencyKey)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		defer release()
		st, err := h.store.Load(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		for _, existing := range st.Messages {
			if existing.IdempotencyKey == req.IdempotencyKey {
				if !messageOutcomesIncomplete(existing) && allRetryableOutcomes(existing.Outcomes) {
					// The store lock spans re-delivery and outcome replacement too.
					// Use ONLY the recorded intent: a changed retry payload/audience
					// must not mutate or expand this message or create a branch.
					view, ok := h.deliverMessage(w, r, sess, existing, nil, true)
					if !ok {
						return
					}
					view.ReplayAction = "redelivered"
					writeJSON(w, http.StatusCreated, view)
					return
				}
				view := h.messageViewOf(st, existing)
				view.Outcomes = outcomeViews(existing.Outcomes)
				summarizeFanout(&view, existing.Outcomes)
				view.ReplayAction = replayAction(existing)
				writeJSON(w, http.StatusCreated, view)
				return // delivered/mixed/canned/incomplete: no second delivery
			}
		}
	}

	msgID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	now := h.now()
	msg := &Message{
		SenderIdentityBasis: basis,
		ID:                  msgID,
		ParentID:            strings.TrimSpace(req.ParentID),
		SessionID:           sess.ID,
		Kind:                kind,
		Author:              author,
		Payload:             req.Payload,
		CreatedAt:           now,
		IdempotencyKey:      req.IdempotencyKey,
	}

	// Thread resolution: a root's thread is its own id; a reply stays in its
	// parent's thread (D11, §4.5) — resolved from the transcript, never
	// fabricated.
	if req.Branch && msg.ParentID == "" {
		writeAPIError(w, http.StatusBadRequest, "PARENT_NOT_FOUND", "a deliberate branch requires parent_id")
		return
	}
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

	// Sub-thread spawn: an explicit branch adds one level from the exact
	// selected parent. The existing compatibility rule is unchanged: addressing an agent
	// that is NOT a participant deliberately pulls a stranger into the
	// conversation, so the message branches into a child thread
	// (parent_thread_id + anchor_message_id). Replies to existing
	// participants stay in this thread — spawnSubThreads writes nothing when
	// every addressee is a member. The branch record is appended BEFORE the
	// message record (one handler pass, contiguous seqs — the atomic write).
	spawn, err := h.spawnSubThreads(r.Context(), sess, msg, messageAddressees(&req), req.Branch)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}

	// @team:x mention resolution (CR-CHAT-022, D8): a body tag `@team:<name>`
	// in the addressing grammar adds the group to the audience as a `group`
	// target — resolved to its CURRENT members at fan-out time by
	// resolveGroupRoster. A name with no roster is a RECORDED skip (§3.2's
	// named-error rule), never a guessed delivery; the capability form
	// `@cap:y` is NOT expanded here (a capability is a selector, not a
	// fan-out — D8) and rides the shipped capability route instead.
	groupMentions := messageGroupMentions(&req)
	if len(groupMentions) > 0 {
		extra, err := h.groupMentionTargets(r.Context(), groupMentions)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "INVALID_ADDRESS", err.Error())
			return
		}
		req.Targets = append(req.Targets, extra...)
	}

	// Resolved audience (§4.2): the caller's explicit targets, else every
	// active participant except the author.
	aud, err := h.resolveAudience(r.Context(), sess, &req, author.AgentID())
	if err != nil {
		writeAudienceError(w, err)
		return
	}

	view, ok := h.sendMessage(w, r, sess, msg, aud, spawn)
	if !ok {
		return
	}
	// Keep the immediate response and keyed replay on the same outcome
	// vocabulary and refusal detail. The SQL request projection preserves
	// detail without changing chat_deliveries' schema.
	view.Outcomes = outcomeViews(msg.Outcomes)
	if spawn != nil {
		view.SpawnedThread = spawn
	}
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
func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request, sess *Session, msg *Message, aud Audience, spawn *spawnedThreadView) (transcriptMessage, bool) {
	msg.Audience = aud

	seq, err := h.store.NextSeq(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	msg.Seq = seq
	return h.deliverMessage(w, r, sess, msg, spawn, false)
}

// allRetryableOutcomes is deliberately fail-closed: an empty/incomplete result,
// canned demo, or ANY received target keeps replay side-effect free. In
// particular a partially delivered fan-out must not run its successful target twice.
func allRetryableOutcomes(outcomes []DeliveryOutcome) bool {
	if len(outcomes) == 0 {
		return false
	}
	for _, o := range outcomes {
		if o.Outcome != OutcomeRefused && o.Outcome != OutcomeFailed && o.Outcome != OutcomeBudgetCut {
			return false
		}
	}
	return true
}

// deliverMessage shares the guarded delivery/reply/outcome path between the
// initial intent and an eligible retry. A retry keeps ID, seq, thread, payload,
// author and audience; only the outcomes are replaced on that transcript row.
func (h *Handler) deliverMessage(w http.ResponseWriter, r *http.Request, sess *Session, msg *Message, spawn *spawnedThreadView, retry bool) (transcriptMessage, bool) {
	aud := msg.Audience
	// The roster of every `group` target is resolved HERE, fresh, at send
	// time (CR-CHAT-013, §1.4 consequence 1): a roster edit routes the NEXT
	// send to the CURRENT members, never a cached set. A nil store leaves
	// every group target a recorded skip.
	roster := h.resolveGroupRoster(r.Context(), aud)
	recipients, skipped := FanoutRecipients(aud, roster)
	prior := append([]DeliveryOutcome(nil), msg.Outcomes...)
	if retry {
		// Retry the original resolved targets, not a group's changed roster.
		recipients = nil
		for _, o := range prior {
			recipients = append(recipients, o.Target)
		}
	}
	// A target that is deliberately NOT fanned out is never a silent drop
	// (§3.2's named-error rule): a group with no roster is a delivery miss
	// and is said out loud, while the by-design skips (a capability is a
	// selector, a principal has no inbox) stay visible at debug level. A kind
	// outside the addressable set cannot arrive through a send — resolveAudience
	// refuses it before anything is written — so reaching here means a
	// directly-constructed audience or a record written before DF-CRIER-475:
	// that one is warned, with the same code the send path would have answered.
	for _, s := range skipped {
		switch s.Target.Kind {
		case TargetGroup:
			slog.Warn("fan-out: skipping group with no known roster",
				"group", s.Target.ID, "message_id", msg.ID, "reason", s.Reason)
		case TargetAgent, TargetCapability, TargetPrincipal:
			slog.Debug("fan-out: audience target is not fanned out",
				"target_kind", string(s.Target.Kind), "target", s.Target.ID,
				"message_id", msg.ID, "reason", s.Reason)
		default:
			slog.Warn("fan-out: audience target kind is not addressable",
				"code", CodeUnknownTargetKind, "target_kind", string(s.Target.Kind),
				"target", s.Target.ID, "message_id", msg.ID, "reason", s.Reason)
		}
	}
	// Explicit targets, group expansion and pre-change retry outcomes must not
	// reintroduce the sender before realm checks can produce a false refusal.
	recipients = withoutAuthor(recipients, msg.Author.AgentID())
	crossRealm, deliverable, remoteTargets := h.partitionRealm(recipients, sess.Namespace, h.now())
	msg.Outcomes = crossRealm

	// 1. Only a NEW send appends intent. A retry must not overwrite durable
	// negative outcomes with an empty in-progress row (or allocate a new seq).
	if !retry {
		if err := h.store.PostMessage(r.Context(), msg); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return transcriptMessage{}, false
		}
	} else {
		msg.Outcomes = prior
	}

	// 2. One delivery per participant through the shipped inbox path — and,
	// for a REMOTE participant (CR-CHAT-023 §3.2/§3.4), one forward to its
	// home instance through the shipped federation transport. Remote leg
	// first: its outcome is an outcome like any other and lands on the same
	// record. Policy gates before anything leaves (§6.3).
	memState, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	// Journal the fan-out boundary before any transport can run. This append
	// deliberately retains the prior outcomes; it is not the outcome write-back.
	// Both legs consume the SAME roster-expanded recipients above. Remote
	// identities never enter local dispatch, even when they have no active
	// membership or federation route; the remote leg records that refusal.
	targets := append(append([]string{}, deliverable...), remoteTargets...)
	// The audience summary of THIS pass (DF-CRIER-479): the rule that resolved
	// the audience and how many targets it resolved to. Counted from the SAME
	// recipient snapshot the fan-out below consumes (roster-expanded, author
	// excluded, before the realm partition), so an EMPTY session audience — no
	// participant has joined yet — records resolved_target_count 0 instead of
	// leaving the caller to infer "nothing was delivered" from two empty arrays.
	msg.DispatchEvidence = append(msg.DispatchEvidence, DispatchEvidence{
		Attempt: len(msg.DispatchEvidence) + 1, StartedAt: h.now(),
		AudienceRule:        aud.Rule,
		ResolvedTargetCount: len(dedupeSorted(recipients)),
		Targets:             append([]string{}, dedupeSorted(targets)...), Receipts: []DeliveryOutcome{},
	})
	attempt := &msg.DispatchEvidence[len(msg.DispatchEvidence)-1]
	if err := h.appendDispatchEvidence(r.Context(), msg, prior); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", "fan-out not started: "+err.Error())
		return transcriptMessage{}, false
	}
	// DF-CRIER-414: register the in-flight window that the durable journal
	// cannot describe. Between this boundary append and the outcome write-back
	// below, the transcript has an intent and NO results — a 30s blocking
	// budget in which an operator could not tell "working" from "wedged", and
	// the first evidence of an unreachable endpoint was the terminal
	// budget_cut. The pass is ended explicitly once the outcomes are durable
	// (below), with this deferred End as the early-return safety net, so a
	// reader that polls past the budget sees the terminal record and never a
	// stale indicator. Targets are the same resolved planned set the journal
	// just recorded — the live view plans nothing of its own.
	if len(attempt.Targets) > 0 {
		h.live.BeginWithClasses(sess.ID, msg.ID, attempt.Attempt, attempt.Targets, h.deliveryBudgetMs(), attempt.StartedAt, h.deliveryClasses(deliverable, remoteTargets))
		defer h.live.End(sess.ID, msg.ID)
	}
	remoteOutcomes, err := h.deliverRemote(r.Context(), sess, memState, msg, remoteTargets, r.Header.Get(namespace.HeaderNamespaceToken))
	attempt.addReceipts(remoteOutcomes)
	if journalErr := h.appendDispatchEvidence(r.Context(), msg, prior); journalErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", journalErr.Error())
		return transcriptMessage{}, false
	}
	if err != nil {
		// A remote fan-out that cannot complete is the same visible,
		// repairable state §3.2 already names: the record is durable,
		// outcomes are incomplete, the caller retries the same message id.
		writeAPIError(w, http.StatusBadGateway, "FANOUT_INCOMPLETE",
			fmt.Sprintf("the transcript record is durable (message %q) but the remote fan-out did not complete: %v", msg.ID, err))
		return transcriptMessage{}, false
	}

	outcomes, err := fanoutAttemptWithReplies(r.Context(), h.opts.Deliverer, msg, deliverable, spawn, h.opts.DeliveryTimeoutMs, r.Header.Get(namespace.HeaderNamespaceToken), retry, func(target string, reply json.RawMessage) error {
		return h.recordWebhookReply(r.Context(), msg, target, reply)
	})
	attempt.addReceipts(outcomes)
	if len(outcomes) == len(deliverable) && len(remoteOutcomes) == len(remoteTargets) {
		completed := h.now()
		attempt.CompletedAt = &completed
	}
	if journalErr := h.appendDispatchEvidence(r.Context(), msg, prior); journalErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", journalErr.Error())
		return transcriptMessage{}, false
	}
	if err != nil {
		// The intent is durable and the outcomes are incomplete — §3.2's
		// visible, repairable state, not a lost message. Report it as a 502
		// so the caller can retry the same message id without a re-send.
		writeAPIError(w, http.StatusBadGateway, "FANOUT_INCOMPLETE",
			fmt.Sprintf("the transcript record is durable (message %q) but the fan-out did not complete: %v", msg.ID, err))
		return transcriptMessage{}, false
	}

	// 3. Write the outcomes back onto the same record (same seq, keep-LAST).
	// Local Fanout returns only local outcomes; merge the disjoint remote
	// results explicitly instead of relying on its prior-outcome replay map.
	outcomes = append(outcomes, remoteOutcomes...)
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Target < outcomes[j].Target })
	msg.Outcomes = append(crossRealm, outcomes...)

	// A refused target is a SKIP, never fatal: the message is durable and
	// every other target still received it. Say the skip out loud
	// (DF-CRIER-305) — an unregistered roster member must be visible in the
	// SERVER LOG, not only in the per-target outcomes a caller has to read
	// (and may never see). The warning names the group the target was
	// addressed through when it came from one.
	warnSkippedTargets(msg.ID, msg.Outcomes, groupOfTarget(roster))
	attempt.OutcomesRecorded = true

	if err := h.store.PostMessage(r.Context(), msg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	// The pass is over the moment its outcomes are durable: from here a reader
	// sees the TERMINAL record, so the in-flight view must not survive into the
	// send response or a later transcript GET (DF-CRIER-414 requirement 5). The
	// deferred End above stays as the early-return safety net; End is idempotent.
	h.live.End(sess.ID, msg.ID)

	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return transcriptMessage{}, false
	}
	recorded := st.Message(msg.ID)
	if recorded == nil {
		recorded = msg
	}
	view := h.messageViewOf(st, recorded)
	// The partial-success counts of THIS send (DF-CRIER-305), read from the
	// SAME outcomes the response carries so the summary can never disagree
	// with the per-target detail.
	summarizeFanout(&view, msg.Outcomes)
	return view, true
}

// summarizeFanout counts every outcome under its exact name. The legacy
// delivered/skipped projections still mean only delivered/refused; queue and
// hold receipts have their own counters and are never evidence of execution.
func summarizeFanout(v *transcriptMessage, outcomes []DeliveryOutcome) {
	seen := make(map[string]bool, len(outcomes))
	v.OutcomeCounts = make(map[Outcome]int, len(outcomes))
	for _, o := range outcomes {
		v.OutcomeCounts[o.Outcome]++
		switch o.Outcome {
		case OutcomeDelivered:
			v.Delivered++
		case OutcomeAccepted:
			v.Accepted++
		case OutcomeHeld:
			v.Held++
		}
		if o.Outcome != OutcomeRefused {
			continue
		}
		v.Skipped++
		if o.Target != "" && !seen[o.Target] {
			seen[o.Target] = true
			v.SkippedAgents = append(v.SkippedAgents, o.Target)
		}
	}
	sort.Strings(v.SkippedAgents)
}

// warnSkippedTargets logs one WARNING per refused/non-answer target. A
// refusal is per-target; it does not imply any other target executed the work.
// An unregistered (or unreachable) target must be visible in the log, never
// only in a response the caller may not read. Named groups are included.
func warnSkippedTargets(messageID string, outcomes []DeliveryOutcome, groupOf map[string]string) {
	for _, o := range outcomes {
		if o.Outcome == OutcomeDelivered || o.Outcome == OutcomeAccepted || o.Outcome == OutcomeHeld || o.Code == codeDeliveryUnguardedUnavailable {
			// The unavailable-dispatcher refusal is logged at the fan-out
			// boundary, including for non-HTTP callers. Do not log it twice.
			continue
		}
		if o.Outcome == OutcomeInProgress {
			slog.Info("fan-out: delivery already in progress", "target", o.Target, "message_id", messageID, "code", o.Code)
			continue
		}
		attrs := []any{"target", o.Target, "message_id", messageID, "reason", o.Reason}
		if g := groupOf[o.Target]; g != "" {
			attrs = append(attrs, "group", g)
		}
		if o.Outcome == OutcomeCanned || o.Outcome == OutcomeFailed || o.Outcome == OutcomeBudgetCut || o.Outcome == OutcomeUnreadableReply {
			slog.Warn("fan-out: webhook runtime reported a non-answer", append(attrs, "outcome", o.Outcome)...)
			continue
		}
		// DF-CRIER-398: name the outcome instead of calling every non-delivery a
		// refusal. leased/acked/expired are not refusals, and a log line that
		// says "refused" for a lease makes an operator hunt a policy that never
		// fired.
		line := "fan-out: target not delivered — other targets retain their own outcomes"
		if o.Outcome == OutcomeRefused {
			line = "fan-out: target refused delivery — other targets retain their own outcomes"
		}
		slog.Warn(line, append(attrs, "outcome", o.Outcome)...)
	}
}

// groupOfTarget indexes which named group a fan-out recipient came from, so a
// skipped target's warning can name the group it was addressed through
// (DF-CRIER-305). A member of several groups is attributed to the
// lexicographically first, so the attribution is deterministic.
func groupOfTarget(roster map[string][]string) map[string]string {
	if len(roster) == 0 {
		return nil
	}
	names := make([]string, 0, len(roster))
	for name := range roster {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		for _, m := range roster[name] {
			if _, ok := out[m]; !ok {
				out[m] = name
			}
		}
	}
	return out
}

// deliverRemote fans a message out to the session's REMOTE participants
// (CR-CHAT-023 §3.2, §3.4): one forward per remote member to its home peer,
// through the SHIPPED federation transport — no second delivery path. The
// policy gates run before anything leaves (§6.3): a remote member whose peer
// policy does not admit the session's namespace, or whose peer has no
// policy, is recorded as a REFUSED outcome with the named reason, and the
// message is never forwarded to it. A transport failure on an admitted peer
// is an error (the caller's FANOUT_INCOMPLETE), matching the local leg's
// contract. Targets are the caller's single roster-expanded recipient snapshot
// (or the durable original targets on retry), not a second audience resolution.
// The membership passed in still gates participation: group addressing cannot
// admit a remote member or bypass peer policy. The policy re-check is applied
// per SEND, not per JOIN (a policy can be tightened between the two).
func (h *Handler) deliverRemote(ctx context.Context, sess *Session, st *State, msg *Message, targets []string, namespaceToken string) ([]DeliveryOutcome, error) {
	outcomes := []DeliveryOutcome{}
	now := h.now()
	active := make(map[string]bool, len(st.Members))
	for _, m := range st.Members {
		if m.ActiveAt(now) && m.MemberType == MemberAgent {
			active[m.MemberID] = true
		}
	}
	for _, target := range withoutAuthor(dedupeSorted(targets), msg.Author.AgentID()) {
		peer, subject, ok := ParseRemoteMemberID(target)
		if !ok {
			outcomes = append(outcomes, refusedOutcome(target, "invalid remote participant identity", now))
			continue
		}
		if !active[target] {
			outcomes = append(outcomes, refusedOutcome(target, "remote target is not an active session participant", now))
			continue
		}
		if h.opts.Fed == nil {
			outcomes = append(outcomes, refusedOutcome(target,
				"remote participant: no federation client is wired (FEDERATION_UNCONFIGURED)", now))
			continue
		}
		policy := h.opts.Fed.PolicyFor(peer)
		if policy == nil {
			outcomes = append(outcomes, refusedOutcome(target,
				fmt.Sprintf("peer %q is not known to this instance (no policy record)", peer), now))
			continue
		}
		if !policy.AdmitsNamespace(sess.Namespace) {
			outcomes = append(outcomes, refusedOutcome(target,
				fmt.Sprintf("peer %q is not permitted to reach namespace %q (namespaces_allow)", peer, namespace.Display(sess.Namespace)), now))
			continue
		}
		if policy.URL == "" {
			outcomes = append(outcomes, refusedOutcome(target,
				fmt.Sprintf("peer %q is inbound-only (no url): the message cannot cross to it", peer), now))
			continue
		}
		envelope, err := json.Marshal(remoteDeliveryEnvelope{
			SessionID:      sess.ID,
			ThreadID:       msg.ThreadID,
			MessageID:      msg.ID,
			Sender:         msg.Author.AgentID(),
			Namespace:      sess.Namespace,
			Kind:           string(msg.Kind),
			IdempotencyKey: IdempotencyKey(msg.SessionID, msg.ThreadID, msg.ID, target),
			Payload:        msg.Payload,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal remote delivery for %q: %w", target, err)
		}
		receipt, ferr := h.opts.Fed.ForwardToURLReceiptWithNamespaceToken(ctx, policy.URL, subject, envelope, namespaceToken)
		status, response := receipt.StatusCode, receipt.Body
		if ferr != nil {
			// Transport-level failure: the message was NOT delivered. A
			// transient outage is the caller's FANOUT_INCOMPLETE (the
			// record is durable, the sender retries); a policy refusal on
			// the DESTINATION side (§6.4's inbound FED_* codes) is a
			// definitive refused outcome carrying the remote's own answer.
			var refusal *federation.DeliveryRefusal
			if errors.As(ferr, &refusal) {
				o := refusedOutcome(target, string(refusal.Verdict), now)
				o.Code = string(refusal.Verdict)
				outcomes = append(outcomes, o)
				continue
			}
			var nsnp *federation.NamespaceNotPermittedError
			if errors.As(ferr, &nsnp) {
				outcomes = append(outcomes, refusedOutcome(target,
					"the destination's peer policy refused the namespace (FED_NAMESPACE_NOT_PERMITTED)", now))
				continue
			}
			return nil, fmt.Errorf("forward to peer %q for %q: %w", peer, target, ferr)
		}
		if status >= 200 && status < 300 {
			outcome, code := transportReceiptOutcome(response, receipt.Verdict, receipt.InboundVerdict)
			if outcome == OutcomeDelivered {
				if err := h.recordAnswerText(ctx, msg, target, transportReplyText(response)); err != nil {
					return nil, fmt.Errorf("record peer reply from %q: %w", target, err)
				}
			}
			outcomes = append(outcomes, DeliveryOutcome{
				Target:    target,
				Outcome:   outcome,
				Code:      code,
				Reason:    transportReceiptReason(code),
				UpdatedAt: now,
			})
			continue
		}
		// Keep only our known symbolic codes. Remote detail is untrusted and may
		// echo a credential, so it must never be copied into the durable transcript.
		var remoteRefusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response, &remoteRefusal) == nil {
			switch remoteRefusal.Error {
			case "NAMESPACE_UNAUTHORIZED", "UNKNOWN_NAMESPACE", "NAMESPACE_MISMATCH",
				"FED_NO_LINK", "FED_BAD_SIGNATURE", "FED_PEER_REVOKED", "FED_NAMESPACE_NOT_PERMITTED",
				"FED_AGENT_NOT_PERMITTED", "FED_DIRECTION_NOT_PERMITTED", "FED_SESSION_NOT_PERMITTED", "DELIVERY_FORBIDDEN":
				o := refusedOutcome(target, fmt.Sprintf("the destination refused the delivery (status %d, %s)", status, remoteRefusal.Error), now)
				o.Code = remoteRefusal.Error
				outcomes = append(outcomes, o)
				continue
			}
		}
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			// The destination refused the crossing (its own per-peer
			// policy or its auth): a definitive, named refusal — never a
			// silent drop, never a retryable outage.
			outcomes = append(outcomes, refusedOutcome(target,
				fmt.Sprintf("the destination refused the delivery (status %d)", status), now))
			continue
		}
		// Any other non-2xx answer from the home instance: also definitive
		// for this send, recorded with the status.
		outcomes = append(outcomes, refusedOutcome(target,
			fmt.Sprintf("the destination answered %d for the delivery", status), now))
	}
	return outcomes, nil
}

// refusedOutcome is the named refusal shape of §3.2: a refused outcome
// carries the real reason, never a swallowed error.
func refusedOutcome(target, reason string, at time.Time) DeliveryOutcome {
	return DeliveryOutcome{Target: target, Outcome: OutcomeRefused, Reason: reason, UpdatedAt: at}
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
		ID:                  m.ID,
		ThreadID:            m.ThreadID,
		ParentID:            m.ParentID,
		Seq:                 m.Seq,
		MessageKind:         string(m.Kind),
		Author:              m.Author,
		Payload:             m.Payload,
		Audience:            m.Audience,
		Outcomes:            outcomeViews(m.Outcomes),
		IdempotencyKey:      m.IdempotencyKey,
		DispatchEvidence:    m.DispatchEvidence,
		OutcomesIncomplete:  messageOutcomesIncomplete(m),
		SenderIdentityBasis: m.SenderIdentityBasis,
		CreatedAt:           m.CreatedAt,
		ThreadDepth:         st.ThreadDepth(m.ThreadID),
	}
	if thread := st.Thread(m.ThreadID); thread != nil && thread.RootMessageID == m.ID {
		v.AnchorMessageID = thread.AnchorMessageID
	}
	v.ParentResolvable = m.ParentID == "" || st.Message(m.ParentID) != nil
	// A live pass is attached ONLY to the message it belongs to, and only
	// while this process is delivering it (DF-CRIER-414). Because every read
	// surface builds its view here, the send response, the transcript GET, the
	// thread reads and the output modes all agree on what "in flight" means.
	v.DispatchLive = h.live.Live(m.SessionID, m.ID)
	if t := st.Thread(m.ThreadID); t != nil {
		v.RootID = t.RootMessageID
	}
	if chain, _ := st.ReplyChain(m.ID); len(chain) > 0 {
		v.ReplyDepth = len(chain) - 1
	}
	if m.Task.ID != "" {
		v.Task = map[string]any{
			"task_id":    m.Task.ID,
			"state":      string(m.Task.State),
			"owner":      m.Task.Owner,
			"updated_at": m.Task.UpdatedAt,
		}
	}
	return v
}

// resolveAudience builds the resolved audience of a send (§4.2). An explicit
// target list excludes the author; otherwise the audience is the session's active
// participants minus the author.
func (h *Handler) resolveAudience(ctx context.Context, sess *Session, req *postMessageRequest, authorAgent string) (Audience, error) {
	if len(req.Targets) > 0 {
		// The kind vocabulary is CLOSED and is checked HERE — before a
		// message id, thread, seq or transcript row exists (DF-CRIER-475). An
		// unknown kind used to survive this function and become a skip inside
		// the fan-out, which answered 201 with the audience recorded and
		// nothing delivered: a silent no-op for the `@team:<name>` body form
		// mis-mapped onto a structured target. It is a named refusal now.
		if err := ValidateAudienceTargets(req.Targets); err != nil {
			return Audience{}, err
		}
		seen := map[string]bool{}
		targets := make([]AudienceTarget, 0, len(req.Targets))
		for _, t := range req.Targets {
			if t.ID == "" {
				return Audience{}, errors.New("every audience target needs an id")
			}
			if t.Kind == TargetAgent && strings.TrimSpace(t.ID) == authorAgent {
				continue
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
// row is in another realm (refused, never delivered — §4 row 38), local
// deliverable targets, and remote-qualified targets owned by the federation leg.
// Even a malformed remote: ID must not fall through to local registry lookup.
// An unknown LOCAL agent is left to the local deliver path for its named refusal.
func (h *Handler) partitionRealm(recipients []string, realm string, now time.Time) (crossRealm []DeliveryOutcome, deliverable, remote []string) {
	for _, id := range recipients {
		if strings.HasPrefix(id, RemotePrefix) {
			remote = append(remote, id)
			continue
		}
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
	return crossRealm, deliverable, remote
}

// checkMemberAgent validates local agent admission against the registry. Remote
// identities retain their separate peer-policy admission contract; principals
// have no registry inbox. A registry outage is not evidence an agent is missing.
func (h *Handler) checkMemberAgent(sess *Session, req addParticipantRequest) error {
	id := strings.TrimSpace(req.MemberID)
	if MemberType(strings.TrimSpace(req.MemberType)) != MemberAgent || id == "" || IsRemoteMember(id) {
		return nil
	}
	unavailable := func() error {
		return &memberError{status: http.StatusServiceUnavailable, code: "AGENT_LOOKUP_UNAVAILABLE",
			msg: fmt.Sprintf("cannot validate agent %q: registry lookup unavailable", id)}
	}
	if h.opts.Agents == nil {
		return unavailable()
	}
	ag, err := h.opts.Agents.Get(id)
	if errors.Is(err, registry.ErrAgentNotFound) {
		return &memberError{status: http.StatusNotFound, code: "AGENT_NOT_FOUND",
			msg: fmt.Sprintf("no such agent %q: register the agent before adding it to a session", id)}
	}
	if err != nil || ag == nil {
		return unavailable()
	}
	if namespace.Canonical(ag.Namespace) != namespace.Canonical(sess.Namespace) {
		return &memberError{status: http.StatusConflict, code: "NAMESPACE_MISMATCH",
			msg: fmt.Sprintf("agent %q is in namespace %q, but session %q is in %q",
				id, namespace.Display(ag.Namespace), sess.ID, namespace.Display(sess.Namespace))}
	}
	return nil
}

// memberPlan is one membership composed and validated but NOT yet written. A
// create builds the whole plan before its first record, so a member the room
// cannot honour — an unknown agent, an invalid share, a boundary the transcript
// does not contain — refuses the request with nothing persisted rather than
// leaving a room behind that the caller believes it never made (DF-CRIER-481).
type memberPlan struct {
	Member *Member
	// Share is the RESOLVED context share (mode/boundary adjusted by
	// resolveJoinShare), not the raw request value.
	Share  *ContextShare
	Reason string
}

// composeMember validates one membership request that needs no room history and
// composes the record it would append. Everything here is state-free apart from
// the prospective session (its id and realm), which is why the create path can
// run it before the room exists.
func (h *Handler) composeMember(sess *Session, req addParticipantRequest, actor string) (*memberPlan, error) {
	mt := MemberType(strings.TrimSpace(req.MemberType))
	switch mt {
	case MemberAgent, MemberPrincipal:
	default:
		return nil, &memberError{status: http.StatusBadRequest, code: "INVALID_MEMBER",
			msg: fmt.Sprintf("member_type must be %q or %q", MemberAgent, MemberPrincipal)}
	}
	id := strings.TrimSpace(req.MemberID)
	if id == "" {
		return nil, &memberError{status: http.StatusBadRequest, code: "INVALID_MEMBER", msg: "member_id is required"}
	}
	role := MemberRole(strings.TrimSpace(req.Role))
	if role == "" {
		// §2.1: every member has exactly one role. The vocabulary is data
		// (CHAT-PERMISSIONS.md owns it), so an absent role takes the least
		// surprising default rather than inventing a new one.
		role = RoleMember
	}
	share := ContextShare{Mode: DefaultContextShareMode}
	if req.ContextShare != nil {
		share = *req.ContextShare // do not mutate caller-owned state
	}
	if req.ContextBoundaryMessageID != "" {
		if share.BoundaryMessageID != "" && share.BoundaryMessageID != req.ContextBoundaryMessageID {
			return nil, &memberError{status: http.StatusBadRequest, code: "INVALID_CONTEXT_SHARE", msg: "conflicting context boundary message ids"}
		}
		share.BoundaryMessageID = req.ContextBoundaryMessageID
	}
	if err := validateContextShare(&share); err != nil {
		return nil, &memberError{status: http.StatusBadRequest, code: "INVALID_CONTEXT_SHARE", msg: err.Error()}
	}
	if err := h.checkMemberAgent(sess, req); err != nil {
		return nil, err
	}

	// CR-CHAT-023 (§3.2, §3.3, §6.2): a remote-qualified member id names a
	// participant of a peer instance. It is admitted only when the peer has
	// a policy record AND that policy admits the session's namespace (the
	// policy is a wall; default deny). A remote-shaped id with no parseable
	// peer or subject is refused rather than guessed at — no silent
	// reinterpretation of identity.
	if IsRemoteMember(id) {
		if err := validateRemoteMember(h.opts.PeerPolicies, id, sess.Namespace); err != nil {
			return nil, err
		}
	}

	m := &Member{
		SessionID:  sess.ID,
		MemberType: mt,
		MemberID:   id,
		Role:       role,
		AddedAt:    h.now(),
		AddedBy:    actor,
	}
	return &memberPlan{Member: m, Share: &share}, nil
}

// checkMemberBoundary resolves a composed member's context share against the
// session's transcript and records the late-join answer. The boundary predicate
// is shared with the later-change path (shareBoundaryProblem) so the two write
// paths of this one field cannot disagree (DF-CRIER-472).
//
// Validate the boundary BEFORE the empty-session override: since must never
// silently become summary or none, even when there is no history.
//
// It is a separate step from composeMember because it is the one check that
// needs the room's RECORDED state — for a create that state is the empty
// transcript of the room not yet written, which is why a declared boundary can
// never resolve there (DF-CRIER-481).
func checkMemberBoundary(st *State, sessionID string, p *memberPlan) error {
	if detail := shareBoundaryProblem(st, sessionID, p.Share); detail != "" {
		return &memberError{status: http.StatusBadRequest, code: "CONTEXT_BOUNDARY_NOT_FOUND", msg: detail}
	}
	p.Reason = resolveJoinShare(st, p.Share)
	return nil
}

// appendMember writes an already-validated membership as the next record.
func (h *Handler) appendMember(ctx context.Context, sess *Session, p *memberPlan) error {
	seq, err := h.store.NextSeq(ctx, sess.ID)
	if err != nil {
		return err
	}
	rec := p.Member.AddRecord(seq, p.Member.AddedAt, p.Share)
	rec.Reason = p.Reason
	return h.store.Append(ctx, rec)
}

// addMember records one membership event (§2.3). A join to a session in
// another realm is refused, and so is adding an unknown agent or one in
// another realm: either would put two realms in one room.
func (h *Handler) addMember(ctx context.Context, sess *Session, req addParticipantRequest, actor string) error {
	plan, err := h.composeMember(sess, req, actor)
	if err != nil {
		return err
	}
	st, err := h.store.Load(ctx, sess.ID)
	if err != nil {
		return err
	}
	if err := checkMemberBoundary(st, sess.ID, plan); err != nil {
		return err
	}
	return h.appendMember(ctx, sess, plan)
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
	if err := strictjson.Decode(strings.NewReader(string(body)), v); err != nil {
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

// writeAPIError writes this surface's ONE error envelope: a named machine
// code in `error` and a human `detail`, so a client can branch on the code and
// a reader can tell WHAT was not found (DF-CRIER-480). Both fields are always
// written: an error body with a code and no detail is the shape that left the
// late-join context GET's callers unable to tell a wrong member from a wrong
// session from an unset share.
//
// The vocabulary is only this surface's answers. A 404 whose body carries
// neither field — most commonly the router's bare {"error":"not found"}
// (cmd/server/fallback.go) — did not reach a session handler at all: the
// request path did not match the route template (a trailing slash on the
// canonical path is the usual cause). No handler in this package writes that
// shape, and none may start to: it would be indistinguishable from a path miss.
func writeAPIError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}

// writeAudienceError renders an audience refusal with its own code where it has
// one: an unaddressable target kind is 400 UNKNOWN_TARGET_KIND (DF-CRIER-475)
// naming the accepted kinds, never the generic INVALID_AUDIENCE. Every other
// audience problem keeps that vocabulary.
func writeAudienceError(w http.ResponseWriter, err error) {
	var unknown *UnknownTargetKindError
	if errors.As(err, &unknown) {
		writeAPIError(w, http.StatusBadRequest, CodeUnknownTargetKind, err.Error())
		return
	}
	writeAPIError(w, http.StatusBadRequest, "INVALID_AUDIENCE", err.Error())
}

// writePartialCreateError renders the one outcome a POST /sessions must never
// misreport: the room EXISTS, so "nothing happened" would be a lie. It keeps the
// failure's own status and code (a store fault is 500 STORE_ERROR) and ADDS the
// surviving session id plus the members already recorded, so the caller can
// address the room it now owns instead of retrying into a 409 for a room it
// believes it never created (DF-CRIER-471/481).
func writePartialCreateError(w http.ResponseWriter, sess *Session, added []string, err error) {
	if added == nil {
		added = []string{}
	}
	detail := fmt.Sprintf("session %q WAS created (state %q) and %d member(s) were recorded (%s); the request failed afterwards: %v — the session is durable, address it by that id rather than retrying the create",
		sess.ID, sess.State, len(added), strings.Join(added, ", "), err)
	writeJSON(w, statusForMemberError(err), map[string]any{
		"error":         codeForMemberError(err),
		"detail":        detail,
		"session_id":    sess.ID,
		"members_added": added,
		"partial":       true,
	})
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
