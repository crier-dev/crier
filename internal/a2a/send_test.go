package a2a

// send_test.go — INT-A2A-003: the SendMessage translation and the crier-status →
// JSON-RPC-error table.
//
// What is asserted here is the CONTRACT the row names: an A2A message becomes a
// crier deliver body and nothing else, a Task is what an accepted delivery
// yields, a direct Message is what a blocking reply yields, and the delivery
// engine's own answer is reported with crier's reason intact rather than
// re-told as an A2A error it is not.

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// minimalParams is the smallest request the binding accepts, written as the JSON
// a client sends.
const minimalParams = `{
  "tenant": "agent-b",
  "message": {
    "messageId": "msg-1",
    "contextId": "ctx-1",
    "role": "ROLE_USER",
    "parts": [{"text": "hello", "metadata": {"alt": "greeting", "tags": ["hi"]}}]
  }
}`

// TestDecodeSendMessageParams_Strictness pins the honoured-or-rejected rule: a
// member this binding cannot carry is refused with the accepted set named.
func TestDecodeSendMessageParams_Strictness(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantHas []string
	}{
		{
			name:    "unknown params member",
			body:    `{"tenant":"a","mesage":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}`,
			wantHas: []string{"params", `unknown field "mesage"`},
		},
		{
			name:    "unknown message member",
			body:    `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}],"conversationId":"c"}}`,
			wantHas: []string{"message", `unknown field "conversationId"`, "contextId"},
		},
		{
			name:    "unknown configuration member",
			body:    `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]},"configuration":{"returnImmediatly":true}}`,
			wantHas: []string{"configuration", `unknown field "returnImmediatly"`, "returnImmediately"},
		},
		{
			name:    "params is not an object",
			body:    `["nope"]`,
			wantHas: []string{"params", "must be a JSON object"},
		},
		{
			name:    "message missing",
			body:    `{"tenant":"a"}`,
			wantHas: []string{"message", "is required"},
		},
		{
			name:    "parts empty",
			body:    `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[]}}`,
			wantHas: []string{"message.parts", "at least one part is required"},
		},
		{
			name:    "unknown request metadata key",
			body:    `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]},"metadata":{"deliveryMod":"async"}}`,
			wantHas: []string{"metadata.deliveryMod", "unknown request metadata key", "deliveryMode"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, rpcErr := DecodeSendMessageParams([]byte(tc.body))
			if rpcErr == nil {
				// A request-level metadata key is validated where it is
				// applied (Translate), so a case about one may pass decode.
				_, err := Translate(params, params.Metadata, "", false)
				if err == nil {
					t.Fatal("the request was accepted; want a refusal")
				}
				rpcErr = refusalOf(err)
			}
			if rpcErr.Code != CodeInvalidParams {
				t.Errorf("code = %d, want %d (%s)", rpcErr.Code, CodeInvalidParams, rpcErr.Message)
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(rpcErr.Message, want) {
					t.Errorf("message = %q, want it to contain %q", rpcErr.Message, want)
				}
			}
			// The refusal carries the spec's own invalid-params detail shape.
			if len(rpcErr.Data) != 1 {
				t.Fatalf("error data = %+v, want one google.rpc.BadRequest detail", rpcErr.Data)
			}
			detail, ok := rpcErr.Data[0].(BadRequest)
			if !ok {
				t.Fatalf("detail = %T, want BadRequest", rpcErr.Data[0])
			}
			if detail.Type != BadRequestType || len(detail.FieldViolations) != 1 {
				t.Errorf("detail = %+v, want one fieldViolation with the %s type", detail, BadRequestType)
			}
		})
	}
}

// TestTranslate_MapsTheA2AMessageOntoCriersDeliverRequest is the row's mapping
// made executable: nothing here is a new crier behaviour, every field is an
// existing member of POST /agents/{id}/inbox.
func TestTranslate_MapsTheA2AMessageOntoCriersDeliverRequest(t *testing.T) {
	params, rpcErr := DecodeSendMessageParams([]byte(`{
	  "tenant": "agent-b",
	  "message": {
	    "messageId": "msg-1",
	    "contextId": "ctx-1",
	    "role": "ROLE_USER",
	    "parts": [{"text": "hello"}]
	  },
	  "metadata": {"sender": "agent-a", "requestId": "req-7", "threadId": "thr-3", "ttlSeconds": 60, "timeoutMs": 1500}
	}`))
	if rpcErr != nil {
		t.Fatalf("decode: %d %s", rpcErr.Code, rpcErr.Message)
	}

	tr, err := Translate(params, params.Metadata, "header-agent", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	req := tr.Request
	if req.SessionID != "ctx-1" {
		t.Errorf("session_id = %q, want the A2A contextId", req.SessionID)
	}
	if req.IdempotencyKey != "msg-1" {
		t.Errorf("idempotency_key = %q, want the A2A messageId (§3.3.1 dedup)", req.IdempotencyKey)
	}
	if req.Sender != "agent-a" {
		t.Errorf("sender = %q, want the request metadata's sender (it outranks the header)", req.Sender)
	}
	if req.RequestID != "req-7" || req.ThreadID != "thr-3" {
		t.Errorf("request_id/thread_id = %q/%q, want the request metadata's", req.RequestID, req.ThreadID)
	}
	if req.TTLSeconds == nil || *req.TTLSeconds != 60 {
		t.Errorf("ttl_seconds = %v, want 60", req.TTLSeconds)
	}
	if req.TimeoutMs != 1500 {
		t.Errorf("timeout_ms = %d, want 1500", req.TimeoutMs)
	}
	if req.DeliveryMode != "" {
		t.Errorf("delivery_mode = %q, want unset — the target's own configuration decides", req.DeliveryMode)
	}
	if tr.ContextID != "ctx-1" {
		t.Errorf("context id = %q, want the message's contextId", tr.ContextID)
	}

	// The payload is a crier payload whose parts carry the message.
	var env Envelope
	if err := json.Unmarshal(req.Payload, &env); err != nil {
		t.Fatalf("payload is not a crier envelope: %v (%s)", err, req.Payload)
	}
	if len(env.Parts) != 1 || env.Parts[0].Text != "hello" || env.Parts[0].Type != PartTypeText {
		t.Errorf("payload parts = %+v, want the projected text part", env.Parts)
	}
	if env.Role != RoleUser || env.MessageID != "msg-1" {
		t.Errorf("payload role/message_id = %q/%q", env.Role, env.MessageID)
	}

	// The header is the fallback sender (crier's own convention).
	params.Metadata = nil
	tr, err = Translate(params, nil, "header-agent", false)
	if err != nil {
		t.Fatalf("translate without metadata: %v", err)
	}
	if tr.Request.Sender != "header-agent" {
		t.Errorf("sender = %q, want the X-Agent-ID header as the fallback", tr.Request.Sender)
	}
}

// TestTranslate_ReturnImmediatelyAndStreamingSelectCriersOwnAsyncOverride pins
// the documented deviation from §3.2.2's blocking default.
func TestTranslate_ReturnImmediatelyAndStreamingSelectCriersOwnAsyncOverride(t *testing.T) {
	body := `{
	  "tenant": "agent-b",
	  "message": {"messageId": "msg-1", "role": "ROLE_USER", "parts": [{"text": "hello"}]},
	  "configuration": {"returnImmediately": true}
	}`
	params, rpcErr := DecodeSendMessageParams([]byte(body))
	if rpcErr != nil {
		t.Fatalf("decode: %d %s", rpcErr.Code, rpcErr.Message)
	}
	tr, err := Translate(params, nil, "", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if tr.Request.DeliveryMode != "async" {
		t.Errorf("delivery_mode = %q, want async for returnImmediately", tr.Request.DeliveryMode)
	}

	// A stream replaces the single blocking answer, so it is queued too.
	params.Configuration = nil
	tr, err = Translate(params, nil, "", true)
	if err != nil {
		t.Fatalf("translate streaming: %v", err)
	}
	if tr.Request.DeliveryMode != "async" {
		t.Errorf("delivery_mode = %q, want async for SendStreamingMessage", tr.Request.DeliveryMode)
	}
}

// TestTranslate_RefusesWhatItCannotHonour covers the required members and the two
// surfaces later rows own.
func TestTranslate_RefusesWhatItCannotHonour(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantHas  string
	}{
		{
			name:     "messageId missing",
			body:     `{"tenant":"a","message":{"role":"ROLE_USER","parts":[{"text":"x"}]}}`,
			wantCode: CodeInvalidParams,
			wantHas:  "message.messageId",
		},
		{
			name:     "role unspecified",
			body:     `{"tenant":"a","message":{"messageId":"m","role":"ROLE_UNSPECIFIED","parts":[{"text":"x"}]}}`,
			wantCode: CodeInvalidParams,
			wantHas:  "message.role",
		},
		{
			name:     "role absent",
			body:     `{"tenant":"a","message":{"messageId":"m","parts":[{"text":"x"}]}}`,
			wantCode: CodeInvalidParams,
			wantHas:  "message.role",
		},
		{
			name:     "task continuation",
			body:     `{"tenant":"a","message":{"messageId":"m","taskId":"t-9","role":"ROLE_USER","parts":[{"text":"x"}]}}`,
			wantCode: CodeUnsupportedOperationError,
			wantHas:  "INT-A2A-004",
		},
		{
			name:     "push notification config",
			body:     `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]},"configuration":{"taskPushNotificationConfig":{"url":"https://x"}}}`,
			wantCode: CodePushNotificationNotSupportedError,
			wantHas:  "INT-A2A-005",
		},
		{
			name:     "historyLength negative",
			body:     `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]},"configuration":{"historyLength":-1}}`,
			wantCode: CodeInvalidParams,
			wantHas:  "configuration.historyLength",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, rpcErr := DecodeSendMessageParams([]byte(tc.body))
			if rpcErr != nil {
				if rpcErr.Code != tc.wantCode || !strings.Contains(rpcErr.Message, tc.wantHas) {
					t.Fatalf("decode refused with %d %q, want %d naming %q", rpcErr.Code, rpcErr.Message, tc.wantCode, tc.wantHas)
				}
				return
			}
			_, err := Translate(params, params.Metadata, "", false)
			if err == nil {
				t.Fatal("the request was accepted; want a refusal")
			}
			// Translate reports a field-level failure as an
			// *InvalidParamsError; the boundary (refusalOf) is what turns it
			// into the -32602 the client receives, so the assertion goes
			// through that same boundary rather than around it.
			rpc := refusalOf(err)
			if rpc.Code != tc.wantCode || !strings.Contains(rpc.Message, tc.wantHas) {
				t.Errorf("refusal = %d %q, want %d naming %q", rpc.Code, rpc.Message, tc.wantCode, tc.wantHas)
			}
			// The A2A-named errors carry crier's own ErrorInfo reason, so a
			// client can machine-read what actually happened.
			if len(rpc.Data) == 1 {
				if info, ok := rpc.Data[0].(ErrorInfo); ok && info.Type != ErrorInfoType {
					t.Errorf("detail type = %q, want %q", info.Type, ErrorInfoType)
				}
			}
		})
	}
}

// TestRequestMetadata_TypesAreEnforced pins the honoured-or-rejected rule per
// key: a fractional timeout, a string ttl and a null sender are all refused.
func TestRequestMetadata_TypesAreEnforced(t *testing.T) {
	cases := []struct {
		key     string
		value   string
		wantHas string
	}{
		{"timeoutMs", `1.5`, "must be an integer"},
		{"ttlSeconds", `"60"`, "must be an integer"},
		{"sender", `7`, "must be a string"},
		{"requestId", `[]`, "must be a string"},
		{"threadId", `{}`, "must be a string"},
		{"deliveryMode", `true`, "must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			body := `{"tenant":"a","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]},"metadata":{"` + tc.key + `":` + tc.value + `}}`
			params, rpcErr := DecodeSendMessageParams([]byte(body))
			if rpcErr != nil {
				if !strings.Contains(rpcErr.Message, "metadata."+tc.key) {
					t.Fatalf("decode refused with %q, want it to name metadata.%s", rpcErr.Message, tc.key)
				}
				return
			}
			_, err := Translate(params, params.Metadata, "", false)
			if err == nil {
				t.Fatalf("%s=%s was accepted; want a refusal", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), "metadata."+tc.key) || !strings.Contains(err.Error(), tc.wantHas) {
				t.Errorf("refusal = %q, want it to name metadata.%s and say %q", err, tc.key, tc.wantHas)
			}
		})
	}
}

// TestCheckVersion pins §3.6/§6.4: a client's stated version is served only when
// this server speaks it.
func TestCheckVersion(t *testing.T) {
	for _, ok := range []string{"", "1", "1.0", "1.0.0"} {
		if err := CheckVersion(ok); err != nil {
			t.Errorf("A2A-Version %q was refused: %s", ok, err.Message)
		}
	}
	for _, bad := range []string{"0.3", "0.5", "2.0"} {
		err := CheckVersion(bad)
		if err == nil {
			t.Errorf("A2A-Version %q was accepted", bad)
			continue
		}
		if err.Code != CodeVersionNotSupportedError {
			t.Errorf("A2A-Version %q → code %d, want %d", bad, err.Code, CodeVersionNotSupportedError)
		}
		if !strings.Contains(err.Message, "not supported") {
			t.Errorf("message = %q, want it to say the version is not supported", err.Message)
		}
	}
}

// TestErrorFromStatus is the crier-status → JSON-RPC-error table. Each row is a
// claim about WHICH answer a client gets and WHY, so the details are asserted
// too: the delivery engine's own body has to reach the client.
func TestErrorFromStatus(t *testing.T) {
	guardBody := `{"error":"GUARD_BLOCKED","guard":{"decision":"block","reason":"injection"}}`
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   int
		wantReason string
	}{
		{"400 → InvalidParams", http.StatusBadRequest, `{"error":"payload is required"}`, CodeInvalidParams, ""},
		{"403 guard → crier's own refusal", http.StatusForbidden, guardBody, CodeDeliveryRefused, "GUARD_BLOCKED"},
		{"403 quarantine → crier's own refusal", http.StatusForbidden, `{"error":"AGENT_QUARANTINED"}`, CodeDeliveryRefused, "AGENT_QUARANTINED"},
		{"404 → TaskNotFoundError", http.StatusNotFound, `{"error":"agent not found"}`, CodeTaskNotFoundError, "TASK_NOT_FOUND"},
		{"409 → InternalError", http.StatusConflict, `{"error":"a delivery with this idempotency_key is already in progress"}`, CodeInternalError, ""},
		{"504 → InternalError", http.StatusGatewayTimeout, `{"error":"timeout"}`, CodeInternalError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpc := ErrorFromStatus(tc.status, []byte(tc.body))
			if rpc.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", rpc.Code, tc.wantCode, rpc.Message)
			}
			// The delivery engine's own answer travels verbatim in the details.
			var refusal *DeliveryRefusal
			for _, detail := range rpc.Data {
				if r, ok := detail.(DeliveryRefusal); ok {
					copied := r
					refusal = &copied
				}
			}
			if refusal == nil {
				t.Fatalf("details = %+v, want a %s object carrying crier's answer", rpc.Data, DeliveryRefusalType)
			}
			if refusal.Status != tc.status || string(refusal.Body) != tc.body {
				t.Errorf("refusal = %+v, want crier's status %d and body %s verbatim", refusal, tc.status, tc.body)
			}
			if tc.wantReason != "" {
				found := false
				for _, detail := range rpc.Data {
					if info, ok := detail.(ErrorInfo); ok && info.Reason == tc.wantReason {
						found = true
					}
				}
				if !found {
					t.Errorf("details = %+v, want an ErrorInfo with reason %q", rpc.Data, tc.wantReason)
				}
			}
		})
	}
}

// TestTaskFromAccept pins the accept → Task projection, including the crier facts
// A2A has no field for and the history rule of §3.2.4.
func TestTaskFromAccept(t *testing.T) {
	params, rpcErr := DecodeSendMessageParams([]byte(minimalParams))
	if rpcErr != nil {
		t.Fatalf("decode: %d %s", rpcErr.Code, rpcErr.Message)
	}
	tr, err := Translate(params, nil, "", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)

	accept := DeliverAccept{
		ID:           "abc123",
		Transport:    "inbox",
		ExpiresAt:    mustJSONRaw(t, expires),
		DeliveryMode: "",
		Guard:        json.RawMessage(`{"decision":"allow"}`),
	}
	task, err := TaskFromAccept(tr, accept, now)
	if err != nil {
		t.Fatalf("TaskFromAccept: %v", err)
	}
	if task.ID != "abc123" || task.ContextID != "ctx-1" {
		t.Errorf("task id/context = %q/%q, want the delivery id and the A2A context", task.ID, task.ContextID)
	}
	if task.Status.State != TaskStateSubmitted {
		t.Errorf("state = %q, want %q (accepted, nothing has claimed it)", task.Status.State, TaskStateSubmitted)
	}
	if task.Status.Timestamp != "2026-09-25T12:00:00Z" {
		t.Errorf("timestamp = %q, want the RFC 3339 instant", task.Status.Timestamp)
	}
	if len(task.History) != 1 || task.History[0].MessageID != "msg-1" {
		t.Errorf("history = %+v, want the message that created the task", task.History)
	}
	meta, ok := task.Metadata["crier"].(CrierMeta)
	if !ok {
		t.Fatalf("metadata = %+v, want the crier facts under a namespaced key", task.Metadata)
	}
	if meta.Transport != "inbox" || string(meta.ExpiresAt) == "" {
		t.Errorf("crier metadata = %+v, want the accept's transport and expiry", meta)
	}

	// §3.2.4: historyLength 0 asks for no history.
	zero := 0
	params.Configuration = &SendMessageConfiguration{HistoryLength: &zero}
	tr, err = Translate(params, nil, "", false)
	if err != nil {
		t.Fatalf("translate with historyLength 0: %v", err)
	}
	task, err = TaskFromAccept(tr, accept, now)
	if err != nil {
		t.Fatalf("TaskFromAccept: %v", err)
	}
	if len(task.History) != 0 {
		t.Errorf("history = %+v, want none for historyLength 0", task.History)
	}

	// A held federation delivery says so, rather than reporting a bare
	// "submitted" for a message that has not been delivered anywhere.
	held, err := TaskFromAccept(tr, DeliverAccept{
		ID: "abc123", Status: "held", MaxHoldS: 300, ExpiresAt: mustJSONRaw(t, expires),
	}, now)
	if err != nil {
		t.Fatalf("TaskFromAccept(held): %v", err)
	}
	if held.Status.Message == nil || held.Status.Message.Parts[0].Text == nil ||
		!strings.Contains(*held.Status.Message.Parts[0].Text, "held") {
		t.Errorf("held task status message = %+v, want it to state the hold", held.Status.Message)
	}
	if meta, ok := held.Metadata["crier"].(CrierMeta); !ok || !meta.Held || meta.MaxHoldS != 300 {
		t.Errorf("held crier metadata = %+v, want held=true with the hold budget", held.Metadata)
	}
}

// TestMessageFromReply pins the direct-Message projection: a blocking reply
// becomes an agent-role Message whose parts come from the shared mapping.
func TestMessageFromReply(t *testing.T) {
	params, rpcErr := DecodeSendMessageParams([]byte(minimalParams))
	if rpcErr != nil {
		t.Fatalf("decode: %d %s", rpcErr.Code, rpcErr.Message)
	}
	tr, err := Translate(params, nil, "", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	// A reply that is one of this binding's envelopes yields its parts.
	reply := mustJSONRaw(t, Envelope{Parts: []MessagePart{{Type: PartTypeText, Text: "done", Alt: "the answer", Tags: []string{"ok"}}}})
	msg, err := MessageFromReply(tr, DeliverAccept{ID: "abc123", Transport: "webhook", Reply: reply}, time.Now())
	if err != nil {
		t.Fatalf("MessageFromReply: %v", err)
	}
	if msg.Role != RoleAgent {
		t.Errorf("role = %q, want %q (the reply comes from the agent side)", msg.Role, RoleAgent)
	}
	if msg.MessageID != "abc123" {
		t.Errorf("messageId = %q, want the delivery id it answers", msg.MessageID)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].Text == nil || *msg.Parts[0].Text != "done" {
		t.Fatalf("parts = %+v, want the projected reply part", msg.Parts)
	}

	// Anything else is carried verbatim as one data part.
	msg, err = MessageFromReply(tr, DeliverAccept{ID: "abc123", Transport: "webhook",
		Reply: json.RawMessage(`{"anything":1}`)}, time.Now())
	if err != nil {
		t.Fatalf("MessageFromReply(opaque): %v", err)
	}
	if len(msg.Parts) != 1 || string(msg.Parts[0].Data) != `{"anything":1}` {
		t.Errorf("parts = %+v, want the opaque reply as one data part", msg.Parts)
	}
}

// TestDecodeRequest pins the JSON-RPC envelope rules of §9.3.
func TestDecodeRequest(t *testing.T) {
	good := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"tenant":"a"}}`
	req, rpcErr := DecodeRequest([]byte(good))
	if rpcErr != nil {
		t.Fatalf("a well-formed request was refused: %d %s", rpcErr.Code, rpcErr.Message)
	}
	if req.Method != MethodSendMessage || string(req.ID) != "1" {
		t.Errorf("decoded = %+v", req)
	}

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantHas  string
	}{
		{"empty body", ``, CodeParseError, "empty"},
		{"not JSON", `{"jsonrpc":`, CodeParseError, "Invalid JSON"},
		{"trailing data", `{"jsonrpc":"2.0","id":1,"method":"SendMessage"}{"x":1}`, CodeParseError, "trailing"},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"SendMessage"}]`, CodeInvalidRequest, "batch"},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"SendMessage"}`, CodeInvalidRequest, `jsonrpc must be "2.0"`},
		{"no method", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest, "method is required"},
		{"no id (a notification)", `{"jsonrpc":"2.0","method":"SendMessage"}`, CodeInvalidRequest, "id is required"},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"SendMessage"}`, CodeInvalidRequest, "id is required"},
		{"object id", `{"jsonrpc":"2.0","id":{},"method":"SendMessage"}`, CodeInvalidRequest, "must be a string or a number"},
		{"unknown envelope member", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","param":{}}`, CodeInvalidRequest, `unknown field "param"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr := DecodeRequest([]byte(tc.body))
			if rpcErr == nil {
				t.Fatal("the request was accepted; want a refusal")
			}
			if rpcErr.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", rpcErr.Code, tc.wantCode, rpcErr.Message)
			}
			if !strings.Contains(rpcErr.Message, tc.wantHas) {
				t.Errorf("message = %q, want it to contain %q", rpcErr.Message, tc.wantHas)
			}
		})
	}

	// A string id is a legal id, and the response echoes the id it was given.
	req, rpcErr = DecodeRequest([]byte(`{"jsonrpc":"2.0","id":"abc","method":"SendMessage"}`))
	if rpcErr != nil {
		t.Fatalf("a string id was refused: %s", rpcErr.Message)
	}
	resp := SuccessResponse(req.ID, map[string]any{"ok": true})
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	if !strings.Contains(string(raw), `"id":"abc"`) || !strings.Contains(string(raw), `"jsonrpc":"2.0"`) {
		t.Errorf("response = %s, want the id and version echoed", raw)
	}
}

// TestResponseEnvelopes enforces the JSON-RPC rule that exactly one of result and
// error is present.
func TestResponseEnvelopes(t *testing.T) {
	success := marshalMap(t, SuccessResponse(NullID, map[string]any{"task": map[string]any{"id": "t"}}))
	if _, hasError := success["error"]; hasError {
		t.Errorf("success response carries an error: %v", success)
	}
	if success["jsonrpc"] != "2.0" || success["id"] != nil {
		t.Errorf("success envelope = %v", success)
	}

	failure := marshalMap(t, ErrorResponse(NullID, NewRPCError(CodeInvalidParams, "nope")))
	if _, hasResult := failure["result"]; hasResult {
		t.Errorf("error response carries a result: %v", failure)
	}
	errObj, ok := failure["error"].(map[string]any)
	if !ok {
		t.Fatalf("error member = %T, want an object", failure["error"])
	}
	if errObj["code"] != float64(CodeInvalidParams) || errObj["message"] != "nope" {
		t.Errorf("error object = %v", errObj)
	}
}

// TestStreamResponseOneofs pins the four stream payloads to their wire names.
func TestStreamResponseOneofs(t *testing.T) {
	task := &Task{ID: "t1", Status: TaskStatus{State: TaskStateSubmitted}}
	for _, tc := range []struct {
		name    string
		resp    StreamResponse
		wantKey string
	}{
		{"task", StreamTask(task), "task"},
		{"message", StreamMessage(&Message{MessageID: "m"}), "message"},
		{"statusUpdate", StreamStatus(&TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1"}), "statusUpdate"},
		{"artifactUpdate", StreamArtifact(&TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1"}), "artifactUpdate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalMap(t, tc.resp)
			if len(got) != 1 {
				t.Fatalf("stream response = %v, want exactly one member", got)
			}
			if _, ok := got[tc.wantKey]; !ok {
				t.Errorf("stream response = %v, want the %q member", got, tc.wantKey)
			}
		})
	}
}

// TestTaskStateTerminality pins which states end a task (§3.1.2).
func TestTaskStateTerminality(t *testing.T) {
	for _, terminal := range []TaskState{TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected} {
		if !terminal.IsTerminal() {
			t.Errorf("%s is not reported as terminal", terminal)
		}
	}
	for _, live := range []TaskState{TaskStateSubmitted, TaskStateWorking, TaskStateUnspecified} {
		if live.IsTerminal() {
			t.Errorf("%s is reported as terminal", live)
		}
	}
}

// mustJSONRaw marshals a value or fails the test. (Named apart from card_test.go's
// mustJSON, which renders an AgentCard.)
func mustJSONRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// marshalMap renders a value as the generic decoded JSON object a comparison
// wants.
func marshalMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out
}

// errorsAs is errors.As for *RPCError, kept local so this file's call sites stay
// readable.
func errorsAs(err error, target **RPCError) bool {
	rpc, ok := err.(*RPCError)
	if ok {
		*target = rpc
	}
	return ok
}

// TestEnvelopeIsAnObjectForCriersDeliverPath is the one wire property the
// delivery path enforces on a payload: it must be a JSON object
// (registry.validateDeliverPayload). A projection that produced anything else
// would be refused by deliver with a message about crier's own key rather than
// about the A2A request.
func TestEnvelopeIsAnObjectForCriersDeliverPath(t *testing.T) {
	params, rpcErr := DecodeSendMessageParams([]byte(minimalParams))
	if rpcErr != nil {
		t.Fatalf("decode: %d %s", rpcErr.Code, rpcErr.Message)
	}
	tr, err := Translate(params, nil, "", false)
	if rpcErr == nil && err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := strings.TrimSpace(string(tr.Request.Payload)); !strings.HasPrefix(got, "{") {
		t.Errorf("payload = %s, want a JSON object", got)
	}
	if !reflect.DeepEqual(marshalMap(t, tr.Request), map[string]any{
		"idempotency_key": "msg-1",
		"payload":         jsonAny(t, tr.Request.Payload),
		"session_id":      "ctx-1",
	}) {
		t.Errorf("deliver body = %s, want exactly the payload, the session and the dedup key", mustJSONString(t, tr.Request))
	}
}

// jsonAny decodes raw JSON into the generic shape.
func jsonAny(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return v
}

// mustJSONString renders a value as a string for an error message.
func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		return "<unencodable>"
	}
	return string(raw)
}
