package ingress

// request.go — the POST /ingress/messages body (CRIER-INGRESS §2.1).
//
// One struct selects the destination with `mode`; the three targets are
// mutually exclusive spellings in the same `target` object so a caller cannot
// half-name two of them. Decoding is STRICT: an unknown field anywhere is
// INGRESS_INVALID_JSON, because a misspelled `sessionid` must not silently
// become "no session named" and route a message somewhere else.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/crier-dev/crier/internal/session"
	"github.com/crier-dev/crier/internal/strictjson"
	"github.com/crier-dev/crier/internal/tags"
)

// DefaultMaxBodyBytes is the §4 default cap on a request body (1 MiB).
const DefaultMaxBodyBytes = 1 << 20

// MaxPayloadBytes is the §2.1 bound on the opaque `payload` object (256 KiB).
const MaxPayloadBytes = 256 << 10

// IdempotencyHeader is the optional request header that carries the caller's
// key (§2.1). `body.idempotency_key` is accepted as well; the header wins, and
// two DIFFERENT keys on one request are refused rather than guessed between.
const IdempotencyHeader = "Idempotency-Key"

// rawRequest is the decoded body. Its field set IS the accepted vocabulary:
// strictjson names any other key in the refusal.
type rawRequest struct {
	Mode           string             `json:"mode"`
	Target         rawTarget          `json:"target"`
	Author         *session.AuthorRef `json:"author"`
	Kind           string             `json:"kind"`
	Text           string             `json:"text"`
	Tags           []string           `json:"tags"`
	Payload        json.RawMessage    `json:"payload"`
	ReplyToThread  string             `json:"reply_to_thread"`
	IdempotencyKey string             `json:"idempotency_key"`
}

// rawTarget is the destination selector: exactly one spelling per mode.
type rawTarget struct {
	// mode=session
	SessionID string `json:"session_id"`
	// mode=new_session
	Template *rawTemplate `json:"template"`
	// mode=inbox
	AgentID    string `json:"agent_id"`
	Capability string `json:"capability"`
	// Harness names the harness adapter a NEW session is bound to
	// (CRIER-HARNESS §4.3). It is accepted here — beside `template`, the way
	// §4.3 spells `target.harness` — and inside the template as well, because
	// both readings of "the new session's harness" mean the same thing and one
	// of them must not be a silent no-op. Absent means store-only, the default.
	Harness string `json:"harness"`
}

// rawTemplate is the minimum a new session needs (§2.2). Memory is NOT a step:
// a member joins with whatever context the runtime grants it.
type rawTemplate struct {
	Title     string      `json:"title"`
	Kind      string      `json:"kind"`
	Namespace string      `json:"namespace"`
	Members   []rawMember `json:"members"`
	// Harness is the template-local spelling of target.harness (CRIER-HARNESS
	// §4.3). Naming it in both places is accepted only when the two agree.
	Harness string `json:"harness"`
}

// rawMember is one template member. `role` is optional and defaults to member —
// the same default a create-then-join pair produces.
type rawMember struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
}

// Request is the parsed and validated form of the body (§2.2).
type Request struct {
	Mode       Mode
	SessionID  string           // mode=session
	Template   *SessionTemplate // mode=new_session
	AgentID    string           // mode=inbox
	Capability string           // mode=inbox, optional
	Author     session.AuthorRef
	Kind       session.MessageKind
	Text       string
	Tags       []string
	Payload    json.RawMessage
	ReplyTo    string
	Key        string // idempotency key, "" allowed
	// Harness is the adapter a new session is bound to (CRIER-HARNESS §4.3):
	// target.harness, or the template-local spelling, or "" for store-only.
	Harness string
}

// SessionTemplate is the minimum a new session needs (§2.2).
type SessionTemplate struct {
	Title     string
	Kind      string // "channel" | "direct"
	Namespace string
	Members   []session.Member
}

// readBody reads the request body under the configured cap and reports
// INGRESS_BODY_TOO_LARGE rather than buffering an unbounded request. A stated
// Content-Length above the cap is refused before a byte is read; the bounded
// read covers a chunked body that does not advertise its length.
func readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	if r.ContentLength > int64(limit) {
		return nil, ErrBodyTooLarge.With("the request body states %d bytes; the cap is %d", r.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return nil, ErrInvalidJSON.With("could not read the request body: %v", err)
	}
	if len(body) > limit {
		return nil, ErrBodyTooLarge.With("the request body exceeds the cap of %d bytes", limit)
	}
	return body, nil
}

// decodeRequest decodes the body into the raw shape with a STRICT decoder, so
// an unknown field is refused instead of dropped (§5 INGRESS_INVALID_JSON).
func decodeRequest(body []byte) (rawRequest, error) {
	var raw rawRequest
	if len(bytes.TrimSpace(body)) == 0 {
		return raw, ErrInvalidJSON.With("a JSON body is required")
	}
	if err := strictjson.Decode(bytes.NewReader(body), &raw); err != nil {
		return raw, ErrInvalidJSON.With("%v", err)
	}
	return raw, nil
}

// parseRequest validates the decoded body into the §2.2 Request. It performs
// every check that does not need the store, in the order §5's catalog presents
// them, so a refused request has written nothing and the FIRST problem is the
// one reported.
func (h *Handler) parseRequest(r *http.Request, raw rawRequest) (*Request, error) {
	req := &Request{}

	mode := Mode(strings.TrimSpace(raw.Mode))
	if mode == "" {
		return nil, ErrModeRequired.With("mode is required: exactly one of %q, %q or %q (it is never defaulted)", ModeSession, ModeNewSession, ModeInbox)
	}
	switch mode {
	case ModeSession, ModeNewSession, ModeInbox:
	default:
		return nil, ErrModeUnknown.With("mode %q is not one of %q, %q or %q (an unknown mode is refused, never defaulted)", raw.Mode, ModeSession, ModeNewSession, ModeInbox)
	}
	req.Mode = mode
	if !h.modeAllowed(mode) {
		return nil, ErrModeDisabled.With("mode %q is excluded by this deployment's ingress.allowed_modes", mode)
	}

	switch mode {
	case ModeSession:
		req.SessionID = strings.TrimSpace(raw.Target.SessionID)
		if req.SessionID == "" {
			return nil, ErrSessionRequired.With("mode=session requires target.session_id")
		}
		req.ReplyTo = strings.TrimSpace(raw.ReplyToThread)
	case ModeNewSession:
		if raw.Target.Template == nil {
			return nil, ErrTemplateRequired.With("mode=new_session requires target.template")
		}
		tmpl, err := parseTemplate(*raw.Target.Template)
		if err != nil {
			return nil, err
		}
		req.Template = tmpl
		// CRIER-HARNESS §4.3: a new session may name the harness adapter it is
		// bound to. The binding is recorded on the Crier session, so the FIRST
		// successful delivery has somewhere to keep the harness-side id.
		harness, err := resolveHarness(raw.Target.Harness, raw.Target.Template.Harness)
		if err != nil {
			return nil, err
		}
		req.Harness = harness
		// §6: a fresh session has no thread to reply into, so a reply target on
		// this mode is refused rather than silently ignored.
		if strings.TrimSpace(raw.ReplyToThread) != "" {
			return nil, ErrReplyTooDeep.With("reply_to_thread is only valid with mode=session: a new session has no thread to reply into")
		}
	case ModeInbox:
		req.AgentID = strings.TrimSpace(raw.Target.AgentID)
		req.Capability = strings.TrimSpace(raw.Target.Capability)
		// The capability form addresses a POOL instead of an agent, so it
		// satisfies the target requirement on its own (exactly as
		// POST /capabilities/{capability}/inbox needs no agent id).
		if req.AgentID == "" && req.Capability == "" {
			return nil, ErrAgentRequired.With("mode=inbox requires target.agent_id (or target.capability for the capability form)")
		}
		if strings.TrimSpace(raw.ReplyToThread) != "" {
			return nil, ErrReplyTooDeep.With("reply_to_thread is only valid with mode=session: an inbox entry is not a session thread")
		}
	}

	// CRIER-HARNESS §4.3 gives the harness binding to a NEW session only: an
	// existing session either already carries one (and the dispatch hook reads
	// it) or would need a rebind record this surface does not own. Accepting
	// the field here and dropping it would be the silent no-op the strict
	// decoder exists to prevent, so it is a named refusal on the other two
	// modes.
	if mode != ModeNewSession {
		if named := strings.TrimSpace(raw.Target.Harness); named != "" {
			return nil, ErrHarnessNotAllowed.With("target.harness %q is only valid with mode=%s: an existing session's harness binding is not rebindable through ingress", named, ModeNewSession)
		}
	}

	if raw.Author == nil || raw.Author.IsZero() {
		return nil, ErrAuthorRequired.With("author (agent, or principal with optional as_agent) is required: a message records its author")
	}
	req.Author = *raw.Author

	kind := strings.TrimSpace(raw.Kind)
	switch session.MessageKind(kind) {
	case "":
		req.Kind = session.MessagePlain
	case session.MessagePlain, session.MessageAddressed:
		req.Kind = session.MessageKind(kind)
	case session.MessageTask:
		return nil, ErrTaskKindRefused.With("kind=task is refused here: create a task through POST /sessions/{id}/tasks (CHAT-SESSIONS §4.4)")
	default:
		return nil, ErrInvalidJSON.With("kind %q is not one of %q or %q", raw.Kind, session.MessagePlain, session.MessageAddressed)
	}

	req.Text = raw.Text
	payload, err := buildPayload(raw.Text, raw.Payload)
	if err != nil {
		return nil, err
	}
	req.Payload = payload

	// CRIER-TAGS §2.1: the grammar gate runs BEFORE anything is stored, so a
	// refused tag writes nothing at all.
	normalized, err := tags.Normalize(raw.Tags)
	if err != nil {
		return nil, ErrTagsInvalid.With("%v", err)
	}
	if tag, reserved := tags.ReservedViolation(normalized); reserved {
		return nil, ErrTagsInvalid.With("tag %q uses the reserved %q prefix, which only an operator principal may write", tag, tags.ReservedPrefix)
	}
	req.Tags = normalized

	// §2.1: the header and the body may each carry the key; a request carrying
	// two DIFFERENT keys is ambiguous and is refused rather than guessed.
	header := ""
	if r != nil {
		header = strings.TrimSpace(r.Header.Get(IdempotencyHeader))
	}
	bodyKey := strings.TrimSpace(raw.IdempotencyKey)
	switch {
	case header != "" && bodyKey != "" && header != bodyKey:
		return nil, ErrInvalidJSON.With("two different idempotency keys were supplied: header %q and body %q", header, bodyKey)
	case header != "":
		req.Key = header
	default:
		req.Key = bodyKey
	}

	return req, nil
}

// resolveHarness reconciles the two accepted spellings of a new session's
// harness binding (CRIER-HARNESS §4.3): `target.harness` — the form the spec
// names — and the same field inside the template. They mean one thing, so
// naming both is accepted when they agree and refused when they disagree: a
// request that says "pi" in one place and "opencode" in the other has not named
// the session's harness at all, and picking one would execute a choice the
// caller never made.
//
// The name is NOT resolved against a registry here. An adapter id is what the
// session records; whether any adapter serves it is the delivery's answer
// (HARNESS_UNKNOWN, reported inside the 201 rather than as a rollback of a
// session that was created correctly).
func resolveHarness(targetField, templateField string) (string, error) {
	target := strings.TrimSpace(targetField)
	tmpl := strings.TrimSpace(templateField)
	switch {
	case target == "":
		return tmpl, nil
	case tmpl == "" || tmpl == target:
		return target, nil
	default:
		return "", ErrTemplateInvalid.With("target.harness %q and target.template.harness %q disagree: name the harness once", target, tmpl)
	}
}

// parseTemplate validates the new-session template (§5 INGRESS_TEMPLATE_*).
// The namespace is checked against the declared policies LATER (in the
// handler, where the realm registry lives); this function checks the shape.
func parseTemplate(raw rawTemplate) (*SessionTemplate, error) {
	title := strings.TrimSpace(raw.Title)
	if title == "" {
		return nil, ErrTemplateInvalid.With("target.template.title is required: a session without a title is not addressable")
	}
	kind := strings.TrimSpace(raw.Kind)
	switch kind {
	case "":
		kind = string(session.KindChannel)
	case string(session.KindChannel), string(session.KindDirect):
	default:
		return nil, ErrTemplateInvalid.With("target.template.kind %q is not %q or %q", raw.Kind, session.KindChannel, session.KindDirect)
	}
	members := make([]session.Member, 0, len(raw.Members))
	for i, m := range raw.Members {
		mt := strings.TrimSpace(m.MemberType)
		id := strings.TrimSpace(m.MemberID)
		switch session.MemberType(mt) {
		case session.MemberAgent, session.MemberPrincipal:
		default:
			return nil, ErrTemplateInvalid.With("target.template.members[%d].member_type %q is not %q or %q", i, m.MemberType, session.MemberAgent, session.MemberPrincipal)
		}
		if id == "" {
			return nil, ErrTemplateInvalid.With("target.template.members[%d].member_id is required", i)
		}
		role := strings.TrimSpace(m.Role)
		switch session.MemberRole(role) {
		case "":
			role = string(session.RoleMember)
		case session.RoleOwner, session.RoleMember, session.RoleObserver:
		default:
			return nil, ErrTemplateInvalid.With("target.template.members[%d].role %q is not one of %q, %q or %q", i, m.Role, session.RoleOwner, session.RoleMember, session.RoleObserver)
		}
		members = append(members, session.Member{
			MemberType: session.MemberType(mt),
			MemberID:   id,
			Role:       session.MemberRole(role),
		})
	}
	return &SessionTemplate{
		Title:     title,
		Kind:      kind,
		Namespace: strings.TrimSpace(raw.Namespace),
		Members:   members,
	}, nil
}

// buildPayload renders the message payload from `text` + `payload`.
//
// A session/inbox message carries its content in the opaque `payload` object
// (the shipped convention: `payload.text` is what a client renders), so `text`
// is folded into it rather than kept beside it:
//
//   - neither empty: the payload must be a JSON object; `text` is added under
//     the `text` key unless the payload already carries one (an explicit
//     payload wins, it is never overwritten);
//   - text only: {"text": "..."};
//   - payload only: the payload verbatim — a structured message with no prose;
//   - both empty: INGRESS_EMPTY_MESSAGE (§6).
func buildPayload(text string, payload json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if len(trimmed) > MaxPayloadBytes {
			return nil, ErrPayloadTooLarge.With("payload is %d bytes; the bound is %d (§2.1)", len(trimmed), MaxPayloadBytes)
		}
		if trimmed[0] != '{' {
			return nil, ErrInvalidJSON.With("payload must be a JSON object, got %q", previewBytes(trimmed))
		}
		if strings.TrimSpace(text) == "" {
			return json.RawMessage(trimmed), nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, ErrInvalidJSON.With("payload could not be decoded as an object: %v", err)
		}
		if _, ok := obj["text"]; ok {
			return json.RawMessage(trimmed), nil
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, ErrInvalidJSON.With("text could not be encoded: %v", err)
		}
		obj["text"] = encoded
		merged, err := json.Marshal(obj)
		if err != nil {
			return nil, ErrInvalidJSON.With("payload could not be encoded: %v", err)
		}
		return merged, nil
	}
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyMessage.With("a message needs text, a payload, or both (§6)")
	}
	encoded, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return nil, ErrInvalidJSON.With("text could not be encoded: %v", err)
	}
	return encoded, nil
}

// previewBytes renders a short, quoted preview of an unexpected value for a
// refusal detail. It never fails and never echoes more than 40 bytes.
func previewBytes(b []byte) string {
	const limit = 40
	s := string(b)
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
