package a2a

// push_test.go — INT-A2A-005: the push-notification configuration surface, at
// the unit level.
//
// The live behaviour (a booted server, a real delivery, the notification body on
// the wire) is asserted in cmd/server/a2apush_test.go; this file pins the parts
// that live below the transport: the strict params decoders, the derived
// configuration id, the projection out of a row, the validation of a write
// (including every refusal and its code), and the shape of the notification
// template.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func pushRowConfigured() PushRow {
	return PushRow{
		Tenant:     "agent-a",
		Configured: true,
		URL:        "https://hooks.example.com/a2a",
		AuthType:   "bearer",
		AuthRefSet: true,
	}
}

// TestPushConfigID_IsDerivedFromTheAgentAndTheURL: the id is not stored, and it
// moves when either half of what it names moves — which is what makes the
// "configuration does not exist" answer honest after the url changes.
func TestPushConfigID_IsDerivedFromTheAgentAndTheURL(t *testing.T) {
	base := PushConfigID("agent-a", "https://hooks.example.com/a2a")
	if base != PushConfigID("agent-a", "https://hooks.example.com/a2a") {
		t.Fatal("PushConfigID is not deterministic")
	}
	if !strings.HasPrefix(base, pushConfigIDPrefix) {
		t.Errorf("id = %q, want the %q prefix that marks a server-assigned id", base, pushConfigIDPrefix)
	}
	if other := PushConfigID("agent-b", "https://hooks.example.com/a2a"); other == base {
		t.Error("two agents sharing a url share an id; the id must name one agent's configuration")
	}
	if other := PushConfigID("agent-a", "https://hooks.example.com/other"); other == base {
		t.Error("a changed url left the id unchanged; a moved push endpoint is a different configuration")
	}
}

// TestDecodeCreatePushParams_Strictness pins the discipline the send params and
// crier's own `a2a` registration block are held to: a member this binding cannot
// honour is a refusal naming it, never a silently dropped field.
func TestDecodeCreatePushParams_Strictness(t *testing.T) {
	row := pushRowConfigured()
	cases := []struct {
		name       string
		params     string
		wantCode   int
		wantField  string
		wantInMsg  string
		wantAccept []string
	}{
		{
			name:      "an unknown member",
			params:    `{"tenant":"agent-a","url":"https://x.example/h","autoRetry":true}`,
			wantCode:  CodeInvalidParams,
			wantInMsg: `unknown field "autoRetry"`,
		},
		{
			name:      "an unknown member of the authentication object",
			params:    `{"tenant":"agent-a","url":"https://x.example/h","authentication":{"scheme":"Bearer","secret":"s"}}`,
			wantCode:  CodeInvalidParams,
			wantInMsg: `unknown field "secret"`,
		},
		{
			name:      "url absent",
			params:    `{"tenant":"agent-a","taskId":"t-1"}`,
			wantCode:  CodeInvalidParams,
			wantField: "url",
			wantInMsg: "is required",
		},
		{
			name:      "url blank",
			params:    `{"tenant":"agent-a","url":"   "}`,
			wantCode:  CodeInvalidParams,
			wantField: "url",
			wantInMsg: "is required",
		},
		{
			name:      "params is not an object",
			params:    `"https://x.example/h"`,
			wantCode:  CodeInvalidParams,
			wantInMsg: "params must be a JSON object",
		},
		{
			name:      "params absent",
			params:    ``,
			wantCode:  CodeInvalidParams,
			wantInMsg: "params is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr := DecodeCreatePushParams(json.RawMessage(tc.params))
			if rpcErr == nil {
				t.Fatal("the params were accepted; want a refusal")
			}
			if rpcErr.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", rpcErr.Code, tc.wantCode, rpcErr.Message)
			}
			if !strings.Contains(rpcErr.Message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", rpcErr.Message, tc.wantInMsg)
			}
			if tc.wantField != "" && !fieldViolationNames(rpcErr, tc.wantField) {
				t.Errorf("the refusal does not name field %q in a fieldViolation: %+v", tc.wantField, rpcErr.Data)
			}
		})
	}

	// The happy path, including the optional members the A2A shape defines.
	params, rpcErr := DecodeCreatePushParams(json.RawMessage(
		`{"tenant":"agent-a","id":"wh-x","taskId":"t-1","url":"https://x.example/h","authentication":{"scheme":"Bearer"}}`))
	if rpcErr != nil {
		t.Fatalf("a well-formed create was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if params.Tenant != "agent-a" || params.ID != "wh-x" || params.TaskID != "t-1" ||
		params.URL != "https://x.example/h" {
		t.Errorf("params decoded to %+v", params)
	}
	if params.Authentication == nil || params.Authentication.Scheme != "Bearer" {
		t.Errorf("authentication decoded to %+v", params.Authentication)
	}
	_ = row
}

// TestDecodePushConfigRefParams_RequiresBothHalves: §3.1.8/§3.1.10 mark taskId
// and id Required, and a configuration is addressed by both.
func TestDecodePushConfigRefParams_RequiresBothHalves(t *testing.T) {
	if _, rpcErr := DecodePushConfigRefParams(json.RawMessage(`{"tenant":"agent-a","id":"wh-x"}`)); rpcErr == nil ||
		!fieldViolationNames(rpcErr, "taskId") {
		t.Errorf("a ref without taskId = %+v, want a -32602 naming taskId", rpcErr)
	}
	if _, rpcErr := DecodePushConfigRefParams(json.RawMessage(`{"tenant":"agent-a","taskId":"t-1"}`)); rpcErr == nil ||
		!fieldViolationNames(rpcErr, "id") {
		t.Errorf("a ref without id = %+v, want a -32602 naming id", rpcErr)
	}
	ref, rpcErr := DecodePushConfigRefParams(json.RawMessage(`{"tenant":"agent-a","taskId":"t-1","id":"wh-x"}`))
	if rpcErr != nil {
		t.Fatalf("a well-formed ref was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if ref.Tenant != "agent-a" || ref.TaskID != "t-1" || ref.ID != "wh-x" {
		t.Errorf("ref decoded to %+v", ref)
	}
}

// TestDecodeListPushConfigsParams_PagingIsDeclinedNotIgnored: crier serves one
// configuration per agent, so it never issues a page token. A token it did not
// issue is refused — a client that is paging believes there is more to fetch.
func TestDecodeListPushConfigsParams_PagingIsDeclinedNotIgnored(t *testing.T) {
	if _, rpcErr := DecodeListPushConfigsParams(json.RawMessage(`{"tenant":"agent-a"}`)); rpcErr == nil ||
		!fieldViolationNames(rpcErr, "taskId") {
		t.Errorf("a list without taskId = %+v, want a -32602 naming taskId", rpcErr)
	}
	if _, rpcErr := DecodeListPushConfigsParams(json.RawMessage(`{"tenant":"agent-a","taskId":"t-1","pageToken":"abc"}`)); rpcErr == nil ||
		!fieldViolationNames(rpcErr, "pageToken") {
		t.Errorf("a page token = %+v, want a -32602 naming pageToken", rpcErr)
	}
	zero := `{"tenant":"agent-a","taskId":"t-1","pageSize":0}`
	if _, rpcErr := DecodeListPushConfigsParams(json.RawMessage(zero)); rpcErr == nil ||
		!fieldViolationNames(rpcErr, "pageSize") {
		t.Errorf("pageSize 0 = %+v, want a -32602 naming pageSize", rpcErr)
	}
	params, rpcErr := DecodeListPushConfigsParams(json.RawMessage(`{"tenant":"agent-a","taskId":"t-1","pageSize":10}`))
	if rpcErr != nil {
		t.Fatalf("a well-formed list was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if params.PageSize == nil || *params.PageSize != 10 {
		t.Errorf("pageSize decoded to %+v", params.PageSize)
	}
}

// TestValidatePushCreate_CapabilityFirst: an agent with no push channel gets the
// §3.3.4 capability error from the create, whatever else the request says — a
// silent success here would be the dishonest surface DF-CRIER-279 is about.
func TestValidatePushCreate_CapabilityFirst(t *testing.T) {
	_, rpcErr := ValidatePushCreate(&CreatePushConfigParams{URL: "https://x.example/h"}, PushRow{Tenant: "agent-a"})
	if rpcErr == nil || rpcErr.Code != CodePushNotificationNotSupportedError {
		t.Fatalf("create against an agent with no push channel = %+v, want -32003", rpcErr)
	}
	if !strings.Contains(rpcErr.Message, "pushNotifications") {
		t.Errorf("the capability refusal must name the capability the card states: %q", rpcErr.Message)
	}
	if !hasErrorInfoReason(rpcErr, "PUSH_NOTIFICATION_NOT_SUPPORTED") {
		t.Errorf("the refusal carries no ErrorInfo reason: %+v", rpcErr.Data)
	}
}

// TestValidatePushCreate_RefusalsAreLoud walks the members crier cannot honour.
func TestValidatePushCreate_RefusalsAreLoud(t *testing.T) {
	row := pushRowConfigured()
	cases := []struct {
		name      string
		params    CreatePushConfigParams
		row       PushRow
		wantCode  int
		wantField string
		wantInMsg string
		wantWhy   string
	}{
		{
			name:      "a client-supplied id that is not this configuration",
			params:    CreatePushConfigParams{URL: row.URL, ID: "my-own-uuid"},
			row:       row,
			wantCode:  CodeInvalidParams,
			wantField: "id",
			wantInMsg: "ONE push configuration per agent",
			wantWhy:   "the id is derived, so a value that names nothing must be refused rather than ignored",
		},
		{
			name:      "a notification token",
			params:    CreatePushConfigParams{URL: row.URL, Token: "task-token"},
			row:       row,
			wantCode:  CodeInvalidParams,
			wantField: "token",
			wantInMsg: "auth_value_ref",
			wantWhy:   "crier has no token field, and accepting one would promise a value no delivery carries",
		},
		{
			name: "a raw credential",
			params: CreatePushConfigParams{URL: row.URL, Authentication: &PushAuthenticationInfo{
				Scheme: "Bearer", Credentials: "an-actual-secret"}},
			row:       row,
			wantCode:  CodeInvalidParams,
			wantField: "authentication.credentials",
			wantInMsg: "never accepted",
			wantWhy:   "a secret is referenced by name, never stored in a registry row",
		},
		{
			name:      "an authentication scheme crier cannot honour",
			params:    CreatePushConfigParams{URL: row.URL, Authentication: &PushAuthenticationInfo{Scheme: "Basic"}},
			row:       row,
			wantCode:  CodeInvalidParams,
			wantField: "authentication.scheme",
			wantInMsg: "accepted schemes are none and bearer",
			wantWhy:   "crier's driver emits exactly none|bearer",
		},
		{
			name:      "authentication with no scheme",
			params:    CreatePushConfigParams{URL: row.URL, Authentication: &PushAuthenticationInfo{}},
			row:       row,
			wantCode:  CodeInvalidParams,
			wantField: "authentication.scheme",
			wantInMsg: "is required",
			wantWhy:   "§4.3.2 marks scheme Yes",
		},
		{
			name:      "Bearer with no secret named on the row",
			params:    CreatePushConfigParams{URL: row.URL, Authentication: &PushAuthenticationInfo{Scheme: "bearer"}},
			row:       PushRow{Tenant: "agent-a", Configured: true, URL: row.URL},
			wantCode:  CodeInvalidParams,
			wantField: "authentication.scheme",
			wantInMsg: "names no secret",
			wantWhy:   "the alternative is a config that claims authentication and sends none",
		},
		{
			name:      "a row that already has a bring-your-own schema",
			params:    CreatePushConfigParams{URL: row.URL},
			row:       PushRow{Tenant: "agent-a", Configured: true, URL: row.URL, CustomSchemaSet: true},
			wantCode:  CodeUnsupportedOperationError,
			wantInMsg: "custom_schema",
			wantWhy:   "crier resolves a custom schema first, so the A2A payload shape could not be honoured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write, rpcErr := ValidatePushCreate(&tc.params, tc.row)
			if rpcErr == nil {
				t.Fatalf("the create was accepted (write %+v); want a refusal — %s", write, tc.wantWhy)
			}
			if rpcErr.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", rpcErr.Code, tc.wantCode, rpcErr.Message)
			}
			if !strings.Contains(rpcErr.Message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", rpcErr.Message, tc.wantInMsg)
			}
			if tc.wantField != "" && !fieldViolationNames(rpcErr, tc.wantField) {
				t.Errorf("the refusal does not name %q in a fieldViolation: %+v", tc.wantField, rpcErr.Data)
			}
		})
	}
}

// TestValidatePushCreate_AcceptedWrites: what a create is allowed to change, and
// what it deliberately leaves alone. The A2A shape has no field for crier's own
// knobs, and the write says so instead of resetting them.
func TestValidatePushCreate_AcceptedWrites(t *testing.T) {
	row := pushRowConfigured()

	// A bare create: the url is the change, the row's authentication stands.
	write, rpcErr := ValidatePushCreate(&CreatePushConfigParams{URL: "https://new.example/h"}, row)
	if rpcErr != nil {
		t.Fatalf("a bare create was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if write.URL != "https://new.example/h" {
		t.Errorf("url = %q, want the requested one", write.URL)
	}
	if write.AuthType != "" || write.ClearAuthRef {
		t.Errorf("write = %+v, want no authentication change", write)
	}

	// scheme none: crier's own auth_type=none, and the named secret is cleared
	// because nothing would use it.
	write, rpcErr = ValidatePushCreate(&CreatePushConfigParams{
		URL: row.URL, Authentication: &PushAuthenticationInfo{Scheme: "None"}}, row)
	if rpcErr != nil {
		t.Fatalf("scheme none was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if write.AuthType != AuthNone || !write.ClearAuthRef {
		t.Errorf("write = %+v, want none + the reference cleared", write)
	}

	// Bearer, case-insensitive per RFC 9110 §11.1, with the secret the row
	// already names: kept, never carried as a credential.
	write, rpcErr = ValidatePushCreate(&CreatePushConfigParams{
		URL: row.URL, Authentication: &PushAuthenticationInfo{Scheme: "BeArEr"}}, row)
	if rpcErr != nil {
		t.Fatalf("Bearer was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if write.AuthType != AuthBearer || write.ClearAuthRef {
		t.Errorf("write = %+v, want bearer with the reference kept", write)
	}

	// The derived id is accepted when the client echoes it — that is what the
	// create returned in the first place.
	write, rpcErr = ValidatePushCreate(&CreatePushConfigParams{
		URL: row.URL, ID: PushConfigID(row.Tenant, row.URL)}, row)
	if rpcErr != nil {
		t.Fatalf("echoing the derived id was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if write.URL != row.URL {
		t.Errorf("write url = %q, want %q", write.URL, row.URL)
	}
}

// TestProjectPushConfig_IsTheA2AViewOfTheRow: the projection states the scheme
// and never the credential, and echoes the client's own taskId.
func TestProjectPushConfig_IsTheA2AViewOfTheRow(t *testing.T) {
	row := pushRowConfigured()
	cfg, rpcErr := ProjectPushConfig("agent-a", "task-7", row)
	if rpcErr != nil {
		t.Fatalf("projection refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if cfg.Tenant != "agent-a" || cfg.TaskID != "task-7" || cfg.URL != row.URL {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.ID != PushConfigID("agent-a", row.URL) {
		t.Errorf("id = %q, want the derived one", cfg.ID)
	}
	if cfg.Authentication == nil || cfg.Authentication.Scheme != "Bearer" {
		t.Errorf("authentication = %+v, want the Bearer scheme", cfg.Authentication)
	}
	if cfg.Authentication.Credentials != "" {
		t.Error("the projection returned a credential; crier holds none and must never invent one")
	}
	if cfg.Token != "" {
		t.Error("the projection returned a token; crier has no token field")
	}

	// An agent with no push channel has no configuration to describe.
	if _, rpcErr := ProjectPushConfig("agent-a", "task-7", PushRow{Tenant: "agent-a"}); rpcErr == nil ||
		rpcErr.Code != CodePushNotificationNotSupportedError {
		t.Errorf("projection for an agent with no webhook = %+v, want -32003", rpcErr)
	}

	// No bearer scheme configured: no authentication member, and neither an
	// invented one nor an empty object.
	plain := PushRow{Tenant: "agent-a", Configured: true, URL: row.URL, AuthType: "none"}
	cfg, rpcErr = ProjectPushConfig("agent-a", "task-7", plain)
	if rpcErr != nil {
		t.Fatalf("projection refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if cfg.Authentication != nil {
		t.Errorf("authentication = %+v, want it absent for an unauthenticated channel", cfg.Authentication)
	}
}

// TestMatchPushConfigID: the one way a configuration "does not exist" here, and
// the error the spec maps it to (§3.1.8, §3.1.10).
func TestMatchPushConfigID(t *testing.T) {
	row := pushRowConfigured()
	good := PushConfigID(row.Tenant, row.URL)
	if rpcErr := MatchPushConfigID(row.Tenant, good, row); rpcErr != nil {
		t.Fatalf("the derived id was rejected: %d %s", rpcErr.Code, rpcErr.Message)
	}
	rpcErr := MatchPushConfigID(row.Tenant, "wh-0000000000000000", row)
	if rpcErr == nil {
		t.Fatal("a foreign id was accepted")
	}
	if rpcErr.Code != CodeTaskNotFoundError {
		t.Errorf("code = %d, want -32001", rpcErr.Code)
	}
	if !hasErrorInfoReason(rpcErr, "PUSH_CONFIG_NOT_FOUND") {
		t.Errorf("no ErrorInfo reason: %+v", rpcErr.Data)
	}
	if !strings.Contains(rpcErr.Message, good) {
		t.Errorf("message = %q, want it to name the configuration that does exist (%s)", rpcErr.Message, good)
	}
}

// TestPushWriteRefused_Mapping: a refusal from crier's OWN registry-update route
// travels with its status and body verbatim, and with a code that says what the
// client can do about it.
func TestPushWriteRefused_Mapping(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		wantCode  int
		wantInMsg string
	}{
		{400, `{"error":"webhook.url must be http(s)://"}`, CodeInvalidParams, "webhook.url must be http(s)://"},
		{401, `{"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}`, CodeDeliveryRefused, "missing agent signature headers"},
		{403, `{"error":"AGENT_SIG_INVALID"}`, CodeDeliveryRefused, "AGENT_SIG_INVALID"},
		{404, `{"error":"agent not found"}`, CodeTaskNotFoundError, "agent not found"},
		{500, `{"error":"storage unavailable"}`, CodeInternalError, "storage unavailable"},
	}
	for _, tc := range cases {
		rpcErr := PushWriteRefused(tc.status, []byte(tc.body))
		if rpcErr.Code != tc.wantCode {
			t.Errorf("status %d → code %d, want %d", tc.status, rpcErr.Code, tc.wantCode)
		}
		if !strings.Contains(rpcErr.Message, tc.wantInMsg) {
			t.Errorf("status %d → %q, want it to carry crier's own words (%q)", tc.status, rpcErr.Message, tc.wantInMsg)
		}
		carried := false
		for _, detail := range rpcErr.Data {
			if refusal, ok := detail.(ConfigRefusal); ok && refusal.Status == tc.status &&
				string(refusal.Body) == tc.body {
				carried = true
			}
		}
		if !carried {
			t.Errorf("status %d did not carry crier's answer verbatim in a %s detail: %+v",
				tc.status, ConfigRefusalType, rpcErr.Data)
		}
	}
}

// TestPushNotificationShape_IsTheStreamResponseEnvelope pins the template the
// push configuration installs. The RENDERED body is asserted on the wire in
// cmd/server/a2apush_test.go (which can reach crier's template engine); what is
// checked here is what that rendering is built from:
//
//   - the body IS §4.3.3's StreamResponse envelope, whose oneof is exactly one
//     of task|message|statusUpdate|artifactUpdate — here `task`;
//   - every placeholder resolves against crier's own envelope metadata, and
//     every one that can be absent carries a `|default:` fallback. Without the
//     fallback crier's templating FAILS a delivery whose optional field is
//     missing (DF-CRIER-279), so a push notification with no session id would
//     be undeliverable;
//   - the media type is the one §4.3.3 fixes.
func TestPushNotificationShape_IsTheStreamResponseEnvelope(t *testing.T) {
	shape := PushNotificationShape()
	if got := shape.Headers["Content-Type"]; got != PushNotificationMediaType {
		t.Errorf("Content-Type = %q, want %q (§4.3.3)", got, PushNotificationMediaType)
	}
	if PushNotificationMediaType != "application/a2a+json" {
		t.Errorf("PushNotificationMediaType = %q, want the A2A media type registration", PushNotificationMediaType)
	}
	if shape.ResponseMap != "raw" {
		t.Errorf("ResponseMap = %q, want crier's own raw extraction", shape.ResponseMap)
	}
	if !strings.Contains(string(shape.Body), string(TaskStateSubmitted)) {
		t.Errorf("the notification does not state %s: %s", TaskStateSubmitted, shape.Body)
	}

	// The oneof: exactly one member at the envelope's own level, and it is
	// `task`. The template is valid JSON (its placeholders sit inside strings),
	// which is also what keeps the RENDERED body parseable.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(shape.Body, &envelope); err != nil {
		t.Fatalf("the notification template is not a JSON object (%v): %s", err, shape.Body)
	}
	if len(envelope) != 1 {
		t.Fatalf("the notification envelope has members %v, want exactly one of task|message|statusUpdate|artifactUpdate (§4.3.3)", keysOf(envelope))
	}
	task, ok := envelope["task"]
	if !ok {
		t.Fatalf("the notification carries %v, want task", keysOf(envelope))
	}
	var taskMembers map[string]json.RawMessage
	if err := json.Unmarshal(task, &taskMembers); err != nil {
		t.Fatalf("the task member is not an object: %v (%s)", err, task)
	}
	for _, want := range []string{"id", "contextId", "status", "metadata"} {
		if _, ok := taskMembers[want]; !ok {
			t.Errorf("the notification's task has no %q member: %s", want, task)
		}
	}

	// Placeholders: crier.* only, every optional one with a default.
	placeholders := pushPlaceholders(string(shape.Body))
	if len(placeholders) == 0 {
		t.Fatal("the template carries no placeholders; a notification with no delivery facts is not a notification")
	}
	known := map[string]bool{
		"crier.version": true, "crier.message_id": true, "crier.request_id": true,
		"crier.session_id": true, "crier.thread_id": true, "crier.delivery_mode": true,
		"crier.sender": true, "crier.target": true, "crier.kind": true,
		"crier.namespace": true,
	}
	for _, ph := range placeholders {
		path := ph
		hasDefault := false
		if i := strings.Index(ph, "|default:"); i >= 0 {
			path = ph[:i]
			hasDefault = true
		}
		if !known[path] {
			t.Errorf("placeholder {{%s}} does not resolve against crier's envelope metadata (which would fail the render, DF-CRIER-279)", ph)
			continue
		}
		if path != "crier.message_id" && !hasDefault {
			t.Errorf("placeholder {{%s}} has no |default: fallback — an absent optional field would fail the delivery", ph)
		}
	}
}

// TestPushNotificationShape_BodyIsCopied: a caller cannot mutate the package's
// template through the returned shape.
func TestPushNotificationShape_BodyIsCopied(t *testing.T) {
	first := PushNotificationShape()
	for i := range first.Body {
		first.Body[i] = 'x'
	}
	if strings.Contains(string(PushNotificationShape().Body), "xxxx") {
		t.Fatal("PushNotificationShape returned the package's own buffer")
	}
}

// --- helpers ---------------------------------------------------------------

// fieldViolationNames reports whether the refusal carries a BadRequest detail
// naming field.
func fieldViolationNames(rpcErr *RPCError, field string) bool {
	for _, detail := range rpcErr.Data {
		bad, ok := detail.(BadRequest)
		if !ok {
			continue
		}
		for _, v := range bad.FieldViolations {
			if v.Field == field {
				return true
			}
		}
	}
	return false
}

// hasErrorInfoReason reports whether the refusal carries an ErrorInfo with this
// machine-readable reason.
func hasErrorInfoReason(rpcErr *RPCError, reason string) bool {
	for _, detail := range rpcErr.Data {
		if info, ok := detail.(ErrorInfo); ok && info.Reason == reason {
			return true
		}
	}
	return false
}

var pushPlaceholderRe = regexp.MustCompile(`\{\{[^{}]+\}\}`)

// pushPlaceholders lists the placeholders of a crier schema template, without
// the braces.
func pushPlaceholders(template string) []string {
	var out []string
	for _, ph := range pushPlaceholderRe.FindAllString(template, -1) {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(ph, "{{"), "}}"))
	}
	return out
}

// keysOf lists a decoded object's member names, sorted, for a failure message.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
