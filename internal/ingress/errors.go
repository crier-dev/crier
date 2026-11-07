// Package ingress implements the push-ingress surface of specs/CRIER-INGRESS.md
// (CRIER-INGRESS-1): POST /ingress/messages, one call that pushes a message to
// exactly one of three destinations — an existing session, a NEW session created
// from a template, or an agent inbox.
//
// The package is additive and one-directional: it imports internal/session (and
// internal/tags, internal/namespace, internal/permissions), and nothing imports
// it except cmd/server/main.go's wiring. The two write paths it composes with
// stay where they are:
//
//   - a session message is appended through the SAME session.Repository calls
//     the session send handler makes (NextSeq → PostMessage), so there is no
//     second append and no second transcript format;
//   - an inbox message goes through the SAME registry delivery path
//     POST /agents/{id}/inbox uses, capability form included, so the inbox
//     entry id, the lease/ack semantics and the guard are the ones that route
//     already had (§4.2: the inbox path is CALLED, not reimplemented).
//
// What this package owns is the routing between the three destinations, the
// request validation, and the idempotency ledger (§3).
package ingress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// The error codes of CRIER-INGRESS §5, plus the two conditions the spec names
// outside that table (§6's empty message and §2.1's payload bound). Every code
// the handler can answer with is declared here, so the catalog has one address
// and a test can enumerate it.
const (
	// CodeInvalidJSON: the body is not JSON, or carries an unknown field
	// (strict decode), or names one message with two different idempotency keys.
	CodeInvalidJSON = "INGRESS_INVALID_JSON"
	// CodeModeRequired: `mode` is missing.
	CodeModeRequired = "INGRESS_MODE_REQUIRED"
	// CodeModeUnknown: `mode` is not session|new_session|inbox. Never defaulted.
	CodeModeUnknown = "INGRESS_MODE_UNKNOWN"
	// CodeModeDisabled: `mode` is excluded by the deployment's allowed set.
	CodeModeDisabled = "INGRESS_MODE_DISABLED"
	// CodeSessionRequired: mode=session with no target.session_id.
	CodeSessionRequired = "INGRESS_SESSION_REQUIRED"
	// CodeTemplateRequired: mode=new_session with no target.template.
	CodeTemplateRequired = "INGRESS_TEMPLATE_REQUIRED"
	// CodeTemplateInvalid: empty title, bad kind, or an undeclared namespace.
	CodeTemplateInvalid = "INGRESS_TEMPLATE_INVALID"
	// CodeAgentRequired: mode=inbox with no target.agent_id (and no capability).
	CodeAgentRequired = "INGRESS_AGENT_REQUIRED"
	// CodeAgentUnknown: mode=inbox and the agent (or a named capability's pool)
	// is not registered.
	CodeAgentUnknown = "INGRESS_AGENT_UNKNOWN"
	// CodeAuthorRequired: `author` names nobody.
	CodeAuthorRequired = "INGRESS_AUTHOR_REQUIRED"
	// CodeAuthorNotMember: mode=session and the author is neither a participant
	// nor permitted to post (CHAT-PERMISSIONS).
	CodeAuthorNotMember = "INGRESS_AUTHOR_NOT_MEMBER"
	// CodeTaskKindRefused: kind=task; task creation stays on its own route.
	CodeTaskKindRefused = "INGRESS_TASK_KIND_REFUSED"
	// CodeReplyTooDeep: reply_to_thread does not exist in that session, names a
	// fresh session, or would move the message out of its thread.
	CodeReplyTooDeep = "INGRESS_REPLY_TOO_DEEP"
	// CodeSessionNotFound: mode=session and the session is absent (or belongs to
	// another realm, which is a 404 for the same reason the session routes answer
	// 404: confirming it exists would cross the realm wall).
	CodeSessionNotFound = "INGRESS_SESSION_NOT_FOUND"
	// CodeSessionClosed: the session is closed and admits no new message
	// (CHAT-SESSIONS §1.3). The spec's table has no code for it; this one is
	// additive and named rather than folded into INGRESS_STORE_ERROR, because a
	// closed room is a caller error, not a backend failure.
	CodeSessionClosed = "INGRESS_SESSION_CLOSED"
	// CodeKeyReused: the same idempotency key with a different request hash.
	CodeKeyReused = "INGRESS_KEY_REUSED"
	// CodeBodyTooLarge: the body exceeds the configured cap.
	CodeBodyTooLarge = "INGRESS_BODY_TOO_LARGE"
	// CodePayloadTooLarge: `payload` exceeds the 256 KiB bound of §2.1. The
	// spec bounds the field without naming a code; this one says what happened
	// instead of folding a caller's oversized object into INGRESS_INVALID_JSON.
	CodePayloadTooLarge = "INGRESS_PAYLOAD_TOO_LARGE"
	// CodeTagsInvalid: see CRIER-TAGS §5 (grammar, count, length, sys:).
	CodeTagsInvalid = "INGRESS_TAGS_INVALID"
	// CodeEmptyMessage: neither text nor payload carried anything (§6).
	CodeEmptyMessage = "INGRESS_EMPTY_MESSAGE"
	// CodeStoreError: a backend append/delivery failed; nothing is claimed
	// delivered.
	CodeStoreError = "INGRESS_STORE_ERROR"
	// CodeNamespaceUnknown: the request acts in a realm no policy declares
	// (the session routes answer this case with 400 UNKNOWN_NAMESPACE; ingress
	// names it in its own vocabulary). A template namespace that does not exist
	// is INGRESS_TEMPLATE_INVALID instead, because §5 spells that case out.
	CodeNamespaceUnknown = "INGRESS_NAMESPACE_UNKNOWN"
	// CodeNamespaceUnauthorized: the realm's token was absent or wrong — the
	// namespace wall, refused before anything is read from or written to it.
	CodeNamespaceUnauthorized = "INGRESS_NAMESPACE_UNAUTHORIZED"
	// CodeHarnessNotAllowed: target.harness was named on a mode that cannot
	// bind one. CRIER-HARNESS §4.3 gives the binding to a NEW session; on
	// mode=session/mode=inbox the field would be dropped, so it is refused by
	// name instead of ignored (the same rule reply_to_thread follows).
	CodeHarnessNotAllowed = "INGRESS_HARNESS_NOT_ALLOWED"
	// CodeMethodNotAllowed: the path is POST-only; a GET reaching the handler
	// (a mux that did not enforce Methods) is refused rather than served.
	CodeMethodNotAllowed = "INGRESS_METHOD_NOT_ALLOWED"
)

// Error is one ingress refusal: the machine code, the HTTP status it answers
// with, and the human detail. It is a value (never a formatted error) so the
// package can declare the catalog as constants and construct a refusal from it
// without risk of mutating a shared sentinel.
type Error struct {
	Code   string
	Status int
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// With returns a copy of the catalog entry carrying this occurrence's detail.
func (e *Error) With(format string, args ...any) *Error {
	out := *e
	if len(args) == 0 {
		out.Detail = format
	} else {
		out.Detail = fmt.Sprintf(format, args...)
	}
	return &out
}

// errorBody is the wire shape §5 names: {"error":"CODE","message":"..."}.
//
// `detail` is written as well: it is the key the session surface's own envelope
// uses (internal/session's writeAPIError), and one push that is refused should
// not require a client to parse two different spellings of the same sentence.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// WriteError writes one refusal with its status and the project's JSON error
// envelope. An *Error carries both; anything else is a backend failure and is
// answered 500 INGRESS_STORE_ERROR, because the handler never claims delivery
// for a failure it cannot classify.
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = ErrStoreError.With("%v", err)
	}
	writeJSON(w, e.Status, errorBody{Error: e.Code, Message: e.Detail, Detail: e.Detail})
}

// The catalog. Each value is a template: With() copies it before adding detail,
// so these are safe to share across goroutines and requests.
var (
	ErrInvalidJSON           = &Error{Code: CodeInvalidJSON, Status: http.StatusBadRequest}
	ErrModeRequired          = &Error{Code: CodeModeRequired, Status: http.StatusBadRequest}
	ErrModeUnknown           = &Error{Code: CodeModeUnknown, Status: http.StatusBadRequest}
	ErrModeDisabled          = &Error{Code: CodeModeDisabled, Status: http.StatusForbidden}
	ErrSessionRequired       = &Error{Code: CodeSessionRequired, Status: http.StatusBadRequest}
	ErrTemplateRequired      = &Error{Code: CodeTemplateRequired, Status: http.StatusBadRequest}
	ErrTemplateInvalid       = &Error{Code: CodeTemplateInvalid, Status: http.StatusBadRequest}
	ErrAgentRequired         = &Error{Code: CodeAgentRequired, Status: http.StatusBadRequest}
	ErrAgentUnknown          = &Error{Code: CodeAgentUnknown, Status: http.StatusNotFound}
	ErrAuthorRequired        = &Error{Code: CodeAuthorRequired, Status: http.StatusBadRequest}
	ErrAuthorNotMember       = &Error{Code: CodeAuthorNotMember, Status: http.StatusForbidden}
	ErrTaskKindRefused       = &Error{Code: CodeTaskKindRefused, Status: http.StatusBadRequest}
	ErrReplyTooDeep          = &Error{Code: CodeReplyTooDeep, Status: http.StatusConflict}
	ErrSessionNotFound       = &Error{Code: CodeSessionNotFound, Status: http.StatusNotFound}
	ErrSessionClosed         = &Error{Code: CodeSessionClosed, Status: http.StatusConflict}
	ErrKeyReused             = &Error{Code: CodeKeyReused, Status: http.StatusConflict}
	ErrBodyTooLarge          = &Error{Code: CodeBodyTooLarge, Status: http.StatusRequestEntityTooLarge}
	ErrPayloadTooLarge       = &Error{Code: CodePayloadTooLarge, Status: http.StatusRequestEntityTooLarge}
	ErrTagsInvalid           = &Error{Code: CodeTagsInvalid, Status: http.StatusBadRequest}
	ErrEmptyMessage          = &Error{Code: CodeEmptyMessage, Status: http.StatusBadRequest}
	ErrStoreError            = &Error{Code: CodeStoreError, Status: http.StatusInternalServerError}
	ErrNamespaceUnknown      = &Error{Code: CodeNamespaceUnknown, Status: http.StatusBadRequest}
	ErrNamespaceUnauthorized = &Error{Code: CodeNamespaceUnauthorized, Status: http.StatusUnauthorized}
	ErrHarnessNotAllowed     = &Error{Code: CodeHarnessNotAllowed, Status: http.StatusBadRequest}
	ErrMethodNotAllowed      = &Error{Code: CodeMethodNotAllowed, Status: http.StatusMethodNotAllowed}
)

// Catalog lists every declared refusal, in the order §5 presents them followed
// by the additive ones. It exists so a test can assert that each code is
// reachable and that no code is declared without a status.
var Catalog = []*Error{
	ErrInvalidJSON, ErrModeRequired, ErrModeUnknown, ErrModeDisabled,
	ErrSessionRequired, ErrTemplateRequired, ErrTemplateInvalid,
	ErrAgentRequired, ErrAgentUnknown, ErrAuthorRequired, ErrAuthorNotMember,
	ErrTaskKindRefused, ErrReplyTooDeep, ErrSessionNotFound, ErrKeyReused,
	ErrBodyTooLarge, ErrTagsInvalid, ErrStoreError,
	// Additive, each with the spec clause that requires it.
	ErrEmptyMessage, ErrPayloadTooLarge, ErrSessionClosed, ErrNamespaceUnknown,
	ErrNamespaceUnauthorized, ErrHarnessNotAllowed, ErrMethodNotAllowed,
}

// writeJSON writes one JSON body with its status. It mirrors the session
// surface's writer (Content-Type application/json, one trailing newline) so the
// two surfaces' bodies are read the same way by a client.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
