package ingress

// handler.go — POST /ingress/messages (CRIER-INGRESS §2, §3, §6).
//
// The handler routes ONE call to exactly one of three destinations:
//
//	session      append the message to an existing session, through the same
//	             repository calls the session send handler makes;
//	new_session  create a session from a template, join its members, then
//	             append the message;
//	inbox        deliver to an agent's durable inbox (or a capability's pool)
//	             through the same registry delivery path
//	             POST /agents/{id}/inbox already uses.
//
// It never reimplements either write path. What it owns is the routing, the
// validation, and the idempotency ledger.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/session"
)

// Mode is the destination selector. Unknown values are refused, never
// defaulted (§2.2).
type Mode string

const (
	ModeSession    Mode = "session"
	ModeNewSession Mode = "new_session"
	ModeInbox      Mode = "inbox"
)

// Modes lists the three destinations in the order the spec presents them.
var Modes = []Mode{ModeSession, ModeNewSession, ModeInbox}

// InboxMessage is the composed message an inbox delivery carries. It is a
// value, not an HTTP request: the inbox route's own body decoding, guard and
// containment gates stay where they are, and an in-process caller hands the
// delivery path exactly what a decoded body would have produced.
type InboxMessage struct {
	Author         session.AuthorRef
	Kind           session.MessageKind
	Text           string
	Payload        json.RawMessage
	Tags           []string
	IdempotencyKey string
}

// InboxWriter is the narrow inbox surface ingress needs (§2.2): deliver one
// message to an agent, or to a capability's selected holder, and report the
// per-target outcomes. The outcome's InboxEntryID is the entry id the delivery
// path stored, which is what the route record and the response name.
type InboxWriter interface {
	Deliver(ctx context.Context, agentID, capability string, msg InboxMessage) ([]session.DeliveryOutcome, error)
}

// DispatchRequest is what a harness adapter (CRIER-HARNESS §4) is offered.
type DispatchRequest struct {
	Mode       Mode
	SessionID  string
	MessageID  string
	AgentID    string
	Capability string
	Author     session.AuthorRef
	Kind       session.MessageKind
	Text       string
	Payload    json.RawMessage
	Tags       []string
}

// DeliveryDispatch is the nil-able harness-dispatch hook. THIS ROW DOES NOT
// IMPLEMENT AN ADAPTER: CRIER-HARNESS-1 owns the hook, so the field exists and
// nothing here claims a message on its own. A nil field means no adapter is
// configured, and the report is {"mode":"store","state":"stored"} exactly as
// §2.1 describes.
type DeliveryDispatch interface {
	Dispatch(ctx context.Context, req DispatchRequest) (DeliveryReport, error)
}

// DeliveryReport is the §2.1 `delivery` object: where the message ended up
// beyond the durable store. "stored" is not a weaker success than
// "dispatched" — it is the honest statement that the transcript (or the inbox)
// is the record and no adapter claimed the message.
type DeliveryReport struct {
	Mode    string `json:"mode"`              // "store" | "harness"
	Harness string `json:"harness,omitempty"` // e.g. "opencode"
	State   string `json:"state"`             // "stored" | "dispatched" | "failed"
	Error   string `json:"error,omitempty"`
}

// Outcome is one destination's result. Two vocabularies meet here by design:
// §2.1's response names `result`, while §2.2 types the slice as
// []session.DeliveryOutcome, whose own JSON key is `outcome`. Both keys carry
// the SAME value, so a client written against either section reads it.
type Outcome struct {
	Target       string          `json:"target"`
	MemberType   string          `json:"member_type,omitempty"`
	MemberID     string          `json:"member_id,omitempty"`
	Result       session.Outcome `json:"result"`
	Outcome      session.Outcome `json:"outcome"`
	InboxEntryID string          `json:"inbox_entry_id,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	// Code is the additive diagnostic the delivery path records (for a stored
	// session message: INGRESS_STORED, i.e. "in the transcript, no inbox write").
	Code string `json:"code,omitempty"`
}

// CodeIngressStored marks the outcome of a message that is durable in the
// session transcript without an inbox delivery: it is what "accepted" means on
// this surface, and the code says so on the record rather than leaving a reader
// to infer a queue that was never written.
const CodeIngressStored = "INGRESS_STORED"

// Result is what the handler persists under the idempotency key and returns
// (§2.2).
type Result struct {
	Mode Mode `json:"mode"`
	// SessionID is the session the message landed in. Absent for mode=inbox,
	// which never touches a session.
	SessionID string `json:"session_id,omitempty"`
	// MessageID is the session message id, or — for mode=inbox — the inbox
	// entry id the delivery path stored.
	MessageID      string         `json:"message_id"`
	Seq            int64          `json:"seq,omitempty"`
	Outcomes       []Outcome      `json:"outcomes,omitempty"`
	CreatedSession bool           `json:"created_session"`
	Delivery       DeliveryReport `json:"delivery"`
}

// Handler serves POST /ingress/messages.
type Handler struct {
	// Repo is the session repository every session-mode write goes through.
	Repo session.Repository
	// Inbox is the delivery path mode=inbox calls.
	Inbox InboxWriter
	// Route records the ingress.route entry mode=inbox writes. Nil skips the
	// record (a test double or an embedding with no route log).
	Route RouteStore
	// Dispatch is the nil-able CRIER-HARNESS hook. Nil = no adapter.
	Dispatch DeliveryDispatch
	// Idem is the idempotency ledger. Nil disables key replay; the request's
	// key still rides the message record, so the session store's own dedupe
	// applies.
	Idem IdempotencyStore
	// Now is the clock; nil means time.Now().UTC.
	Now func() time.Time
	// Namespaces resolves and authorizes realms, the same registry the session
	// handler holds.
	Namespaces *namespace.Registry
	// Permissions is the optional delivery ACL (CHAT-PERMISSIONS). Nil is an
	// unarmed deployment: membership is the whole rule.
	Permissions *permissions.Checker
	// AllowedModes restricts the destinations this deployment accepts. Empty
	// means all three (§4).
	AllowedModes []Mode
	// MaxBodyBytes is the request-body cap. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int
	// NewID mints ids; nil uses a random 16-hex-digit generator with the
	// caller's prefix.
	NewID func(prefix string) (string, error)
}

// ServeHTTP implements the route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, ErrMethodNotAllowed.With("POST /ingress/messages is the only method on this path"))
		return
	}
	if h.Repo == nil {
		WriteError(w, ErrStoreError.With("no session repository is wired, so ingress cannot store anything"))
		return
	}

	body, err := readBody(w, r, h.maxBodyBytes())
	if err != nil {
		WriteError(w, err)
		return
	}
	raw, err := decodeRequest(body)
	if err != nil {
		WriteError(w, err)
		return
	}
	req, err := h.parseRequest(r, raw)
	if err != nil {
		WriteError(w, err)
		return
	}
	realm, err := h.realmOf(r, req)
	if err != nil {
		WriteError(w, err)
		return
	}

	payload, status, err := h.serve(r.Context(), req, realm)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

// serve runs one request under the idempotency rule and returns the exact body
// and status to answer with. A keyed request is one critical section: the lock
// is taken on the repository (so independent handles and processes agree), the
// ledger is consulted, the work runs, and only a SUCCESS is recorded. A failure
// that wrote nothing therefore leaves no key behind, and a retry resumes
// instead of duplicating (§6).
func (h *Handler) serve(ctx context.Context, req *Request, realm string) ([]byte, int, error) {
	if req.Key == "" || h.Idem == nil {
		res, status, err := h.execute(ctx, req, realm)
		if err != nil {
			return nil, 0, err
		}
		return encodeResult(res, status)
	}

	hash, err := requestHash(req, realm)
	if err != nil {
		return nil, 0, ErrStoreError.With("%v", err)
	}
	// The lock's session id is a synthetic "ingress:<realm>": the repository
	// lock is a (id, key) pair, and a new_session call has no session to name
	// yet. Using the repo lock — rather than a handler-local mutex — is what
	// makes two concurrent identical keys resolve to ONE session even across
	// processes (§6, "new_session race").
	release, err := h.Repo.LockMessage(ctx, "ingress:"+realm, req.Key)
	if err != nil {
		return nil, 0, ErrStoreError.With("could not take the idempotency lock: %v", err)
	}
	defer release()

	if rec, ok, err := h.Idem.Lookup(ctx, realm, req.Key); err != nil {
		return nil, 0, ErrStoreError.With("could not read the idempotency ledger: %v", err)
	} else if ok {
		if rec.RequestHash != hash {
			return nil, 0, ErrKeyReused.With("idempotency key %q was already used by a different request (%s); use a new key for a new message", req.Key, rec.RequestHash)
		}
		return rec.Result, rec.Status, nil
	}

	res, status, err := h.execute(ctx, req, realm)
	if err != nil {
		return nil, 0, err
	}
	body, status, err := encodeResult(res, status)
	if err != nil {
		return nil, 0, err
	}
	if err := h.Idem.Save(ctx, realm, req.Key, IdempotencyRecord{
		RequestHash: hash, Result: body, Status: status, CreatedAt: h.now(),
	}); err != nil {
		return nil, 0, ErrStoreError.With("the message was stored but its idempotency key could not be recorded: %v", err)
	}
	return body, status, nil
}

// execute routes one validated request to its destination.
func (h *Handler) execute(ctx context.Context, req *Request, realm string) (Result, int, error) {
	switch req.Mode {
	case ModeSession:
		return h.postToSession(ctx, req, realm)
	case ModeNewSession:
		return h.createAndPost(ctx, req, realm)
	case ModeInbox:
		return h.deliverToInbox(ctx, req)
	default:
		// Unreachable: parseRequest refuses anything else. Kept explicit so a
		// future mode cannot fall through to "session".
		return Result{}, 0, ErrModeUnknown.With("mode %q has no destination", req.Mode)
	}
}

// postToSession appends to an EXISTING session (§2.1 mode=session).
//
// The write is the session send handler's own: NextSeq then PostMessage on the
// same Repository, with the same record shape (kind, author, resolved audience,
// thread, tags). There is no second append and no second transcript format.
func (h *Handler) postToSession(ctx context.Context, req *Request, realm string) (Result, int, error) {
	st, sess, err := h.loadSession(ctx, req.SessionID, realm)
	if err != nil {
		return Result{}, 0, err
	}
	if sess.State == session.SessionClosed {
		return Result{}, 0, ErrSessionClosed.With("session %q is closed and admits no new message (CHAT-SESSIONS §1.3)", sess.ID)
	}
	threadID, parentID, err := resolveReply(st, req.ReplyTo, sess.ID)
	if err != nil {
		return Result{}, 0, err
	}
	if err := h.requireParticipant(ctx, sess, st, req.Author); err != nil {
		return Result{}, 0, err
	}

	msgID, err := h.newID("m_")
	if err != nil {
		return Result{}, 0, ErrStoreError.With("%v", err)
	}
	if threadID == "" {
		threadID = msgID
	}
	now := h.now()
	recipients := agentRecipients(st, req.Author, now)
	msg := &session.Message{
		ID:             msgID,
		ThreadID:       threadID,
		ParentID:       parentID,
		SessionID:      sess.ID,
		Kind:           req.Kind,
		Author:         req.Author,
		Payload:        req.Payload,
		Audience:       audienceOf(recipients),
		Outcomes:       storedOutcomes(recipients, now),
		Tags:           req.Tags,
		IdempotencyKey: req.Key,
		CreatedAt:      now,
	}

	seq, err := h.Repo.NextSeq(ctx, sess.ID)
	if err != nil {
		return Result{}, 0, ErrStoreError.With("%v", err)
	}
	msg.Seq = seq
	if err := h.Repo.PostMessage(ctx, msg); err != nil {
		return Result{}, 0, ErrStoreError.With("append to session %q failed: %v", sess.ID, err)
	}

	return h.finish(ctx, req, Result{
		Mode:      ModeSession,
		SessionID: sess.ID,
		MessageID: msg.ID,
		Seq:       seq,
		Outcomes:  outcomeViews(msg.Outcomes, nil),
	}, http.StatusOK)
}

// createAndPost creates a session from a template and then appends (§2.1
// mode=new_session).
//
// Ordering is deliberate and matches the create handler's: the session record
// first, then membership, then the message. A member-add failure therefore
// leaves REAL records behind — the session and the members already joined — and
// appends NO message (§6 "Partial member add"). The idempotency key is not
// recorded on a failure, so a retry resumes rather than duplicating.
func (h *Handler) createAndPost(ctx context.Context, req *Request, realm string) (Result, int, error) {
	tmpl := req.Template
	now := h.now()
	id, err := h.newID("s_")
	if err != nil {
		return Result{}, 0, ErrStoreError.With("%v", err)
	}
	sess := &session.Session{
		ID:        id,
		Namespace: realm,
		Kind:      session.Kind(tmpl.Kind),
		Title:     tmpl.Title,
		CreatedAt: now,
		CreatedBy: req.Author,
		State:     session.SessionOpen,
	}
	if err := h.Repo.CreateSession(ctx, sess, 1); err != nil {
		return Result{}, 0, ErrStoreError.With("create session failed: %v", err)
	}

	// The author auto-joins with what a normal create+join grants (member): the
	// message it is about to post names it as a participant, and a sender that
	// is not in the room could not read its own message back.
	seq := int64(1)
	joined := map[string]bool{}
	join := func(m session.Member, label string) error {
		key := string(m.MemberType) + ":" + m.MemberID
		if joined[key] {
			return nil
		}
		seq++
		m.SessionID = sess.ID
		m.AddedAt = now
		m.AddedBy = req.Author.AgentID()
		actor := req.Author
		m.AddedByActor = &actor
		if err := h.Repo.AddMember(ctx, &m, seq, nil); err != nil {
			return ErrStoreError.With("session %q exists but member %s could not be added: %v", sess.ID, label, err)
		}
		joined[key] = true
		return nil
	}

	if req.Author.Agent != "" {
		if err := join(session.Member{MemberType: session.MemberAgent, MemberID: req.Author.Agent, Role: session.RoleMember}, "agent:"+req.Author.Agent); err != nil {
			return Result{}, 0, err
		}
	} else if req.Author.AsAgent != "" && req.Author.Principal == "" {
		if err := join(session.Member{MemberType: session.MemberAgent, MemberID: req.Author.AsAgent, Role: session.RoleMember}, "agent:"+req.Author.AsAgent); err != nil {
			return Result{}, 0, err
		}
	}
	if req.Author.Principal != "" {
		if err := join(session.Member{MemberType: session.MemberPrincipal, MemberID: req.Author.Principal, Role: session.RoleMember}, "principal:"+req.Author.Principal); err != nil {
			return Result{}, 0, err
		}
		if req.Author.AsAgent != "" {
			if err := join(session.Member{MemberType: session.MemberAgent, MemberID: req.Author.AsAgent, Role: session.RoleMember}, "agent:"+req.Author.AsAgent); err != nil {
				return Result{}, 0, err
			}
		}
	}
	declared := make([]session.Member, 0, len(tmpl.Members))
	for _, m := range tmpl.Members {
		if err := join(m, string(m.MemberType)+":"+m.MemberID); err != nil {
			return Result{}, 0, err
		}
		declared = append(declared, m)
	}

	msgID, err := h.newID("m_")
	if err != nil {
		return Result{}, 0, ErrStoreError.With("%v", err)
	}
	seq, err = h.Repo.NextSeq(ctx, sess.ID)
	if err != nil {
		return Result{}, 0, ErrStoreError.With("%v", err)
	}
	msg := &session.Message{
		ID:        msgID,
		ThreadID:  msgID,
		SessionID: sess.ID,
		Kind:      req.Kind,
		Author:    req.Author,
		Payload:   req.Payload,
		Audience:  audienceOf(memberTargets(declared)),
		Outcomes:  memberOutcomes(declared, now),
		Tags:      req.Tags,
		CreatedAt: now,
		Seq:       seq,
	}
	msg.IdempotencyKey = req.Key
	if err := h.Repo.PostMessage(ctx, msg); err != nil {
		return Result{}, 0, ErrStoreError.With("session %q exists with its members, but the message could not be appended: %v", sess.ID, err)
	}

	return h.finish(ctx, req, Result{
		Mode:           ModeNewSession,
		SessionID:      sess.ID,
		MessageID:      msg.ID,
		Seq:            seq,
		Outcomes:       outcomeViews(msg.Outcomes, memberKinds(declared)),
		CreatedSession: true,
	}, http.StatusCreated)
}

// deliverToInbox hands the message to the SAME delivery path POST
// /agents/{id}/inbox uses (capability form included) and records where it
// landed (§2.1 mode=inbox, §3 ingress.route).
func (h *Handler) deliverToInbox(ctx context.Context, req *Request) (Result, int, error) {
	if h.Inbox == nil {
		return Result{}, 0, ErrStoreError.With("no inbox delivery path is wired, so mode=inbox cannot deliver")
	}
	outcomes, err := h.Inbox.Deliver(ctx, req.AgentID, req.Capability, InboxMessage{
		Author:         req.Author,
		Kind:           req.Kind,
		Text:           req.Text,
		Payload:        req.Payload,
		Tags:           req.Tags,
		IdempotencyKey: req.Key,
	})
	if err != nil {
		if errors.Is(err, ErrAgentUnknown) {
			return Result{}, 0, ErrAgentUnknown.With("%v", err)
		}
		return Result{}, 0, ErrStoreError.With("inbox delivery failed: %v", err)
	}
	entryID := ""
	target := req.AgentID
	for _, o := range outcomes {
		if o.InboxEntryID != "" && entryID == "" {
			entryID = o.InboxEntryID
		}
		if o.Target != "" {
			target = o.Target
		}
	}
	if entryID == "" {
		return Result{}, 0, ErrStoreError.With("the inbox path accepted the message but named no inbox entry id")
	}
	if target == "" {
		target = req.AgentID
	}

	if h.Route != nil {
		routeID, err := h.newID("r_")
		if err != nil {
			return Result{}, 0, ErrStoreError.With("%v", err)
		}
		rec := RouteRecord{
			ID:         routeID,
			AgentID:    target,
			Capability: req.Capability,
			MessageID:  entryID,
			Author:     req.Author,
			Tags:       req.Tags,
			CreatedAt:  h.now(),
		}
		if err := h.Route.RecordRoute(ctx, rec); err != nil {
			// The entry IS stored; the route record is the receipt that says
			// where. Reporting success without it would be a claim the server
			// cannot stand behind, so this is a 500 that names the entry id.
			return Result{}, 0, ErrStoreError.With("inbox entry %q was stored but its ingress.route record failed: %v", entryID, err)
		}
	}

	return h.finish(ctx, req, Result{
		Mode:      ModeInbox,
		MessageID: entryID,
		Outcomes:  outcomeViews(outcomes, nil),
	}, http.StatusCreated)
}

// finish attaches the delivery report to a stored result. Harness dispatch is
// best-effort and reported HONESTLY (§6): the message is already durable, so a
// dispatch failure is a `failed` report on a successful call, never a rolled
// back write and never a silent "dispatched".
func (h *Handler) finish(ctx context.Context, req *Request, res Result, status int) (Result, int, error) {
	res.Delivery = DeliveryReport{Mode: "store", State: "stored"}
	if h.Dispatch == nil {
		return res, status, nil
	}
	report, err := h.Dispatch.Dispatch(ctx, DispatchRequest{
		Mode: req.Mode, SessionID: res.SessionID, MessageID: res.MessageID,
		AgentID: req.AgentID, Capability: req.Capability, Author: req.Author,
		Kind: req.Kind, Text: req.Text, Payload: req.Payload, Tags: req.Tags,
	})
	if err != nil {
		res.Delivery = DeliveryReport{Mode: "harness", State: "failed", Error: err.Error()}
		return res, status, nil
	}
	if report.Mode == "" {
		report.Mode = "harness"
	}
	if report.State == "" {
		report.State = "dispatched"
	}
	res.Delivery = report
	return res, status, nil
}

// ---------------------------------------------------------------------------
// Store-bound helpers.
// ---------------------------------------------------------------------------

// loadSession resolves a session by id inside the request's realm. A session in
// another realm is a 404, never a 403 — the same rule the session routes apply,
// because confirming it exists would cross the realm wall.
func (h *Handler) loadSession(ctx context.Context, id, realm string) (*session.State, *session.Session, error) {
	st, err := h.Repo.Load(ctx, id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, nil, ErrSessionNotFound.With("session %q does not exist", id)
		}
		return nil, nil, ErrStoreError.With("load session %q: %v", id, err)
	}
	if st == nil || st.Session == nil {
		return nil, nil, ErrSessionNotFound.With("session %q does not exist", id)
	}
	if namespace.Canonical(st.Session.Namespace) != realm {
		return nil, nil, ErrSessionNotFound.With("session %q does not exist in this realm", id)
	}
	return st, st.Session, nil
}

// resolveReply turns `reply_to_thread` into (thread, parent). A reply stays in
// its thread (CHAT-SESSIONS §4.5): the message is attached to the thread's ROOT,
// so naming a thread never deepens it and a thread that is not in this session
// (or belongs to another room) is INGRESS_REPLY_TOO_DEEP, never a silent re-root.
func resolveReply(st *session.State, threadID, sessionID string) (string, string, error) {
	if threadID == "" {
		return "", "", nil
	}
	thread := st.Thread(threadID)
	if thread == nil || thread.SessionID != sessionID {
		return "", "", ErrReplyTooDeep.With("thread %q is not in session %q", threadID, sessionID)
	}
	root := thread.RootMessageID
	if root == "" {
		root = thread.ID
	}
	if st.Message(root) == nil {
		return "", "", ErrReplyTooDeep.With("thread %q has no root message in session %q, so a reply has nothing to attach to", threadID, sessionID)
	}
	return thread.ID, root, nil
}

// requireParticipant is the mode=session author gate: the author must be an
// active participant, or — when the CHAT-PERMISSIONS ACL is armed — permitted to
// SEND into this session. It is the same question the session send path asks,
// answered against the same membership projection.
func (h *Handler) requireParticipant(ctx context.Context, sess *session.Session, st *session.State, author session.AuthorRef) error {
	now := h.now()
	agentID := author.Agent
	if agentID == "" {
		agentID = author.AsAgent
	}
	for _, m := range st.Members {
		if !m.ActiveAt(now) {
			continue
		}
		if agentID != "" && m.MemberType == session.MemberAgent && m.MemberID == agentID {
			return nil
		}
		if author.Principal != "" && m.MemberType == session.MemberPrincipal && m.MemberID == author.Principal {
			return nil
		}
	}
	if h.Permissions != nil && h.Permissions.Armed() {
		sender := permissions.EffectiveSender{Kind: permissions.SenderAnonymous}
		switch {
		case author.Agent != "":
			sender = permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: author.Agent}
		case author.Principal != "":
			sender = permissions.EffectiveSender{Kind: permissions.SenderPrincipal, Principal: author.Principal, AsAgent: author.AsAgent}
		}
		res, err := h.Permissions.MayDeliver(ctx, permissions.CheckInput{
			Sender:  sender,
			Action:  permissions.ActionSend,
			Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID},
		})
		if err == nil && res.Allowed {
			return nil
		}
	}
	return ErrAuthorNotMember.With("author %s is neither an active participant of session %q nor permitted to post into it (CHAT-PERMISSIONS)", describeAuthor(author), sess.ID)
}

// agentRecipients is the audience of a message posted into an existing session:
// every ACTIVE agent participant except the author, sorted so two runs of the
// same membership record the same audience.
func agentRecipients(st *session.State, author session.AuthorRef, now time.Time) []session.AudienceTarget {
	self := author.AgentID()
	seen := map[string]bool{}
	out := make([]session.AudienceTarget, 0, len(st.Members))
	for _, m := range st.Members {
		if m.MemberType != session.MemberAgent || !m.ActiveAt(now) {
			continue
		}
		if m.MemberID == self || seen[m.MemberID] {
			continue
		}
		seen[m.MemberID] = true
		out = append(out, session.AudienceTarget{Kind: session.TargetAgent, ID: m.MemberID})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// memberTargets is the audience of a new session's first message: the members
// the caller declared, as agents and principals, in declaration order.
func memberTargets(members []session.Member) []session.AudienceTarget {
	seen := map[string]bool{}
	out := make([]session.AudienceTarget, 0, len(members))
	for _, m := range members {
		kind := session.TargetAgent
		if m.MemberType == session.MemberPrincipal {
			kind = session.TargetPrincipal
		}
		key := string(kind) + ":" + m.MemberID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, session.AudienceTarget{Kind: kind, ID: m.MemberID})
	}
	return out
}

// memberKinds maps each declared member's id to its member type, so the
// response can report a principal destination as a principal rather than
// re-reading its id.
func memberKinds(members []session.Member) map[string]session.MemberType {
	out := make(map[string]session.MemberType, len(members))
	for _, m := range members {
		out[m.MemberID] = m.MemberType
	}
	return out
}

func audienceOf(targets []session.AudienceTarget) session.Audience {
	return session.Audience{Rule: session.AudienceExplicit, Targets: targets}
}

// storedOutcomes renders one accepted outcome per audience target. "accepted"
// on this surface means the message is DURABLE in the session — the transcript
// every participant reads — and the code says so; it is not a claim that an
// inbox entry was written, because this path writes none.
func storedOutcomes(targets []session.AudienceTarget, at time.Time) []session.DeliveryOutcome {
	out := make([]session.DeliveryOutcome, 0, len(targets))
	for _, t := range targets {
		out = append(out, session.DeliveryOutcome{
			Target: t.ID, Outcome: session.OutcomeAccepted, Code: CodeIngressStored, UpdatedAt: at,
		})
	}
	return out
}

// memberOutcomes renders one accepted outcome per DECLARED template member: the
// destinations the caller named in this call. The author's own implicit join is
// not a destination — it is the sender — so it is not listed.
func memberOutcomes(members []session.Member, at time.Time) []session.DeliveryOutcome {
	seen := map[string]bool{}
	out := make([]session.DeliveryOutcome, 0, len(members))
	for _, m := range members {
		key := string(m.MemberType) + ":" + m.MemberID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, session.DeliveryOutcome{
			Target: m.MemberID, Outcome: session.OutcomeAccepted, Code: CodeIngressStored, UpdatedAt: at,
		})
	}
	return out
}

// outcomeViews renders the stored outcomes for the response, filling the
// member-facing fields §2.1 shows (member_type/member_id). `kinds` names the
// member type of each target where the caller knows it (a principal target is
// not an agent); an unknown target is an agent, which is what every fan-out
// target in this package is by default.
func outcomeViews(outcomes []session.DeliveryOutcome, kinds map[string]session.MemberType) []Outcome {
	if len(outcomes) == 0 {
		return nil
	}
	out := make([]Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		result := o.Outcome
		if result == "" {
			result = session.OutcomeAccepted
		}
		memberType := session.MemberAgent
		if kind, ok := kinds[o.Target]; ok {
			memberType = kind
		}
		out = append(out, Outcome{
			Target:       o.Target,
			MemberType:   string(memberType),
			MemberID:     o.Target,
			Result:       result,
			Outcome:      result,
			InboxEntryID: o.InboxEntryID,
			Reason:       o.Reason,
			Code:         o.Code,
		})
	}
	return out
}

// realmOf resolves the realm the request acts in:
//
//	mode=new_session  the template's namespace (validated: an undeclared one is
//	                  INGRESS_TEMPLATE_INVALID, §5);
//	the other modes   the request realm (X-Crier-Namespace), the same rule the
//	                  session routes apply.
func (h *Handler) realmOf(r *http.Request, req *Request) (string, error) {
	bodyNS := ""
	if req.Mode == ModeNewSession && req.Template != nil {
		bodyNS = req.Template.Namespace
	}
	realm, err := session.RealmRule(r, bodyNS, h.Namespaces)
	if err == nil {
		return realm, nil
	}
	switch {
	case errors.Is(err, namespace.ErrUnknownNamespace) && req.Mode == ModeNewSession:
		return "", ErrTemplateInvalid.With("target.template.namespace %q does not exist", bodyNS)
	case errors.Is(err, namespace.ErrUnknownNamespace):
		return "", ErrNamespaceUnknown.With("%v", err)
	case errors.Is(err, namespace.ErrUnauthorized):
		return "", ErrNamespaceUnauthorized.With("%v", err)
	default:
		return "", ErrNamespaceUnknown.With("%v", err)
	}
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) maxBodyBytes() int {
	if h.MaxBodyBytes > 0 {
		return h.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (h *Handler) modeAllowed(mode Mode) bool {
	if len(h.AllowedModes) == 0 {
		return true
	}
	for _, allowed := range h.AllowedModes {
		if allowed == mode {
			return true
		}
	}
	return false
}

func (h *Handler) newID(prefix string) (string, error) {
	if h.NewID != nil {
		return h.NewID(prefix)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func describeAuthor(a session.AuthorRef) string {
	switch {
	case a.Agent != "":
		return "agent:" + a.Agent
	case a.Principal != "" && a.AsAgent != "":
		return "principal:" + a.Principal + " as agent:" + a.AsAgent
	case a.Principal != "":
		return "principal:" + a.Principal
	case a.System != "":
		return "system:" + a.System
	default:
		return "(anonymous)"
	}
}

// encodeResult marshals one result, refusing to answer with a body it could not
// encode (that would be a 200 with no content for a write that DID happen).
func encodeResult(res Result, status int) ([]byte, int, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return nil, 0, ErrStoreError.With("the write succeeded but its result could not be encoded: %v", err)
	}
	return body, status, nil
}
