// compile.go — compile (merge) messages into ONE context bundle, with a
// provenance citation per part (CR-CHAT-028).
//
// A COMPILE selects N messages — from one thread, from several, or from what
// the humans said — MERGES them into ONE payload, and HANDS that payload to an
// agent by tagging it. It is deliberately NOT the same affordance as a quote or
// a reply: a reply answers "in answer to X" with an id pointer, while a compile
// answers "here is everything relevant, gathered" with the content INLINE plus
// the citation that says where each part came from. Both are needed.
//
// The design rule that makes a merged message safe (specs/CHAT-SESSIONS.md
// §4.8) is enforced here, not merely documented:
//
//   - the merged message is a NEW message. The sources are never mutated,
//     re-parented or moved: they stay resolvable to anyone who may read them;
//   - every part keeps a PROVENANCE citation {source_session_id,
//     source_message_id}, so a recipient can always expand a part back to where
//     it came from (GET /sessions/{id}/messages/{mid}/expand);
//   - a source the COMPILER could not read is OMITTED WITH A NAMED GAP — the
//     part is still emitted, citing its source and carrying a machine-readable
//     reason — and a merge whose every source is unreadable is REFUSED. A
//     source is never silently included and never silently dropped;
//   - tagging an agent is ADDRESSING only (D12): a compiled bundle is `plain`
//     or `addressed` and can never be `task` — a tag on a compile is never an
//     instruction to execute anything.
//
// The compiled message rides the SHIPPED delivery path exactly as any other
// session message does (Handler.sendMessage → Fanout → registry.Store.Deliver):
// there is no second delivery path (CHAT-INTERFACE.md §5.1).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// CompiledBundleKind is the payload discriminator a compiled message carries in
// its payload's `kind` field. It is data, not inference: a reader can tell a
// compiled bundle from any other opaque payload without reading its text.
const CompiledBundleKind = "compiled"

// ---------------------------------------------------------------------------
// The wire shapes.
// ---------------------------------------------------------------------------

// CompilePart is ONE source folded into a compiled bundle, with its provenance
// citation. A part the compiler could read carries the source's content inline
// and cites it (`cited: true`). A part the compiler could NOT read is still
// emitted — citing the source it named, `cited: false`, and an `omitted` reason
// naming exactly why. There is no third shape: a source is never absent from
// the bundle without a reason, and never present without its citation.
type CompilePart struct {
	SourceSessionID string          `json:"source_session_id"`
	SourceMessageID string          `json:"source_message_id"`
	Cited           bool            `json:"cited"`
	Content         json.RawMessage `json:"content,omitempty"`
	Author          *AuthorRef      `json:"author,omitempty"`
	CreatedAt       *time.Time      `json:"created_at,omitempty"`
	// Omitted names the gap when the part could not be read: the source
	// session or message was not found in the request's realm, or the compiler
	// was not permitted to read it. Present exactly when Cited is false.
	Omitted string `json:"omitted,omitempty"`
}

// CompiledBundle is the payload of a compiled message: ONE message that gathers
// N cited sources. It is the merge's whole surface — a recipient renders the
// parts and expands any of them back to its source.
type CompiledBundle struct {
	Kind string `json:"kind"`
	// Text is the compiler's optional note. It is the payload's `text` field,
	// so the shipped preview convention (payload.text) renders a compiled
	// message like any other.
	Text  string        `json:"text,omitempty"`
	Parts []CompilePart `json:"parts"`
	// Count is len(Parts); OmittedCount is how many of those carry a named
	// gap. Both are reported so a caller can see the merge's completeness
	// without walking the parts.
	Count        int `json:"count"`
	OmittedCount int `json:"omitted_count"`
}

// compileSourceRef names one source message: a session and a message id. The
// session may be this session (the shorthand form) or another one (the
// cross-thread / cross-session merge).
type compileSourceRef struct {
	SessionID string `json:"session_id,omitempty"`
	MessageID string `json:"message_id"`
}

// compileRequest is the POST /sessions/{id}/compile body.
type compileRequest struct {
	// MessageIDs is the shorthand form: messages of THIS session, gathered
	// across threads or within one.
	MessageIDs []string `json:"message_ids,omitempty"`
	// Sources is the explicit form, for a source in another session of the
	// same realm.
	Sources []compileSourceRef `json:"sources,omitempty"`
	// Note is the compiler's optional note (the bundle's `text`).
	Note           string           `json:"note,omitempty"`
	Sender         string           `json:"sender,omitempty"`
	PrincipalID    string           `json:"principal_id,omitempty"`
	AsAgent        string           `json:"as_agent,omitempty"`
	MessageKind    string           `json:"message_kind,omitempty"`
	Targets        []AudienceTarget `json:"targets,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
}

// compileResponse is the POST /sessions/{id}/compile answer: the ONE new
// recorded message, plus the completeness of its bundle. The gaps are repeated
// at the top level so a caller that does not walk the payload still sees them.
type compileResponse struct {
	Message       transcriptMessage `json:"message"`
	PartsCount    int               `json:"parts_count"`
	ResolvedCount int               `json:"resolved_count"`
	OmittedCount  int               `json:"omitted_count"`
	Gaps          []string          `json:"gaps,omitempty"`
}

// expandPart is one citation resolved back to its ORIGINAL: the content and the
// location (session, message id, author, timestamp, seq, thread) it came from.
// An unreadable source is reported as the same part with `resolved: false` and
// a named gap — never as an empty success.
type expandPart struct {
	SourceSessionID string          `json:"source_session_id"`
	SourceMessageID string          `json:"source_message_id"`
	Cited           bool            `json:"cited"`
	Resolved        bool            `json:"resolved"`
	Content         json.RawMessage `json:"content,omitempty"`
	Author          *AuthorRef      `json:"author,omitempty"`
	CreatedAt       *time.Time      `json:"created_at,omitempty"`
	Seq             int64           `json:"seq,omitempty"`
	ThreadID        string          `json:"thread_id,omitempty"`
	Omitted         string          `json:"omitted,omitempty"`
}

// expandResponse is the GET /sessions/{id}/messages/{mid}/expand body.
type expandResponse struct {
	MessageID     string       `json:"message_id"`
	SessionID     string       `json:"session_id"`
	Parts         []expandPart `json:"parts"`
	Count         int          `json:"count"`
	ResolvedCount int          `json:"resolved_count"`
	OmittedCount  int          `json:"omitted_count"`
}

// ---------------------------------------------------------------------------
// POST /sessions/{id}/compile — merge N cited sources into ONE message.
// ---------------------------------------------------------------------------

// HandleCompile serves POST /sessions/{id}/compile: it gathers the named
// sources into ONE new message in the target session and fans it out through
// the shipped delivery path. The sources are left exactly as they were — a
// compile never mutates, moves or re-parents a source.
//
// The merge is auditable at every step: each part carries its citation, an
// unreadable source becomes a part with a NAMED gap, and a merge with nothing
// readable is refused (422 COMPILE_NO_READABLE_SOURCE) rather than shipped as
// an empty shell.
func (h *Handler) HandleCompile(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.loadOpenScoped(w, r)
	if !ok {
		return
	}
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}
	var req compileRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if h.opts.Deliverer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SESSION_DELIVERY_UNCONFIGURED",
			"no inbox deliverer is wired, so a compiled message cannot be fanned out")
		return
	}

	author, ok := resolveAuthor(nil, req.Sender, req.PrincipalID, req.AsAgent)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "AUTHOR_REQUIRED",
			"sender (an agent id), or principal_id (optionally with as_agent), is required: a compiled message records its author")
		return
	}

	refs, err := compileRefs(req, sess.ID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if len(refs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"at least one source is required: name message_ids (messages of this session) and/or sources ({session_id, message_id})")
		return
	}

	// D12: a compile GATHERS context; it never instructs. `task` is the only
	// kind that may create work, and a compiled bundle is never one — a tag on
	// it is addressing, not an action (specs/CHAT-SESSIONS.md §4.4, §4.8).
	kind := MessageKind(strings.TrimSpace(req.MessageKind))
	switch kind {
	case "":
		if len(req.Targets) > 0 {
			kind = MessageAddressed
		} else {
			kind = MessagePlain
		}
	case MessagePlain, MessageAddressed:
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_MESSAGE_KIND",
			fmt.Sprintf("message_kind must be %q or %q — a compiled bundle is addressing, never an instruction to execute (D12, specs/CHAT-SESSIONS.md §4.4)", MessagePlain, MessageAddressed))
		return
	}

	// Resolve every citation. An unreadable source becomes a part with a NAMED
	// gap; the merge is never silent about either outcome.
	parts := make([]CompilePart, 0, len(refs))
	var gaps []string
	resolved := 0
	for _, ref := range refs {
		res, err := h.resolveCompileSource(r.Context(), realm, author, sess.ID, ref)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if res.Resolved() {
			resolved++
			at := res.CreatedAt
			au := res.Author
			parts = append(parts, CompilePart{
				SourceSessionID: res.SessionID,
				SourceMessageID: res.MessageID,
				Cited:           true,
				Content:         res.Content,
				Author:          &au,
				CreatedAt:       &at,
			})
			continue
		}
		gaps = append(gaps, res.Gap)
		parts = append(parts, CompilePart{
			SourceSessionID: res.SessionID,
			SourceMessageID: res.MessageID,
			Cited:           false,
			Omitted:         res.Gap,
		})
	}

	// A bundle with nothing readable is not a merge. It is refused, WITH the
	// named gaps — never shipped as an empty shell that hides what happened.
	if resolved == 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, "COMPILE_NO_READABLE_SOURCE",
			"no source could be read: "+strings.Join(gaps, "; "))
		return
	}

	bundle := CompiledBundle{
		Kind:         CompiledBundleKind,
		Text:         req.Note,
		Parts:        parts,
		Count:        len(parts),
		OmittedCount: len(parts) - resolved,
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	if err := validatePayload(payload); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}

	msgID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	now := h.now()
	msg := &Message{
		ID:        msgID,
		SessionID: sess.ID,
		Kind:      kind,
		Author:    author,
		Payload:   payload,
		CreatedAt: now,
		// A compile is a NEW message: it opens its own thread and is not a
		// reply to any source. Citation is not reply attribution (§4.8).
		ThreadID:       msgID,
		IdempotencyKey: req.IdempotencyKey,
	}

	aud, err := h.resolveAudience(r.Context(), sess, &postMessageRequest{Targets: req.Targets}, author.AgentID())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_AUDIENCE", err.Error())
		return
	}

	view, ok := h.sendMessage(w, r, sess, msg, aud)
	if !ok {
		return
	}
	view.Outcomes = outcomeViews(msg.Outcomes)

	writeJSON(w, http.StatusCreated, compileResponse{
		Message:       view,
		PartsCount:    len(parts),
		ResolvedCount: resolved,
		OmittedCount:  len(parts) - resolved,
		Gaps:          gaps,
	})
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/messages/{mid}/expand — citation → source.
// ---------------------------------------------------------------------------

// HandleExpandCompiledMessage serves GET /sessions/{id}/messages/{mid}/expand:
// it resolves every part of a compiled message back to the ORIGINAL it came
// from, reporting the source's content and location (session, message id,
// author, timestamp, seq, thread).
//
// The resolution runs as the CALLER, not as the compiler: a part whose source
// the caller may not read is reported with a NAMED gap rather than served. That
// is the design rule's second half — a source stays resolvable to anyone who
// may read it.
func (h *Handler) HandleExpandCompiledMessage(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}

	mid := muxVar(r, "mid")
	m := st.Message(mid)
	if m == nil {
		writeAPIError(w, http.StatusNotFound, "MESSAGE_NOT_FOUND",
			fmt.Sprintf("message %q is not in session %q", mid, sess.ID))
		return
	}

	var bundle CompiledBundle
	if err := json.Unmarshal(m.Payload, &bundle); err != nil || bundle.Kind != CompiledBundleKind {
		writeAPIError(w, http.StatusBadRequest, "NOT_COMPILED",
			fmt.Sprintf("message %q is not a compiled bundle", mid))
		return
	}

	who := callerIdentity(r)
	out := expandResponse{MessageID: m.ID, SessionID: sess.ID, Parts: make([]expandPart, 0, len(bundle.Parts))}
	for _, part := range bundle.Parts {
		ref := compileSourceRef{SessionID: part.SourceSessionID, MessageID: part.SourceMessageID}
		res, err := h.resolveCompileSource(r.Context(), realm, who, sess.ID, ref)
		if err != nil {
			// The compiler wrote this citation, so a malformed one cannot
			// normally reach here; if it does it is reported as a named gap
			// rather than a 500.
			res = sourceResolution{SessionID: ref.SessionID, MessageID: ref.MessageID, Gap: err.Error()}
		}
		ep := expandPart{
			SourceSessionID: res.SessionID,
			SourceMessageID: res.MessageID,
			Cited:           res.Resolved(),
			Resolved:        res.Resolved(),
		}
		if res.Resolved() {
			at := res.CreatedAt
			au := res.Author
			ep.Content = res.Content
			ep.Author = &au
			ep.CreatedAt = &at
			ep.Seq = res.Seq
			ep.ThreadID = res.ThreadID
			out.ResolvedCount++
		} else {
			ep.Omitted = res.Gap
			out.OmittedCount++
		}
		out.Parts = append(out.Parts, ep)
	}
	out.Count = len(out.Parts)
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Source resolution — the ONE place a citation becomes content or a named gap.
// ---------------------------------------------------------------------------

// sourceResolution is what one citation resolved to: the source message's
// content and location, or the named gap explaining why it could not be read.
type sourceResolution struct {
	SessionID string
	MessageID string
	Content   json.RawMessage
	Author    AuthorRef
	CreatedAt time.Time
	Seq       int64
	ThreadID  string
	// Gap names the reason when the source could not be read. Empty means
	// Resolved.
	Gap string
}

// Resolved reports whether the source was read.
func (s sourceResolution) Resolved() bool { return s.Gap == "" }

// resolveCompileSource resolves ONE citation to its original message.
//
// It returns an ERROR only for a citation the caller malformed (a source with
// no message id) — a request defect, refused up front. Every other failure to
// read the source is a NAMED GAP on the returned value, because "the compiler
// could not read it" is a fact about the merge that the bundle must carry, not
// a reason to fail the request:
//
//   - the source session is not in the request's realm (the §4 row 38 realm
//     wall) — the compiler cannot reach across it;
//   - the source session does not exist, or has no record;
//   - the source session is private and the compiler is neither a member nor
//     granted a read on it (the permission the compiler lacks, §4 row 13);
//   - the source message id is not in that session (a retention hole, or an
//     id that never existed).
//
// PERMISSION HOOK POINT (CR-CHAT-003): the delivery ACL is not deployed in the
// shipped posture (HTTPOptions.Permissions may be nil). When it is armed,
// authorizeReadAs already consults it for a private source session; a per-part
// grant check for a PUBLIC source belongs here, and it must land in the SAME
// branch as the member check does — the source is omitted with a named gap,
// never silently included.
func (h *Handler) resolveCompileSource(ctx context.Context, realm string, who AuthorRef, defaultSessionID string, ref compileSourceRef) (sourceResolution, error) {
	mid := strings.TrimSpace(ref.MessageID)
	if mid == "" {
		return sourceResolution{}, errors.New("a compile source needs a non-empty message_id")
	}
	sid := strings.TrimSpace(ref.SessionID)
	if sid == "" {
		sid = defaultSessionID
	}
	res := sourceResolution{SessionID: sid, MessageID: mid}

	st, err := h.store.Load(ctx, sid)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			res.Gap = fmt.Sprintf("source session %q does not exist in this realm", sid)
			return res, nil
		}
		return sourceResolution{}, err
	}
	if st.Session == nil {
		res.Gap = fmt.Sprintf("source session %q has no session record", sid)
		return res, nil
	}
	if namespace.Canonical(st.Session.Namespace) != realm {
		res.Gap = fmt.Sprintf("source session %q is in another namespace than the request's realm", sid)
		return res, nil
	}
	if err := h.authorizeReadAs(ctx, who, st.Session, st); err != nil {
		res.Gap = fmt.Sprintf("source session %q is not readable by the compiler: %v", sid, err)
		return res, nil
	}
	m := st.Message(mid)
	if m == nil {
		res.Gap = fmt.Sprintf("source message %q is not in session %q", mid, sid)
		return res, nil
	}
	res.Content = m.Payload
	res.Author = m.Author
	res.CreatedAt = m.CreatedAt
	res.Seq = m.Seq
	res.ThreadID = m.ThreadID
	return res, nil
}

// compileRefs flattens a compile request into ordered, de-duplicated source
// citations: the same-session shorthand (message_ids) first, then the explicit
// cross-session form (sources). A shorthand source is recorded with the TARGET
// session's id, so a citation in the bundle always names the session it came
// from — never an empty field a later reader has to resolve again — and the
// same source named in both forms is gathered once.
//
// A source with no message id is an error, not a skip: the caller named
// something and it is refused by name rather than dropped in silence.
func compileRefs(req compileRequest, defaultSessionID string) ([]compileSourceRef, error) {
	seen := map[string]bool{}
	var out []compileSourceRef
	add := func(sid, mid string) error {
		mid = strings.TrimSpace(mid)
		if mid == "" {
			return errors.New("a compile source needs a non-empty message_id")
		}
		sid = strings.TrimSpace(sid)
		if sid == "" {
			sid = defaultSessionID
		}
		key := sid + "\x00" + mid
		if seen[key] {
			return nil
		}
		seen[key] = true
		out = append(out, compileSourceRef{SessionID: sid, MessageID: mid})
		return nil
	}
	for _, mid := range req.MessageIDs {
		if err := add("", mid); err != nil {
			return nil, err
		}
	}
	for _, s := range req.Sources {
		if err := add(s.SessionID, s.MessageID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
