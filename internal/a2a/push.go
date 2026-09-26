// push.go — the A2A push-notification configuration surface (INT-A2A-005,
// specs/A2A-OPTION.md §5.5).
//
// A2A's TaskPushNotificationConfig operations (§3.1.7-§3.1.10, §9.4.7) are
// served over the SAME JSON-RPC binding as SendMessage (POST /a2a): create,
// get, list and delete are four METHODS of the one binding crier implements
// (§2's binding decision — the HTTP+JSON/REST binding of §11 is declared MAY and
// is not built, so there is no /tasks/{id}/pushNotificationConfigs route and
// adding one would be a third A2A surface this option does not have).
//
// The configuration is a VIEW over crier's existing per-agent webhook config
// (§3's mapping table: TaskPushNotificationConfig ⇄ webhook.Config). This file
// holds the A2A half — the wire shape, the strict params decoders, the id
// derivation, the projection out of a row, and the validation of a write. It
// deliberately mirrors CardRow's discipline and imports NOTHING from
// internal/registry or internal/webhook: the row arrives as plain values
// (PushRow), and the crier-side config is assembled by the caller that owns it
// (cmd/server/a2apush.go).
//
// Two properties are load-bearing and stated here rather than implied:
//
//   - the capability answer is a MUST, not a nicety (§3.3.4). An agent whose
//     AgentCard says pushNotifications:false — i.e. an agent with no crier
//     webhook configured — gets PushNotificationNotSupportedError from ALL FOUR
//     operations. There is no silent success: a config CRUD that answered 200
//     while no push channel exists would be exactly the class of dishonest
//     surface DF-CRIER-279 fixed in the templating path.
//   - crier keeps ONE push configuration per agent, because crier has one push
//     channel per agent (the webhook). The A2A config id is therefore DERIVED
//     from the agent and its configured url rather than stored, so there is no
//     second store that could disagree with the webhook config.
package a2a

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The push-notification configuration methods (§9.4.7), verbatim — the
// specification's own PascalCase operation names are the wire `method` strings.
const (
	// MethodCreateTaskPushNotificationConfig creates a push configuration.
	MethodCreateTaskPushNotificationConfig = "CreateTaskPushNotificationConfig"
	// MethodGetTaskPushNotificationConfig retrieves one push configuration.
	MethodGetTaskPushNotificationConfig = "GetTaskPushNotificationConfig"
	// MethodListTaskPushNotificationConfigs lists an agent's push configs.
	MethodListTaskPushNotificationConfigs = "ListTaskPushNotificationConfigs"
	// MethodDeleteTaskPushNotificationConfig removes a push configuration.
	MethodDeleteTaskPushNotificationConfig = "DeleteTaskPushNotificationConfig"
)

// PushNotificationMediaType is the Content-Type §4.3.3 fixes for the payload a
// push channel receives. It is the same media type the binding answers with,
// which is the point: a push notification and a stream frame carry the same
// StreamResponse.
const PushNotificationMediaType = CardMediaType

// pushConfigIDPrefix marks a derived configuration id as one this server
// assigned, so a client can tell it from a value it invented itself.
const pushConfigIDPrefix = "wh-"

// PushConfigID returns the id of the push configuration an agent with this url
// has. It is DERIVED, never stored: crier has exactly one push channel per agent
// (the registry row's webhook, §3), so there is nothing to enumerate and no
// second store that could disagree with the webhook config.
//
// Deriving it from (agent, url) also makes the "does not exist" answer honest
// (§3.1.8): change the agent's url — through crier's own PATCH /agents/{id} or
// through these operations — and the configuration a previously issued id named
// genuinely no longer exists, which is what GetTaskPushNotificationConfig and
// DeleteTaskPushNotificationConfig report.
func PushConfigID(tenant, url string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + url))
	return pushConfigIDPrefix + hex.EncodeToString(sum[:8])
}

// PushAuthenticationInfo is the §4.3.2 authentication detail. crier reads the
// scheme and REFUSES the credential member: a secret is never stored in a
// registry row (see ValidatePushCreate).
type PushAuthenticationInfo struct {
	// Scheme is an IANA HTTP authentication scheme, case-insensitive per
	// RFC 9110 §11.1. crier serves none and Bearer, which are the two
	// schemes its webhook config can actually honour.
	Scheme string `json:"scheme"`
	// Credentials is the §4.3.2 credential itself. It is never accepted on the
	// way in and never returned on the way out: crier's webhook config
	// references a secret by name (auth_value_ref, resolved out of band at
	// send time) and holds no credential to return.
	Credentials string `json:"credentials,omitempty"`
}

// TaskPushNotificationConfig is the §4.3.1 object (v1.0's FLATTENED shape: the
// configuration's own fields live on the container). crier answers with exactly
// these members and no invented ones.
type TaskPushNotificationConfig struct {
	// Tenant is the agent this configuration belongs to — the registry id the
	// Agent Card publishes as AgentInterface.tenant.
	Tenant string `json:"tenant,omitempty"`
	// ID is the derived configuration id (PushConfigID).
	ID string `json:"id,omitempty"`
	// TaskID is the task the client addressed the operation with. crier's push
	// configuration is per AGENT (the mapping table's row), so the same
	// configuration serves every task of that agent; the value here is the
	// client's own addressing echoed back, and §5.5.2 states that rather than
	// pretending per-task configurations exist.
	TaskID string `json:"taskId,omitempty"`
	// URL is the endpoint the push notification is POSTed to: crier's
	// webhook.url, which is the same field that routes an ordinary delivery.
	URL string `json:"url"`
	// Token is the §4.3.1 per-task notification token. crier has no such field
	// in its webhook config, so it is never returned and never accepted: a
	// token this server stored but never sent would be a silent lie, and
	// crier's own authentication mechanism is the named secret below.
	Token string `json:"token,omitempty"`
	// Authentication describes how the notification is authenticated, and it
	// describes it truthfully: a scheme, never a credential.
	Authentication *PushAuthenticationInfo `json:"authentication,omitempty"`
}

// ListPushConfigsResponse is the §3.1.9 result.
type ListPushConfigsResponse struct {
	// Configs holds this agent's push configurations. crier has exactly one
	// per agent, so this is either one entry or the operation answered
	// PushNotificationNotSupportedError.
	Configs []TaskPushNotificationConfig `json:"configs"`
	// NextPageToken is absent: crier serves one configuration per agent, so
	// there is no next page and no page token is ever issued.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// DeletePushConfigResult is §3.1.10's "confirmation of deletion
// (implementation-specific)".
type DeletePushConfigResult struct {
	Deleted bool   `json:"deleted"`
	ID      string `json:"id"`
	Tenant  string `json:"tenant,omitempty"`
}

// PushRow is the registry-row evidence the push-config operations need, passed
// in as plain values so this package stays independent of internal/registry —
// the same rule CardRow follows.
type PushRow struct {
	// Tenant is the registry id.
	Tenant string
	// Configured reports whether the row carries a crier webhook — the push
	// channel. It is the local form of AgentCard.capabilities.pushNotifications
	// (§5.3), so the capability answer and the card can never disagree.
	Configured bool
	// URL is the configured push endpoint.
	URL string
	// AuthType is the row's webhook auth type: "" | "none" | "bearer".
	AuthType string
	// AuthRefSet reports whether the row names a secret for bearer auth
	// (auth_value_ref). The reference is NEVER carried through this package:
	// only the fact that one exists, because the A2A view says "authenticated
	// with a bearer scheme" and never "here is the secret's name".
	AuthRefSet bool
	// CustomSchemaSet reports whether the row's webhook carries a
	// bring-your-own schema, which crier resolves BEFORE any named template
	// (ResolveTemplate). A config that has one cannot also promise this
	// binding's notification payload shape (see ValidatePushCreate).
	CustomSchemaSet bool
}

// CreatePushConfigParams is the `params` of CreateTaskPushNotificationConfig
// (§3.1.7): the flattened configuration the client asks for.
type CreatePushConfigParams struct {
	Tenant         string
	ID             string
	TaskID         string
	URL            string
	Token          string
	Authentication *PushAuthenticationInfo
}

// PushConfigRefParams is the `params` of Get and Delete
// (Get/DeleteTaskPushNotificationConfig), which address one configuration.
type PushConfigRefParams struct {
	Tenant string
	TaskID string
	ID     string
}

// ListPushConfigsParams is the `params` of ListTaskPushNotificationConfigs.
type ListPushConfigsParams struct {
	Tenant    string
	TaskID    string
	PageSize  *int
	PageToken string
}

// wire shapes of the params objects: the nested authentication object is kept
// raw so a refusal can NAME the path it came from (`authentication.credentials`)
// instead of reporting json's pathless "unknown field" for a member one level
// down. Mirrors the send binding's wireSendParams discipline.
type wirePushConfig struct {
	Tenant         string          `json:"tenant"`
	ID             string          `json:"id"`
	TaskID         string          `json:"taskId"`
	URL            string          `json:"url"`
	Token          string          `json:"token"`
	Authentication json.RawMessage `json:"authentication"`
}

type wirePushConfigRef struct {
	Tenant string `json:"tenant"`
	TaskID string `json:"taskId"`
	ID     string `json:"id"`
}

type wireListPushConfigs struct {
	Tenant    string `json:"tenant"`
	TaskID    string `json:"taskId"`
	PageSize  *int   `json:"pageSize"`
	PageToken string `json:"pageToken"`
}

type wirePushAuthentication struct {
	Scheme      string `json:"scheme"`
	Credentials string `json:"credentials"`
}

// pushParamsObject reads a `params` member as a JSON object, which every push
// operation requires: the object IS the configuration (§4.3.1's flattened
// shape), so there is nothing these operations could do with a scalar.
func pushParamsObject(raw json.RawMessage) ([]byte, *RPCError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, NewRPCError(CodeInvalidParams, "params is required",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "is required"}))
	}
	if trimmed[0] != '{' {
		return nil, NewRPCError(CodeInvalidParams, "params must be a JSON object",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "must be a JSON object"}))
	}
	return trimmed, nil
}

// DecodeCreatePushParams strictly decodes the `params` of
// CreateTaskPushNotificationConfig (§3.1.7), refusing everything it cannot
// carry faithfully and naming the field:
//
//   - an unknown member is refused with the accepted set (the same discipline
//     the send params and crier's own `a2a` registration block are held to);
//   - `url` is REQUIRED (§4.3.1 marks it Yes);
//   - the nested `authentication` object is strict-decoded too, so a misspelled
//     scheme is a refusal rather than a dropped member.
func DecodeCreatePushParams(raw json.RawMessage) (*CreatePushConfigParams, *RPCError) {
	trimmed, rpcErr := pushParamsObject(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var wire wirePushConfig
	if err := strictDecode(trimmed, &wire); err != nil {
		return nil, invalidParamsError("params", err)
	}
	params := &CreatePushConfigParams{
		Tenant: wire.Tenant,
		ID:     wire.ID,
		TaskID: wire.TaskID,
		URL:    wire.URL,
		Token:  wire.Token,
	}
	if auth, rpcErr := decodePushAuthentication(wire.Authentication); rpcErr != nil {
		return nil, rpcErr
	} else if auth != nil {
		params.Authentication = auth
	}
	if strings.TrimSpace(params.URL) == "" {
		return nil, invalidParamsError("url", errRequired)
	}
	return params, nil
}

// decodePushAuthentication strict-decodes the optional §4.3.2 object. A JSON
// null (or an absent member) is (nil, nil): the client said nothing about
// authentication, which the write reads as "leave the row's auth alone".
func decodePushAuthentication(raw json.RawMessage) (*PushAuthenticationInfo, *RPCError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var wire wirePushAuthentication
	if err := strictDecode(trimmed, &wire); err != nil {
		return nil, invalidParamsError("authentication", err)
	}
	return &PushAuthenticationInfo{Scheme: wire.Scheme, Credentials: wire.Credentials}, nil
}

// DecodePushConfigRefParams strictly decodes the `params` of Get and Delete
// (§3.1.8, §3.1.10): both mark taskId and id Required, and so does this
// binding — a configuration is addressed by both, and answering without one of
// them would be answering a question the client did not ask.
func DecodePushConfigRefParams(raw json.RawMessage) (*PushConfigRefParams, *RPCError) {
	trimmed, rpcErr := pushParamsObject(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var wire wirePushConfigRef
	if err := strictDecode(trimmed, &wire); err != nil {
		return nil, invalidParamsError("params", err)
	}
	if strings.TrimSpace(wire.TaskID) == "" {
		return nil, invalidParamsError("taskId", errRequired)
	}
	if strings.TrimSpace(wire.ID) == "" {
		return nil, invalidParamsError("id", errRequired)
	}
	return &PushConfigRefParams{Tenant: wire.Tenant, TaskID: wire.TaskID, ID: wire.ID}, nil
}

// DecodeListPushConfigsParams strictly decodes the `params` of
// ListTaskPushNotificationConfigs (§3.1.9). `taskId` is Required. Paging is
// DECLINED rather than half-implemented: crier serves one configuration per
// agent, so it never issues a page token, and a token it never issued is
// refused instead of being silently ignored — a client that is paging thinks it
// has more results to fetch, and this server has none.
func DecodeListPushConfigsParams(raw json.RawMessage) (*ListPushConfigsParams, *RPCError) {
	trimmed, rpcErr := pushParamsObject(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var wire wireListPushConfigs
	if err := strictDecode(trimmed, &wire); err != nil {
		return nil, invalidParamsError("params", err)
	}
	if strings.TrimSpace(wire.TaskID) == "" {
		return nil, invalidParamsError("taskId", errRequired)
	}
	if wire.PageSize != nil && *wire.PageSize < 1 {
		return nil, invalidParamsError("pageSize", errors.New("must be at least 1"))
	}
	if strings.TrimSpace(wire.PageToken) != "" {
		return nil, invalidParamsError("pageToken", errors.New(
			"is not a token this server issued: crier serves at most one push configuration per agent, so ListTaskPushNotificationConfigs is never paged and no nextPageToken is ever returned"))
	}
	return &ListPushConfigsParams{Tenant: wire.Tenant, TaskID: wire.TaskID, PageSize: wire.PageSize}, nil
}

// PushWrite is the change a validated create asks crier to make to its own
// webhook config. It carries only the fields the A2A view OWNS; everything else
// on the config (retries, timeout_ms, batch, delivery_mode, schema_template,
// the secret reference) is preserved by the caller, because the A2A shape has
// no field for it — and dropping what a request cannot express would be a
// silent loss of operator configuration.
type PushWrite struct {
	// URL is the endpoint the notification is POSTed to (webhook.url).
	URL string
	// AuthType is the auth type to write: "" leaves the row's auth alone,
	// "none" and "bearer" set it.
	AuthType string
	// ClearAuthRef reports that the write must also clear the named secret
	// (scheme none: nothing would use it).
	ClearAuthRef bool
}

// ValidatePushCreate checks a create request against the row's evidence and
// returns the write it asks for, or the refusal it earns.
//
// The refusals, and why each is a refusal rather than a silent adjustment:
//
//   - the agent has no push channel → PushNotificationNotSupportedError, the
//     §3.3.4 capability MUST (the caller raises this before calling here, and it
//     is raised here as well so the two call sites cannot drift);
//   - `authentication.credentials` set → -32602. crier never stores a raw
//     credential in a registry row; the secret is named (auth_value_ref,
//     resolved out of band at send time) and stored where secrets belong.
//     Accepting the credential and dropping it would report an authentication
//     that does not exist;
//   - a scheme crier cannot honour → -32602 naming the two it can (none,
//     Bearer). crier's webhook driver emits exactly those;
//   - Bearer with no secret named on the row → -32602 saying what to do first,
//     because the alternative is a config that claims authentication and sends
//     none;
//   - `token` set → -32602. crier's webhook config has no notification-token
//     field: accepting one would promise a token that no delivery ever carries;
//   - a row whose webhook already has a bring-your-own custom_schema → -32004.
//     crier resolves a custom schema BEFORE any named template, so a config
//     that has one cannot also promise §4.3.3's payload shape; the alternative
//     would be to overwrite the operator's schema without saying so.
func ValidatePushCreate(params *CreatePushConfigParams, row PushRow) (*PushWrite, *RPCError) {
	if !row.Configured {
		return nil, PushNotSupportedError(row.Tenant)
	}

	derived := PushConfigID(row.Tenant, params.URL)
	if id := strings.TrimSpace(params.ID); id != "" && id != derived {
		return nil, invalidParamsError("id", fmt.Errorf(
			"names configuration %q, but this agent's push configuration for %s is %q: crier serves ONE push configuration per agent (its webhook, the channel that agent actually has) and assigns its id from the agent and the url, so there is no second configuration for a client-supplied id to address — omit \"id\" to have the server assign it",
			id, params.URL, derived))
	}
	if token := strings.TrimSpace(params.Token); token != "" {
		return nil, invalidParamsError("token", errors.New(
			"is not a field of a crier push configuration: crier's push channel is the agent's webhook config, which carries a named secret reference (auth_value_ref) rather than a per-task notification token, and no delivery would ever send this value — use \"authentication\" for the scheme the notification is sent with"))
	}
	if row.CustomSchemaSet {
		return nil, NewRPCError(CodeUnsupportedOperationError,
			fmt.Sprintf("agent %q has a bring-your-own custom_schema on its webhook config, and crier resolves a custom schema BEFORE the notification schema this operation installs — the push configuration would claim a payload shape crier would not send. Clear custom_schema on the agent row first (PATCH /agents/%s with {\"webhook\":{...,\"custom_schema\":null}}) or keep it and configure the push channel there",
				row.Tenant, row.Tenant),
			ErrorInfo{Type: ErrorInfoType, Reason: "PUSH_CONFIG_CUSTOM_SCHEMA_SET", Domain: ErrorDomain,
				Metadata: map[string]string{"tenant": row.Tenant}})
	}

	write := &PushWrite{URL: params.URL}
	if params.Authentication == nil {
		return write, nil
	}
	if strings.TrimSpace(params.Authentication.Credentials) != "" {
		return nil, invalidParamsError("authentication.credentials", errors.New(
			"is never accepted: crier stores no credential in a registry row — the webhook config references a secret by name (auth_value_ref) and the delivery driver resolves it at send time, so a credential posted here would be either stored where secrets do not belong or dropped while the client believed it was sent"))
	}
	switch scheme := strings.ToLower(strings.TrimSpace(params.Authentication.Scheme)); scheme {
	case "":
		return nil, invalidParamsError("authentication.scheme", errRequired)
	case "none":
		// An explicit "no authentication": crier's own auth_type=none, and the
		// named secret is cleared because nothing would use it.
		write.AuthType = string(AuthNone)
		write.ClearAuthRef = true
	case "bearer":
		if !row.AuthRefSet {
			return nil, invalidParamsError("authentication.scheme", errors.New(
				"is Bearer, but this agent's webhook config names no secret (auth_value_ref) for it: crier sends the notification with the secret the row references and never with a credential supplied here, so name it on the agent row first (PATCH /agents/{id}, webhook.auth_value_ref)"))
		}
		write.AuthType = string(AuthBearer)
	default:
		return nil, invalidParamsError("authentication.scheme",
			fmt.Errorf("is %q, which crier cannot honour: the push notification is authenticated by crier's webhook driver, whose accepted schemes are %s and %s",
				params.Authentication.Scheme, AuthNone, AuthBearer))
	}
	return write, nil
}

// AuthNone and AuthBearer are the two schemes crier's webhook driver emits,
// spelled here as the strings an A2A client puts in `authentication.scheme`
// (comparison is case-insensitive per RFC 9110 §11.1). The caller maps them
// onto internal/webhook's own constants — this package names no webhook type.
const (
	AuthNone   = "none"
	AuthBearer = "bearer"
)

// ProjectPushConfig renders the §4.3.1 object for an agent's push channel.
//
// The capability answer is checked here as well as before every operation, so
// no call path can project a configuration for an agent that has none: an
// agent without a push channel has no configuration to describe.
func ProjectPushConfig(tenant, taskID string, row PushRow) (TaskPushNotificationConfig, *RPCError) {
	if !row.Configured {
		return TaskPushNotificationConfig{}, PushNotSupportedError(tenant)
	}
	cfg := TaskPushNotificationConfig{
		Tenant: tenant,
		ID:     PushConfigID(tenant, row.URL),
		TaskID: taskID,
		URL:    row.URL,
	}
	if strings.EqualFold(row.AuthType, AuthBearer) {
		// The SCHEME, never the credential: crier holds a reference to a
		// secret, not the secret. Credentials stays empty for exactly that
		// reason (§5.5.2).
		cfg.Authentication = &PushAuthenticationInfo{Scheme: "Bearer"}
	}
	return cfg, nil
}

// MatchPushConfigID reports whether id names this agent's push configuration,
// and otherwise the §3.1.8/§3.1.10 not-found refusal ("the push notification
// configuration does not exist").
func MatchPushConfigID(tenant, id string, row PushRow) *RPCError {
	if id == PushConfigID(tenant, row.URL) {
		return nil
	}
	return NewRPCError(CodeTaskNotFoundError, fmt.Sprintf(
		"no push notification configuration %q for agent %q: its configuration is %q (the id is derived from the agent and the url crier pushes to, so a changed url is a different configuration)",
		id, tenant, PushConfigID(tenant, row.URL)),
		ErrorInfo{Type: ErrorInfoType, Reason: "PUSH_CONFIG_NOT_FOUND", Domain: ErrorDomain,
			Metadata: map[string]string{"tenant": tenant, "id": id}})
}

// PushNotSupportedError is the §3.3.4 capability refusal: -32003
// PushNotificationNotSupportedError, raised by all four operations when the
// target agent has no push channel. AgentCard.capabilities.pushNotifications is
// false for exactly these agents (§5.3), so the card and the operations answer
// the same question the same way.
func PushNotSupportedError(tenant string) *RPCError {
	return NewRPCError(CodePushNotificationNotSupportedError, fmt.Sprintf(
		"push notifications are not supported by agent %q: crier's push channel for an agent is its webhook config, and this agent has none — its Agent Card already says so (capabilities.pushNotifications is false, §3.3.4). Configure one with POST /agents or PATCH /agents/%s (webhook.url), then retry",
		tenant, tenant),
		ErrorInfo{Type: ErrorInfoType, Reason: "PUSH_NOTIFICATION_NOT_SUPPORTED", Domain: ErrorDomain,
			Metadata: map[string]string{"tenant": tenant, "capability": "pushNotifications"}})
}

// ConfigRefusal is crier's own detail object for a push-config write crier
// refused: the status and body its own route answered, carried verbatim so a
// client sees exactly what crier said rather than a re-telling of it. It is the
// write-path sibling of DeliveryRefusal, and it is a separate type on purpose —
// the two name different things, and a client shouldn't have to guess which one
// it is holding.
type ConfigRefusal struct {
	Type   string          `json:"@type"`
	Status int             `json:"crierStatus"`
	Body   json.RawMessage `json:"crierBody,omitempty"`
}

// ConfigRefusalType is ConfigRefusal's detail type URI (§9.5).
const ConfigRefusalType = "type.googleapis.com/crier.ConfigRefusal"

// PushWriteRefused renders a refusal from crier's own registry-update route as
// the JSON-RPC error it should travel as.
//
// The mapping is by what the client can do about it: a 400 is a parameter
// problem (-32602, with crier's own validation message), a 401/403 is crier's
// own authorization for an agent-owned write and has no A2A name (-32050, the
// implementation-defined server error, with crier's reason), a 404 means the
// agent is not a row here any more (-32001), and anything else is a server-side
// outcome the client cannot fix by editing its request (-32603).
func PushWriteRefused(status int, body []byte) *RPCError {
	reason := crierErrorCode(body)
	message := crierErrorMessage(body)
	refusal := ConfigRefusal{Type: ConfigRefusalType, Status: status, Body: json.RawMessage(body)}

	switch {
	case status == 400:
		return NewRPCError(CodeInvalidParams,
			fmt.Sprintf("crier refused the push configuration: %s", message), refusal)
	case status == 401 || status == 403:
		if reason == "" {
			reason = "CONFIG_REFUSED"
		}
		return NewRPCError(CodeDeliveryRefused, message,
			ErrorInfo{Type: ErrorInfoType, Reason: reason, Domain: ErrorDomain,
				Metadata: map[string]string{"crierStatus": fmt.Sprintf("%d", status)}},
			refusal)
	case status == 404:
		return NewRPCError(CodeTaskNotFoundError,
			fmt.Sprintf("the agent is not a registry row on this relay: %s", message),
			ErrorInfo{Type: ErrorInfoType, Reason: "TASK_NOT_FOUND", Domain: ErrorDomain,
				Metadata: map[string]string{"crierStatus": fmt.Sprintf("%d", status)}},
			refusal)
	default:
		return NewRPCError(CodeInternalError,
			fmt.Sprintf("crier could not write the push configuration (HTTP %d): %s", status, message),
			refusal)
	}
}

// NotificationShape is the notification payload shape a push configuration
// installs on crier's webhook config — the §4.3.3 StreamResponse envelope,
// expressed in crier's OWN bring-your-own-schema mechanism rather than as a
// second delivery path.
//
// The members are the values the delivery driver fills in, and the caller maps
// them onto internal/webhook's RequestShape/CustomSchema (this package names no
// webhook type).
type NotificationShape struct {
	// Body is the crier request-schema template that renders the notification.
	Body json.RawMessage
	// Headers is the request header set §4.3.3 fixes: the payload is an A2A
	// StreamResponse, and it says so.
	Headers map[string]string
	// ResponseMap is crier's reply-extraction rule for the endpoint's answer.
	// "raw" is crier's own default and the honest one here: the binding makes
	// no claim about the shape of a client's 2xx.
	ResponseMap string
}

// pushNotificationBody is the template above. Every placeholder resolves
// against the crier envelope the delivery driver renders from, and every one
// that can be ABSENT carries an explicit `|default:` — crier's templating FAILS
// the delivery loudly on a placeholder the context cannot fill (DF-CRIER-279),
// so an absent optional field must be spelled as "renders empty" here or a
// delivery with no session id would fail.
//
// The task's content is deliberately NOT carried: crier's payload is opaque, the
// template context cannot faithfully project a non-object payload, and a
// notification that silently dropped the message body would be worse than one
// that does not claim to carry it. The notification carries the task's identity
// and its state, plus the crier facts A2A has no field for under
// metadata.crier — the same convention §5.4.4 states for a send's Task.
var pushNotificationBody = json.RawMessage(fmt.Sprintf(`{
  "task": {
    "id": "{{crier.message_id}}",
    "contextId": "{{crier.session_id|default:}}",
    "status": {"state": %q},
    "metadata": {"crier": {
      "transport": "webhook",
      "kind": "{{crier.kind|default:message}}",
      "delivery_mode": "{{crier.delivery_mode|default:}}",
      "sender": "{{crier.sender|default:}}",
      "target": "{{crier.target|default:}}",
      "thread_id": "{{crier.thread_id|default:}}",
      "request_id": "{{crier.request_id|default:}}",
      "namespace": "{{crier.namespace|default:}}"
    }}
  }
}`, string(TaskStateSubmitted)))

// PushNotificationShape returns the shape above. The body is copied so a caller
// cannot mutate the package's template.
func PushNotificationShape() NotificationShape {
	headers := make(map[string]string, 1)
	headers["Content-Type"] = PushNotificationMediaType
	return NotificationShape{
		Body:        append(json.RawMessage(nil), pushNotificationBody...),
		Headers:     headers,
		ResponseMap: "raw",
	}
}
