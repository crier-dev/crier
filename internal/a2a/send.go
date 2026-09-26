// send.go — the SendMessage / SendStreamingMessage translation (INT-A2A-003,
// specs/A2A-OPTION.md §5.4).
//
// This is the whole point of the row: an A2A message is translated into crier's
// EXISTING delivery path and nothing else. There is no second delivery engine
// here — Translate produces the very body POST /agents/{id}/inbox already
// accepts, and everything downstream of that (the guard, idempotency, the
// detection layer, federation hold/retry, webhook push, the durable inbox, TTL
// and the lease/ack lifecycle) is the shipped code, reached unchanged.
//
// It is pure on purpose: the translation, the accept→Task/Message projection and
// the crier-status→JSON-RPC-error table are all functions of their inputs, so
// the wire contract is testable without a server.
package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AgentIDHeader is crier's own convention for identifying the sending agent on
// a relay publish (the relay's rate limiter reads it). An A2A client that
// identifies itself this way sets the delivery's `sender`, which is what
// federation hold/retry needs to be able to report a terminal outcome — A2A has
// no sender field of its own, so without either this header or the `sender`
// request-metadata key a delivery is simply unattributed.
const AgentIDHeader = "X-Agent-ID"

// A2AVersionHeader is the A2A service parameter carrying the client's protocol
// version (§3.2.6, §14.2.1). Service parameters travel as HTTP headers, never
// inside params (§9.2).
const A2AVersionHeader = "A2A-Version"

// A2AExtensionsHeader is the A2A service parameter listing the extension URIs a
// client wants (§3.2.6, §14.2.2). It travels as a header; crier declares no
// extension, so it is read and recorded only as far as this file documents.
const A2AExtensionsHeader = "A2A-Extensions"

// SupportedVersions are the A2A protocol versions this server serves. The Agent
// Card's supportedInterfaces carries ProtocolVersion, and a client asking for a
// different one is refused with VersionNotSupportedError (§3.6, §6.4).
var SupportedVersions = []string{ProtocolVersion}

// CheckVersion validates the A2A-Version service parameter. An absent (or empty)
// header is accepted as this server's version: the header is opt-in negotiation
// on the client's side and its absence is the common case.
func CheckVersion(header string) *RPCError {
	got := strings.TrimSpace(header)
	if got == "" {
		return nil
	}
	for _, v := range SupportedVersions {
		if got == v {
			return nil
		}
	}
	// A client stating a minor ("1") or a patch ("1.0.0") of a version this
	// server serves is served: the protocol's own compatibility rule is
	// major-scoped, and refusing a patch-level spelling would refuse a client
	// the spec considers compatible.
	major := strings.Split(ProtocolVersion, ".")[0]
	if got == major || strings.HasPrefix(got, major+".") {
		return nil
	}
	return NewRPCError(CodeVersionNotSupportedError, fmt.Sprintf(
		"A2A-Version %s is not supported: this server serves %s", got, strings.Join(SupportedVersions, ", ")),
		ErrorInfo{Type: ErrorInfoType, Reason: "VERSION_NOT_SUPPORTED", Domain: ErrorDomain,
			Metadata: map[string]string{"requested": got, "supported": strings.Join(SupportedVersions, ",")}})
}

// SendMessageConfiguration is §3.2.2 — the optional `configuration` object of a
// SendMessage request.
type SendMessageConfiguration struct {
	// AcceptedOutputModes is read and NOT honoured: crier does not re-encode a
	// payload, and the media type of a part is the one the agent that produced
	// it chose. §3.2.2 makes it a SHOULD on the server, and this binding says
	// so in the spec rather than pretending to tailor output it does not touch.
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	// TaskPushNotificationConfig is the A2A push-notification configuration.
	// An inline one is REFUSED rather than silently ignored (a silent accept
	// would tell the client it has push notifications it will never receive):
	// the push configuration is its own operation surface (INT-A2A-005,
	// CreateTaskPushNotificationConfig and friends, §9.4.7), which is where a
	// channel is configured and where the target agent's capability is
	// validated. A send delivers; it never writes configuration.
	TaskPushNotificationConfig json.RawMessage `json:"taskPushNotificationConfig,omitempty"`
	// HistoryLength bounds how many recent history messages the returned Task
	// carries (§3.2.4): unset = this server's default, 0 = no history.
	HistoryLength *int `json:"historyLength,omitempty"`
	// ReturnImmediately asks the server not to wait (§3.2.2). In this binding
	// `true` maps onto crier's own request-level override — delivery_mode
	// "async" — so the delivery is queued instead of waiting on the target's
	// webhook. See Translate for the full statement of what crier can and
	// cannot honour here.
	ReturnImmediately bool `json:"returnImmediately,omitempty"`
}

// wireSendParams and wireMessage are the strictly-decoded wire shapes of the
// request: the nested objects are kept raw so the refusal can NAME the path it
// came from (`message.parts[1].raw`) instead of reporting json's pathless
// "unknown field" for a member three levels down.
type wireSendParams struct {
	Tenant        string          `json:"tenant"`
	Message       json.RawMessage `json:"message"`
	Configuration json.RawMessage `json:"configuration"`
	Metadata      map[string]any  `json:"metadata"`
}

// wireMessage mirrors Message with the parts array left raw.
type wireMessage struct {
	MessageID        string          `json:"messageId"`
	ContextID        string          `json:"contextId"`
	TaskID           string          `json:"taskId"`
	Role             string          `json:"role"`
	Parts            json.RawMessage `json:"parts"`
	Metadata         map[string]any  `json:"metadata"`
	Extensions       []string        `json:"extensions"`
	ReferenceTaskIDs []string        `json:"referenceTaskIds"`
}

// DecodeSendMessageParams strictly decodes the `params` object of SendMessage
// and SendStreamingMessage (§3.2.1) and refuses everything it cannot carry
// faithfully, naming the field:
//
//   - an unknown member anywhere in the tree is refused with the accepted set
//     (the discipline crier's own `a2a` registration block is held to);
//   - a part whose oneof is not exactly one of text|raw|url|data is refused —
//     including a part with none, which is how a client speaking the older
//     `kind`-discriminated shape would come apart, and the refusal names the
//     accepted members so that is one round trip to fix.
func DecodeSendMessageParams(raw json.RawMessage) (*SendMessageParams, *RPCError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, NewRPCError(CodeInvalidParams, "params is required",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "is required"}))
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '{' {
		return nil, NewRPCError(CodeInvalidParams, "params must be a JSON object",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "must be a JSON object"}))
	}

	var wire wireSendParams
	if err := strictDecode(trimmed, &wire); err != nil {
		return nil, invalidParamsError("params", err)
	}
	if len(bytes.TrimSpace(wire.Message)) == 0 {
		return nil, invalidParamsError("message", errRequired)
	}
	var msgWire wireMessage
	if err := strictDecode(wire.Message, &msgWire); err != nil {
		return nil, invalidParamsError("message", err)
	}
	parts, err := decodeParts(msgWire.Parts, "message.parts")
	if err != nil {
		return nil, refusalOf(err)
	}

	params := &SendMessageParams{
		Tenant: wire.Tenant,
		Message: &Message{
			MessageID:        msgWire.MessageID,
			ContextID:        msgWire.ContextID,
			TaskID:           msgWire.TaskID,
			Role:             msgWire.Role,
			Parts:            parts,
			Metadata:         msgWire.Metadata,
			Extensions:       msgWire.Extensions,
			ReferenceTaskIDs: msgWire.ReferenceTaskIDs,
		},
		Metadata: wire.Metadata,
	}

	if len(bytes.TrimSpace(wire.Configuration)) > 0 {
		trimmedCfg := bytes.TrimSpace(wire.Configuration)
		if string(trimmedCfg) != "null" {
			var cfg SendMessageConfiguration
			if err := strictDecode(trimmedCfg, &cfg); err != nil {
				return nil, invalidParamsError("configuration", err)
			}
			params.Configuration = &cfg
		}
	}
	return params, nil
}

// errRequired is the shared "is required" detail.
var errRequired = errors.New("is required")

// refusalOf renders an error the translation produced as the JSON-RPC error it
// should travel as.
func refusalOf(err error) *RPCError {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	var params *InvalidParamsError
	if errors.As(err, &params) {
		return NewRPCError(CodeInvalidParams, params.Error(), InvalidParamsDetail(params))
	}
	return NewRPCError(CodeInvalidParams, err.Error())
}

// invalidParamsError wraps a strict-decode failure as the -32602 refusal for
// field.
func invalidParamsError(field string, err error) *RPCError {
	return refusalOf(invalidParams(field, "%s", err.Error()))
}

// SendMessageParams is the `params` object of SendMessage and
// SendStreamingMessage (§3.2.1). The tenant names the target: in crier one
// origin hosts many agents, so the routing identifier the Agent Card publishes
// as AgentInterface.tenant is the registry row's id (§5.3).
type SendMessageParams struct {
	Tenant        string                    `json:"tenant,omitempty"`
	Message       *Message                  `json:"message"`
	Configuration *SendMessageConfiguration `json:"configuration,omitempty"`
	// Metadata carries request-level parameters. It is strictly decoded: the
	// keys below change what the server DOES, and crier's own rule for a
	// request-level parameter is that it is honoured or rejected, never
	// silently accepted (DF-CRIER-180).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Request-metadata keys this binding maps onto crier's own deliver fields.
// Every one of them is an existing knob of POST /agents/{id}/inbox; none of them
// is a new behaviour, and none of them may change the meaning of anything crier
// already does.
const (
	metaDeliveryMode = "deliveryMode"
	metaTimeoutMs    = "timeoutMs"
	metaTTLSeconds   = "ttlSeconds"
	metaSender       = "sender"
	metaRequestID    = "requestId"
	metaThreadID     = "threadId"
)

// requestMetadataKeys is the accepted set, in the order a refusal prints them.
var requestMetadataKeys = []string{
	metaDeliveryMode, metaTimeoutMs, metaTTLSeconds, metaSender, metaRequestID, metaThreadID,
}

// DeliveryRequest is the crier deliver body this binding produces. It mirrors
// the wire shape of POST /agents/{id}/inbox exactly (registry.deliverRequest);
// it is declared here because internal/registry imports this package and not the
// other way round.
type DeliveryRequest struct {
	Payload        json.RawMessage `json:"payload"`
	Sender         string          `json:"sender,omitempty"`
	SessionID      string          `json:"session_id,omitempty"`
	ThreadID       string          `json:"thread_id,omitempty"`
	DeliveryMode   string          `json:"delivery_mode,omitempty"`
	TimeoutMs      int             `json:"timeout_ms,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Kind           string          `json:"kind,omitempty"`
	TTLSeconds     *int            `json:"ttl_seconds,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// Translation is the result of translating one A2A request: the delivery body to
// hand to crier's deliver handler, plus the A2A facts the response needs and
// that only the request carries.
type Translation struct {
	// Request is the crier deliver body. Its Payload is the projected
	// Envelope; every other member is either derived from the A2A message or
	// absent.
	Request *DeliveryRequest
	// Envelope is the projected payload, kept so a caller can build the Task's
	// history without a second projection.
	Envelope *Envelope
	// ContextID is the A2A context the interaction belongs to: the message's own
	// contextId when it stated one, else the crier session it becomes, else —
	// because A2A's Task and every one of its status events carries a contextId
	// and a task that belongs to no context is not representable — the task id.
	// §3.4.1 sanctions that last inference ("If only task_id is provided, the
	// server will infer context_id from it").
	ContextID string
	// SentMessage is the A2A message the client sent, for the Task history.
	SentMessage *Message
	// HistoryLength is the configuration's historyLength (§3.2.4).
	HistoryLength *int
	// Streaming reports whether this translation came from
	// SendStreamingMessage, which never selects the blocking transport (see
	// Translate).
	Streaming bool
}

// Translate turns one SendMessage/SendStreamingMessage request into a crier
// delivery. It performs every check that does not depend on the target agent's
// row, and refuses what it cannot honour faithfully:
//
//   - message and its required members (§4.1.4: messageId, role, parts);
//   - the oneof of every part, via the shared mapping;
//   - `taskId`: continuing an existing crier task means addressing an inbox
//     entry's lifecycle, which is INT-A2A-004's surface — and which crier
//     cannot represent as a continuation at all (a task IS its entry). The
//     ROUTE resolves a task id against crier's own store and answers the
//     specification's three cases for it (§5.5.3); this branch is the
//     translation layer's own refusal for a caller that reaches it directly;
//   - `configuration.taskPushNotificationConfig`: refused — an inline push
//     configuration is not accepted on a send; the four §9.4.7 operations
//     (INT-A2A-005, §5.5) are where a push channel is configured, and they are
//     where the target agent's capability is validated.
//
// Session and sender mapping, so no later reader has to infer it:
//
//   - Message.contextId becomes crier's `session_id` — both mean "the
//     conversation this belongs to" (CR-FEAT-004);
//   - Message.messageId becomes crier's `idempotency_key`, which is A2A §3.3.1's
//     own deduplication rule made executable: a retry of the same message id
//     answers with the FIRST delivery's accept — the same task id — instead of
//     delivering the same work twice;
//   - `sender` is the request-metadata key, else the X-Agent-ID header.
//
// `returnImmediately` and streaming mode: crier's delivery is asynchronous for
// the durable inbox (the message is durable on accept; the receiving agent's
// ack is what makes it terminal, possibly much later) and blocking only for a
// target whose webhook asks for it. §3.2.2's blocking default therefore cannot
// mean "hold the send open until the consumer acks" — the value of that
// behaviour would be a 120-second hold on every send. What this binding does
// instead: `returnImmediately: true` selects crier's own async override, and
// for SendStreamingMessage `async` is selected unconditionally, because the
// client asked for updates INSTEAD of one blocking answer. A target whose
// webhook is configured blocking still answers with a direct Message when the
// client did not ask otherwise. This is stated as a deviation in
// specs/A2A-OPTION.md §5.4.3, not left for a client to discover.
func Translate(params *SendMessageParams, requestMetadata map[string]any, senderHeader string, streaming bool) (*Translation, error) {
	if params == nil {
		return nil, invalidParams("params", "is required")
	}
	if params.Message == nil {
		return nil, invalidParams("message", "is required")
	}
	msg := params.Message
	if strings.TrimSpace(msg.MessageID) == "" {
		return nil, invalidParams("message.messageId", "is required")
	}
	switch msg.Role {
	case RoleUser, RoleAgent:
	case RoleUnspecified, "":
		return nil, invalidParams("message.role", "is required and must be %s or %s", RoleUser, RoleAgent)
	default:
		return nil, invalidParams("message.role", "must be %s or %s, got %q", RoleUser, RoleAgent, msg.Role)
	}
	if len(msg.Parts) == 0 {
		return nil, invalidParams("message.parts", "at least one part is required")
	}
	if msg.TaskID != "" {
		// The ROUTE resolves a task id against crier's store first and answers
		// the specification's own three cases for it (INT-A2A-004, §5.5.3): a
		// task id crier holds no record of is TaskNotFoundError, a terminal one
		// is UnsupportedOperationError naming the state, an open one is this
		// refusal. This branch is the translation layer's own guard for a caller
		// that reaches it directly: no task id is ever translated into a
		// delivery, because a delivery is a NEW task.
		return nil, NewRPCError(CodeUnsupportedOperationError,
			"message.taskId: continuing an existing task is not supported by this build — INT-A2A-004 ships the task lifecycle (GetTask / ListTasks / CancelTask / SubscribeToTask) but not task CONTINUATION, which crier cannot represent: a task IS its inbox entry, and no primitive appends a message to an existing entry. Send a message without message.taskId to start a new task",
			ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskContinuationUnsupported, Domain: ErrorDomain,
				Metadata: map[string]string{"taskId": msg.TaskID}})
	}
	if msg.MessageID != "" && len(msg.MessageID) > 128 {
		// The same bound crier's own idempotency_key carries. Checked here so
		// the refusal names the A2A field instead of leaking crier's wire name.
		return nil, invalidParams("message.messageId", "must be at most 128 characters (it is the delivery's deduplication key)")
	}

	env, err := EnvelopeFromMessage(msg)
	if err != nil {
		return nil, err
	}

	deliver := &DeliveryRequest{
		Sender:         senderHeader,
		SessionID:      msg.ContextID,
		IdempotencyKey: msg.MessageID,
	}
	if streaming {
		// See the doc comment: a stream replaces the single blocking answer.
		deliver.DeliveryMode = "async"
	}
	if params.Configuration != nil {
		cfg := params.Configuration
		if len(strings.TrimSpace(string(cfg.TaskPushNotificationConfig))) > 0 {
			// The push-configuration surface exists as its own operations
			// (INT-A2A-005, §9.4.7) and is what validates the target agent's
			// capability — a send does not write configuration. The refusal is
			// kept target-agnostic here because this function cannot see the
			// row; the handler re-codes it to UnsupportedOperationError when
			// the resolved agent DOES have a push channel, so the answer never
			// claims a capability the agent has (§5.5.3).
			return nil, NewRPCError(CodePushNotificationNotSupportedError,
				"configuration.taskPushNotificationConfig: an inline push configuration is not accepted on a send — INT-A2A-005 serves push configuration as its own operations (CreateTaskPushNotificationConfig, GetTaskPushNotificationConfig, ListTaskPushNotificationConfigs, DeleteTaskPushNotificationConfig, §9.4.7), which are what configure and validate an agent's push channel",
				ErrorInfo{Type: ErrorInfoType, Reason: "PUSH_NOTIFICATION_NOT_SUPPORTED", Domain: ErrorDomain})
		}
		if cfg.HistoryLength != nil && *cfg.HistoryLength < 0 {
			return nil, invalidParams("configuration.historyLength", "must not be negative")
		}
		if cfg.ReturnImmediately {
			deliver.DeliveryMode = "async"
		}
	}

	if err := applyRequestMetadata(deliver, requestMetadata); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode the projected payload: %w", err)
	}
	deliver.Payload = payload

	contextID := msg.ContextID
	if contextID == "" {
		contextID = deliver.SessionID
	}
	t := &Translation{
		Request:       deliver,
		Envelope:      env,
		ContextID:     contextID,
		SentMessage:   msg,
		HistoryLength: params.Configuration.historyLengthOrNil(),
		Streaming:     streaming,
	}
	return t, nil
}

// historyLengthOrNil returns the configuration's historyLength, or nil.
func (c *SendMessageConfiguration) historyLengthOrNil() *int {
	if c == nil {
		return nil
	}
	return c.HistoryLength
}

// applyRequestMetadata maps the accepted request-metadata keys onto crier's own
// deliver fields. An unknown key is refused naming the accepted set: a
// request-level parameter this binding cannot honour must not be silently
// dropped, because the client that sent it believes it was applied.
func applyRequestMetadata(deliver *DeliveryRequest, meta map[string]any) error {
	for key := range meta {
		if !contains(requestMetadataKeys, key) {
			return invalidParams("metadata."+key, "unknown request metadata key (accepted: %s)",
				strings.Join(requestMetadataKeys, ", "))
		}
	}
	for _, key := range requestMetadataKeys {
		v, ok := meta[key]
		if !ok {
			continue
		}
		switch key {
		case metaDeliveryMode:
			s, err := metaString(key, v)
			if err != nil {
				return err
			}
			deliver.DeliveryMode = s
		case metaTimeoutMs:
			n, err := metaInt(key, v)
			if err != nil {
				return err
			}
			deliver.TimeoutMs = int(n)
		case metaTTLSeconds:
			n, err := metaInt(key, v)
			if err != nil {
				return err
			}
			ttl := int(n)
			deliver.TTLSeconds = &ttl
		case metaSender:
			s, err := metaString(key, v)
			if err != nil {
				return err
			}
			deliver.Sender = s
		case metaRequestID:
			s, err := metaString(key, v)
			if err != nil {
				return err
			}
			deliver.RequestID = s
		case metaThreadID:
			s, err := metaString(key, v)
			if err != nil {
				return err
			}
			deliver.ThreadID = s
		}
	}
	return nil
}

// metaString reads one string-valued request-metadata member.
func metaString(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", invalidParams("metadata."+key, "must be a string, got %s", jsonTypeName(v))
	}
	return s, nil
}

// metaInt reads one integer-valued request-metadata member. The decoder runs
// with UseNumber, so a fractional or out-of-range value is refused rather than
// truncated to whatever a float64 round-trip produced.
func metaInt(key string, v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return 0, invalidParams("metadata."+key, "must be an integer, got %s", n.String())
		}
		return i, nil
	default:
		return 0, invalidParams("metadata."+key, "must be an integer, got %s", jsonTypeName(v))
	}
}

// jsonTypeName names a decoded JSON value's type for a refusal message.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// DeliverAccept is the subset of crier's deliver accept body this binding reads.
// Every member is one crier already publishes on POST /agents/{id}/inbox — no
// field is added to that response for A2A's benefit.
type DeliverAccept struct {
	ID               string          `json:"id"`
	Transport        string          `json:"transport"`
	DeliveryMode     string          `json:"delivery_mode"`
	ExpiresAt        json.RawMessage `json:"expires_at"`
	Guard            json.RawMessage `json:"guard"`
	IdempotentReplay bool            `json:"idempotent_replay"`
	Reply            json.RawMessage `json:"reply"`
	Status           string          `json:"status"`
	MaxHoldS         int             `json:"max_hold_s"`
}

// ReplyPayload returns the reply body of a blocking delivery, if any.
func (a DeliverAccept) ReplyPayload() []byte {
	if len(a.Reply) == 0 || string(a.Reply) == "null" {
		return nil
	}
	return a.Reply
}

// Expiry returns the resolved message expiry: absent (no inbox entry), the zero
// time for a never-expiring message (JSON null), or the finite instant.
func (a DeliverAccept) Expiry() (at time.Time, applies bool) {
	trimmed := strings.TrimSpace(string(a.ExpiresAt))
	switch trimmed {
	case "":
		return time.Time{}, false
	case "null":
		return time.Time{}, true
	}
	var t time.Time
	if err := json.Unmarshal(a.ExpiresAt, &t); err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Held reports whether the accept is the federation hold answer (202 with
// status "held"): the delivery is queued at the source for bounded retry, so it
// is real but not yet delivered — which the A2A task's status message says.
func (a DeliverAccept) Held() bool { return a.Status == "held" }

// CrierMeta is the crier-specific facts an A2A Task carries in its `metadata`
// under the "crier" key. A2A metadata is "a key/value object to store custom
// metadata about a task" (§4.1.1), and these are the facts crier's accept body
// states that A2A has no field for — kept namespaced so they can never be
// confused with an agent's own metadata.
//
// One shape serves both directions on purpose. A task is written by a
// SendMessage (whose facts come from the delivery accept) and read back by
// GetTask / ListTasks / CancelTask (whose facts come from the stored entry), and
// a client that sees the same task through both must not be handed two
// different key sets for it. The read path therefore fills the same members it
// can prove from the entry and states which record it read the state from in
// StateBasis (INT-A2A-004).
type CrierMeta struct {
	Transport        string          `json:"transport,omitempty"`
	DeliveryMode     string          `json:"delivery_mode,omitempty"`
	ExpiresAt        json.RawMessage `json:"expires_at,omitempty"`
	Held             bool            `json:"held,omitempty"`
	MaxHoldS         int             `json:"max_hold_s,omitempty"`
	IdempotentReplay bool            `json:"idempotent_replay,omitempty"`
	Guard            json.RawMessage `json:"guard,omitempty"`
	// StateBasis names the crier record a READ task's state was resolved from
	// (INT-A2A-004, specs/A2A-OPTION.md §5.5.2). It is absent on a task a
	// delivery wrote — the write path reports what the delivery did, not what a
	// state was read from — and `omitempty` is what keeps that task's metadata
	// byte-identical to what it was before this field existed.
	StateBasis string `json:"state_basis,omitempty"`
	// DeadLetteredAt is when crier's expiry sweep recorded a durable failure
	// for this task's message, RFC 3339. Absent unless that is what the state
	// was resolved from.
	DeadLetteredAt string `json:"dead_lettered_at,omitempty"`
}

// TaskFromAccept builds the A2A Task for a crier accept that created a task:
// every accepted delivery that did NOT answer with a reply body.
//
// State mapping (the row's own mapping table, made executable):
//
//   - an inbox accept (201) is TASK_STATE_SUBMITTED: the message is durable and
//     no one has claimed it;
//   - a queued webhook accept (202, async/batch) is TASK_STATE_SUBMITTED: the
//     delivery is accepted for push and the inbox was deliberately bypassed;
//   - a held federation delivery (202 status=held) is TASK_STATE_SUBMITTED with
//     a status message that says so, because "queued at the source for bounded
//     retry" is a fact an A2A client must not have to guess.
//
// TASK_STATE_WORKING is never asserted here: at this instant nothing can have
// claimed the message yet, and a state this server cannot observe is exactly
// the kind of claim the whole option is forbidden from inventing.
func TaskFromAccept(t *Translation, accept DeliverAccept, now time.Time) (*Task, error) {
	if strings.TrimSpace(accept.ID) == "" {
		return nil, errors.New("crier accept carried no message id")
	}
	contextID := t.ContextID
	if contextID == "" {
		contextID = accept.ID
	}
	task := &Task{
		ID:        accept.ID,
		ContextID: contextID,
		Status: TaskStatus{
			State:     TaskStateSubmitted,
			Timestamp: formatTimestamp(now),
		},
	}
	if len(t.Request.Payload) > 0 {
		task.Metadata = map[string]any{"crier": CrierMeta{
			Transport:        accept.Transport,
			DeliveryMode:     accept.DeliveryMode,
			ExpiresAt:        accept.ExpiresAt,
			Held:             accept.Held(),
			MaxHoldS:         accept.MaxHoldS,
			IdempotentReplay: accept.IdempotentReplay,
			Guard:            accept.Guard,
		}}
	}
	if accept.Held() {
		text := fmt.Sprintf(
			"accepted and held at the source: no configured relay link had this agent, and the delivery is retried for up to %d seconds before its terminal outcome is reported to the sender",
			accept.MaxHoldS)
		msg := &Message{
			MessageID: accept.ID,
			ContextID: contextID,
			TaskID:    accept.ID,
			Role:      RoleAgent,
			Parts:     []Part{TextPart(text)},
		}
		task.Status.Message = msg
	}
	// History is the message that created the task, bounded by §3.2.4's
	// historyLength semantics (0 = none).
	if t.SentMessage != nil && historyAllows(t.HistoryLength) {
		task.History = []Message{*t.SentMessage}
	}
	return task, nil
}

// historyAllows reports whether the returned Task may carry history: unset means
// this server's default (the single creating message), 0 means none, > 0 means
// at most that many — and there is exactly one to give.
func historyAllows(historyLength *int) bool {
	return historyLength == nil || *historyLength > 0
}

// MessageFromReply builds the direct A2A Message a blocking delivery answered
// with (§3.1.1's "direct response message for simple interactions"). The reply
// body is projected through the shared part mapping: a reply that is one of this
// binding's envelopes yields its parts, and anything else becomes a single data
// part carrying the reply verbatim.
func MessageFromReply(t *Translation, accept DeliverAccept, now time.Time) (*Message, error) {
	payload := accept.ReplyPayload()
	if len(payload) == 0 {
		return nil, errors.New("blocking delivery carried no reply body")
	}
	parts, err := PartsFromPayload(payload)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errors.New("blocking delivery carried an empty reply body")
	}
	contextID := t.ContextID
	if contextID == "" {
		contextID = accept.ID
	}
	msg := &Message{
		// The reply's message id is the delivery id it answers: crier mints the
		// id for the delivery, and a second invented one would be a source of
		// truth that could disagree with the accept the sender already has.
		MessageID: accept.ID,
		ContextID: contextID,
		Role:      RoleAgent,
		Parts:     parts,
	}
	if t.SentMessage != nil && t.SentMessage.MessageID != "" {
		msg.Metadata = map[string]any{"crier": map[string]any{
			"transport":  accept.Transport,
			"replies_to": t.SentMessage.MessageID,
			"replay":     accept.IdempotentReplay,
		}}
	}
	_ = now
	return msg, nil
}

// formatTimestamp renders an RFC 3339 instant the way protoJSON's Timestamp
// does: UTC with a Z suffix, nanoseconds only when they are non-zero.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// Timestamp renders an instant as the RFC 3339 string an A2A Timestamp field
// carries (§5.6.1), so every event this binding writes stamps the same way.
func Timestamp(t time.Time) string { return formatTimestamp(t) }

// ErrorFromStatus maps a non-2xx answer from crier's delivery engine onto the
// JSON-RPC error object this binding reports, carrying the delivery engine's own
// body verbatim so a client sees exactly what crier said.
//
// The table, and why each row is what it is:
//
//   - 400 → InvalidParams. The request parameters really were unusable, and
//     crier's own message (e.g. "payload is required") is the description.
//   - 403 → CodeDeliveryRefused, crier's implementation-defined server error
//     carrying crier's own machine-readable reason (GUARD_BLOCKED,
//     AGENT_QUARANTINED). A2A has no error for "the receiving agent's policy
//     refused this message", and reporting one of its named errors would be a
//     lie about what happened.
//   - 404 → TaskNotFoundError. The target agent turned out not to be on this
//     relay (or in the federation of relays), which is the one A2A-named error
//     whose meaning includes "not accessible here".
//   - anything else (409, 5xx) → InternalError, with crier's status and body in
//     the details. These are server-side outcomes the client cannot fix by
//     changing its request.
func ErrorFromStatus(status int, body []byte) *RPCError {
	reason := crierErrorCode(body)
	message := crierErrorMessage(body)
	refusal := DeliveryRefusal{
		Type:   DeliveryRefusalType,
		Status: status,
		Body:   json.RawMessage(body),
	}

	switch {
	case status == 400:
		return NewRPCError(CodeInvalidParams,
			fmt.Sprintf("the delivery was refused by crier (%s)", message), refusal)
	case status == 401 || status == 403:
		if reason == "" {
			reason = "DELIVERY_REFUSED"
		}
		return NewRPCError(CodeDeliveryRefused, message,
			ErrorInfo{Type: ErrorInfoType, Reason: reason, Domain: ErrorDomain,
				Metadata: map[string]string{"crierStatus": strconv.Itoa(status)}},
			refusal)
	case status == 404:
		return NewRPCError(CodeTaskNotFoundError,
			fmt.Sprintf("the target agent is not on this relay or in its federation: %s", message),
			ErrorInfo{Type: ErrorInfoType, Reason: "TASK_NOT_FOUND", Domain: ErrorDomain,
				Metadata: map[string]string{"crierStatus": strconv.Itoa(status)}},
			refusal)
	default:
		return NewRPCError(CodeInternalError,
			fmt.Sprintf("crier could not complete the delivery (HTTP %d): %s", status, message),
			refusal)
	}
}

// crierErrorCode reads the machine-readable `error` member crier's refusal
// bodies carry (e.g. GUARD_BLOCKED, AGENT_QUARANTINED, FEDERATION_FAILED).
func crierErrorCode(body []byte) string {
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Error
}

// crierErrorMessage renders the human-readable reason of a crier refusal, or the
// body itself when it is not one of the JSON error objects.
func crierErrorMessage(body []byte) string {
	if code := crierErrorCode(body); code != "" {
		return code
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "no response body"
	}
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return text
}

// TaskTopic is the relay topic a task's updates are published on, and the topic
// SendStreamingMessage subscribes to. The convention is stated once, here, so
// both the streaming adapter and the row that owns SubscribeToTask address the
// same name.
//
// crier's relay is explicitly a bus: a task's progress is produced by the agent
// working it (or by anything that knows the task id), published through the
// ordinary POST /relay/publish path, and the A2A stream is the SSE view of that
// subscription. No new publish path is introduced, and the WebSocket subscribers
// of the same topic see the same frames.
//
// ok=false means the task id cannot be a relay topic segment, in which case the
// stream carries the task's own lifecycle and no relay subscription — never a
// silently mangled topic name.
func TaskTopic(taskID string) (string, bool) {
	if taskID == "" || len(taskID) > 200 {
		return "", false
	}
	for _, r := range taskID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return "", false
		}
	}
	return TaskTopicPrefix + taskID, true
}

// TaskTopicPrefix namespaces the relay topic of one A2A task.
const TaskTopicPrefix = "a2a.task."
