// rpc.go — the JSON-RPC 2.0 envelope layer of crier's A2A binding
// (INT-A2A-003, specs/A2A-OPTION.md §5.4).
//
// Everything in this file is pure: the request envelope, the response
// envelope, the two error-detail shapes the specification's §9.5 example
// defines, and crier's mapping from the delivery engine's HTTP answer onto a
// JSON-RPC error. Nothing here talks HTTP, and nothing here can be reached
// unless CR_A2A_ENABLED is set AND the target agent opted in — A2A is an EXTRA
// in crier, never a first-class path.
package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// JSONRPCVersion is the only protocol version this binding speaks: the JSON-RPC
// 2.0 binding of §9 fixes it, and A2A's own version is negotiated separately
// through the A2A-Version header (§9.2, §3.6).
const JSONRPCVersion = "2.0"

// RPCMediaType is what a JSON-RPC response carries. The A2A media type
// registration (§14.1.1) is used for the binding's bodies — the same type the
// Agent Card discovery route serves — while the request accepts either the
// registration or the plain application/json §9.1 names.
const RPCMediaType = CardMediaType

// StreamMediaType is the streaming response's content type (§9.1, §9.4.2).
const StreamMediaType = "text/event-stream"

// Method names, verbatim from the specification (§9.4, PascalCase per §9.1).
// The row-suffix forms of the REST column in §5.3 (/message:send,
// /message:stream) belong to the HTTP+JSON/REST binding, which crier declares
// MAY-and-not-built (§2) — this binding is the JSON-RPC one, served at
// JSONRPCBindingPath.
const (
	// MethodSendMessage is §9.4.1: send a message, answer a Task or a Message.
	MethodSendMessage = "SendMessage"
	// MethodSendStreamingMessage is §9.4.2: send a message and stream the
	// updates as Server-Sent Events.
	MethodSendStreamingMessage = "SendStreamingMessage"
)

// Standard JSON-RPC 2.0 error codes (§9.5).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// A2A-specific error codes (§5.4). Only the ones this binding can produce are
// named here; the rest of the range is left to the rows that own those
// operations (INT-A2A-004/005).
const (
	// CodeTaskNotFoundError is -32001: the task id does not exist or is not
	// accessible (§5.4). A delivery answered 404 by the delivery engine — the
	// target agent is not on this relay after all — is reported with it,
	// because from an A2A client's point of view the thing it addressed does
	// not exist here.
	CodeTaskNotFoundError = -32001
	// CodePushNotificationNotSupportedError is -32003: the push-notification
	// configuration surface is not implemented in this build (INT-A2A-005).
	CodePushNotificationNotSupportedError = -32003
	// CodeUnsupportedOperationError is -32004: the operation cannot accept
	// this request (§3.3.4 capability validation, and a message aimed at an
	// existing task's continuation until INT-A2A-004 lands the lifecycle).
	CodeUnsupportedOperationError = -32004
	// CodeVersionNotSupportedError is -32009: A2A-Version asked for a protocol
	// version this server does not serve (§3.6, §9.2).
	CodeVersionNotSupportedError = -32009

	// CodeDeliveryRefused is crier's IMPLEMENTATION-DEFINED server error.
	// JSON-RPC 2.0 §5 reserves -32000..-32099 for implementation-defined
	// server errors, and A2A's own -32001..-32009 are all spoken for by names
	// with a different meaning (§5.4) — so a refusal that A2A has no name for,
	// such as the target agent's message guard blocking the delivery or a
	// quarantined agent, is reported here with crier's own machine-readable
	// reason in the error's data, never as an A2A error it is not.
	CodeDeliveryRefused = -32050
)

// Detail-object type URIs (§9.5: every object in `error.data` MUST carry an
// `@type` key identifying its type).
const (
	// ErrorInfoType is google.rpc.ErrorInfo, the shape §9.5 recommends for
	// refining an error's reporting.
	ErrorInfoType = "type.googleapis.com/google.rpc.ErrorInfo"
	// BadRequestType is google.rpc.BadRequest, the shape §9.5's own
	// invalid-params example uses: one fieldViolation per offending field.
	BadRequestType = "type.googleapis.com/google.rpc.BadRequest"
	// DeliveryRefusalType is crier's own detail object: the delivery engine's
	// verbatim answer to a delivery it refused.
	DeliveryRefusalType = "type.googleapis.com/crier.DeliveryRefusal"
	// ErrorDomain is the domain crier's ErrorInfo details report.
	ErrorDomain = "crier"
)

// RPCRequest is a JSON-RPC 2.0 request envelope (§9.3).
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCResponse is a JSON-RPC 2.0 response envelope. Exactly one of Result and
// Error is present (§9.4, §9.5).
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object (§9.5): a numeric code, a
// human-readable message, and an optional array of detail objects each
// carrying "@type".
//
// It implements error as well, because every refusal this binding produces
// travels as one of these — a translation failure that is a *RPCError is
// reported verbatim, and one that is an *InvalidParamsError is wrapped into
// -32602 ("InvalidParams") at the boundary, so there is exactly one place that
// decides how a refusal is shaped.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    []any  `json:"data,omitempty"`
}

// Error implements error: the code and message, in the shape a log line wants.
func (e *RPCError) Error() string {
	if e == nil {
		return "a2a: <nil rpc error>"
	}
	return fmt.Sprintf("a2a: %d %s", e.Code, e.Message)
}

// ErrorInfo is the google.rpc.ErrorInfo-shaped detail object (§9.5).
type ErrorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason,omitempty"`
	Domain   string            `json:"domain,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// BadRequest and FieldViolation are the google.rpc.BadRequest-shaped detail
// object: the spec's §9.5 invalid-params example is exactly this, one
// fieldViolation per offending field.
type BadRequest struct {
	Type            string           `json:"@type"`
	FieldViolations []FieldViolation `json:"fieldViolations"`
}

// FieldViolation names one offending field and why it was refused.
type FieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// DeliveryRefusal is crier's own detail object: the status and body the
// delivery engine answered, carried verbatim so a client can see exactly what
// crier's own deliver route said rather than a re-telling of it.
type DeliveryRefusal struct {
	Type   string          `json:"@type"`
	Status int             `json:"crierStatus"`
	Body   json.RawMessage `json:"crierBody,omitempty"`
}

// InvalidParamsError is a translation failure the binding reports as the
// JSON-RPC -32602 InvalidParams error, naming the offending field so the
// refusal carries the same field-level detail the spec's example shows.
type InvalidParamsError struct {
	// Field is the parameter path as an A2A client would write it, e.g.
	// "message.parts[1].raw".
	Field string
	// Detail is why it was refused, in one sentence.
	Detail string
}

// Error implements error.
func (e *InvalidParamsError) Error() string {
	if e.Field == "" {
		return e.Detail
	}
	return e.Field + ": " + e.Detail
}

// invalidParams builds an InvalidParamsError.
func invalidParams(field, format string, args ...any) *InvalidParamsError {
	return &InvalidParamsError{Field: field, Detail: fmt.Sprintf(format, args...)}
}

// InvalidParamsDetail renders the error as the -32602 error object's data:
// a google.rpc.BadRequest with one fieldViolation, which is the spec's own
// example for an invalid-params refusal (§9.5).
func InvalidParamsDetail(e *InvalidParamsError) any {
	field := e.Field
	if field == "" {
		field = "params"
	}
	return BadRequest{
		Type:            BadRequestType,
		FieldViolations: []FieldViolation{{Field: field, Description: e.Detail}},
	}
}

// NewRPCError builds a JSON-RPC error object with an optional handler-supplied
// message. Every code this binding produces names itself, so the message always
// says more than the code's own standard text.
func NewRPCError(code int, message string, data ...any) *RPCError {
	return &RPCError{Code: code, Message: message, Data: data}
}

// ErrorResponse builds a JSON-RPC error response for id.
func ErrorResponse(id json.RawMessage, err *RPCError) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: JSONRPCVersion, ID: id, Error: err}
}

// SuccessResponse builds a JSON-RPC success response for id.
func SuccessResponse(id json.RawMessage, result any) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}

// NullID is the id an error response carries when the request's own id could
// not be read (JSON-RPC 2.0 §5: "If there was an error in detecting the id in
// the Request object (e.g. Parse error/Invalid Request), it MUST be Null").
var NullID = json.RawMessage("null")

// ParseErrorCode names the error a body that is not JSON at all gets.
const ParseErrorCode = CodeParseError

// DecodeRequest validates the JSON-RPC 2.0 request envelope and returns it.
//
// The rules are JSON-RPC 2.0 §4 as §9.3 restates them, made loud rather than
// lenient — a request this bus cannot correlate is refused instead of being
// half-executed:
//
//   - the body MUST be a single JSON object. A batch (array) is refused: the
//     A2A operations this binding serves are all request/response, and
//     answering a batch would mean inventing behaviour the spec does not
//     define for them;
//   - "jsonrpc" MUST be exactly "2.0";
//   - "method" MUST be a non-empty string;
//   - "id" MUST be present, and a string or a number. A request WITHOUT an id
//     is refused: every A2A operation answers a correlated response, and a
//     delivery with nothing to correlate it to is worse than a refusal. This
//     is a deliberate, documented narrowing of JSON-RPC's notification case.
func DecodeRequest(body []byte) (*RPCRequest, *RPCError) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, NewRPCError(CodeParseError, "Invalid JSON payload: the request body is empty")
	}
	if trimmed[0] == '[' {
		return nil, NewRPCError(CodeInvalidRequest,
			"batch requests are not supported: every A2A operation in this binding is a single JSON-RPC request")
	}

	var req RPCRequest
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// A body that is not JSON at all is a parse error; a JSON body whose
		// MEMBERS are wrong (a misspelled envelope member, a wrong type) is an
		// invalid request. json's own error text carries the distinction.
		if errors.Is(err, io.EOF) {
			return nil, NewRPCError(CodeParseError, "Invalid JSON payload: no JSON value")
		}
		if isJSONSyntaxError(err) {
			return nil, NewRPCError(CodeParseError, "Invalid JSON payload: "+jsonErrText(err))
		}
		return nil, NewRPCError(CodeInvalidRequest, "not a valid JSON-RPC 2.0 request object: "+jsonErrText(err))
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, NewRPCError(CodeParseError, "Invalid JSON payload: trailing data after the request object")
	}

	if req.JSONRPC != JSONRPCVersion {
		return nil, NewRPCError(CodeInvalidRequest, fmt.Sprintf(
			"jsonrpc must be %q (got %q)", JSONRPCVersion, req.JSONRPC))
	}
	if req.Method == "" {
		return nil, NewRPCError(CodeInvalidRequest, `method is required`)
	}
	if len(bytes.TrimSpace(req.ID)) == 0 || string(bytes.TrimSpace(req.ID)) == "null" {
		return nil, NewRPCError(CodeInvalidRequest, fmt.Sprintf(
			"id is required: a %s request must carry a string or number id so its response can be correlated (notifications are not part of this binding)", req.Method))
	}
	if err := validateID(req.ID); err != nil {
		return nil, NewRPCError(CodeInvalidRequest, err.Error())
	}
	return &req, nil
}

// validateID enforces JSON-RPC 2.0 §4's id type rule: a String, a Number — or
// Null, which this binding treats as absent (see DecodeRequest).
func validateID(id json.RawMessage) error {
	trimmed := bytes.TrimSpace(id)
	switch trimmed[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return nil
	default:
		return fmt.Errorf(`id must be a string or a number, got %s`, trimmed)
	}
}

// isJSONSyntaxError reports whether err is a JSON SYNTAX error (a malformed
// body) rather than a decoding error about a well-formed body's shape. A body
// that stops mid-value ("{"jsonrpc":") is io.ErrUnexpectedEOF, which the json
// package reports for a truncated document rather than as a SyntaxError.
func isJSONSyntaxError(err error) bool {
	var se *json.SyntaxError
	return errors.As(err, &se) || errors.Is(err, io.ErrUnexpectedEOF)
}

// jsonErrText renders a decode error compactly.
func jsonErrText(err error) string {
	return strings.TrimPrefix(err.Error(), "json: ")
}

// strictDecode decodes raw into dst, refusing any member dst's type does not
// declare — naming the offending key AND the keys that are accepted, exactly
// the discipline internal/a2a's `a2a` block decoder applies to a registry row.
//
// It exists so an A2A parameter this binding cannot honour is a loud refusal
// rather than a silently dropped member: a client that misspelled
// `returnImmediately` must not be answered as if it had said nothing.
//
// dst is a pointer to a struct, or to a slice of structs (the `parts` array,
// whose elements are where a client's spelling mistakes actually show up: the
// per-element scan reports which element and which key). The scan is driven by
// the type (the same declaresField probe internal/a2a's config decoder uses), so
// it cannot drift from the struct it guards, and DisallowUnknownFields on the
// decoder is the second line of defence.
func strictDecode(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("no JSON value")
	}
	typ := reflect.TypeOf(dst)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return errors.New("strictDecode: dst must be a pointer")
	}
	elem := typ.Elem()
	switch elem.Kind() {
	case reflect.Struct:
		if key, accepted, found := unknownMemberFor(elem, raw); found {
			return fmt.Errorf("unknown field %q (accepted: %s)", key, strings.Join(accepted, ", "))
		}
	case reflect.Slice:
		if err := strictDecodeElements(raw, elem); err != nil {
			return err
		}
	default:
		return errors.New("strictDecode: dst must point to a struct or a slice of structs")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing data")
	}
	return nil
}

// strictDecodeElements scans a JSON array of structs for members the element
// type does not declare, naming the element index so a refusal points at the
// part that carries the typo rather than at the array.
func strictDecodeElements(raw []byte, slice reflect.Type) error {
	var elements []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&elements); err != nil {
		// Not an array of values this scan can judge (a type error, a nested
		// shape): the decode below reports it with json's own message.
		return nil
	}
	for i, element := range elements {
		if key, accepted, found := unknownMemberFor(slice.Elem(), element); found {
			return fmt.Errorf("element %d: unknown field %q (accepted: %s)", i, key, strings.Join(accepted, ", "))
		}
	}
	return nil
}

// unknownMemberFor finds the first member of the JSON object raw that the
// struct type typ does not declare. It is internal/a2a's own type-driven scan
// (config.go) generalised over the type, so both decoders share one probe.
func unknownMemberFor(typ reflect.Type, raw []byte) (key string, accepted []string, found bool) {
	keys, ok := objectMemberKeys(raw)
	if !ok {
		return "", nil, false
	}
	for _, k := range keys {
		if !declaresField(typ, k) {
			return k, acceptedKeys(typ), true
		}
	}
	return "", nil, false
}
