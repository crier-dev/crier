package main

// a2a_jsonrpc_test.go — INT-A2A-003: the A2A JSON-RPC binding, live.
//
// The row's acceptance criteria are the shape of this file:
//
//  1. an A2A client completes send → task → stream against a RUNNING crier
//     (TestA2AJSONRPCSend_... and TestA2AJSONRPCStreaming_..., both against the
//     real server booted by run(nil), middleware included);
//  2. a round-trip test proves a multi-part A2A message survives to a crier
//     CONSUMER with alt/tags intact — the consumer being the agent's own
//     GET /agents/{id}/inbox read, signed as that agent;
//  3. the binding REUSES crier's delivery path rather than adding a second
//     engine: the tests below prove it by observing crier's own delivery
//     behaviour through A2A — an inbox entry that retrieves and acks normally,
//     crier's idempotency replay, crier's sender/lease lifecycle and the
//     delivery's own 404/400/403 answers surfacing as JSON-RPC errors;
//  4. the relay's WebSocket path is unchanged with the option ON (the existing
//     relay tests are untouched; TestA2AJSONRPC_RelayWebSocketIsUnchangedWith
//     TheOptionOn additionally exercises it on an A2A-enabled boot).
//
// The wire transcripts are asserted frame by frame, not summarised.

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/crier-dev/crier/internal/a2a"
)

// a2aMediaType is what this binding answers with, asserted literally rather than
// through the constant so a constant that drifts from the spec fails here.
const a2aMediaType = "application/a2a+json"

// a2aRPC posts a JSON-RPC request body to the binding and returns the status,
// the raw body and the response headers.
func a2aRPC(t *testing.T, client *http.Client, baseURL, body string) (int, []byte, http.Header) {
	t.Helper()
	return a2aRPCWithHeaders(t, client, baseURL, body, nil)
}

// a2aRPCWithHeaders is a2aRPC with caller-supplied request headers (the A2A
// service parameters travel there, §9.2).
func a2aRPCWithHeaders(t *testing.T, client *http.Client, baseURL, body string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+a2a.JSONRPCBindingPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the JSON-RPC request: %v", err)
	}
	req.Header.Set("Content-Type", a2aMediaType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", a2a.JSONRPCBindingPath, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	return resp.StatusCode, raw, resp.Header
}

// rpcResult decodes a JSON-RPC response and fails the test on an error object,
// returning the `result` member.
func rpcResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var resp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  map[string]any `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    []any  `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode the JSON-RPC response %s: %v", raw, err)
	}
	if resp.Error != nil {
		t.Fatalf("the request was refused: %d %s (%s)", resp.Error.Code, resp.Error.Message, raw)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	return resp.Result
}

// rpcError decodes a JSON-RPC ERROR response and fails the test if the answer was
// a success.
func rpcError(t *testing.T, raw []byte) (int, string) {
	t.Helper()
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode the JSON-RPC response %s: %v", raw, err)
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error, got %s", raw)
	}
	return resp.Error.Code, resp.Error.Message
}

// publishToRelay posts an event to crier's relay publish route. X-Agent-ID is
// set because the relay's rate limiter requires it — crier's own convention,
// which the binding's sender mapping also reads.
func publishToRelay(base, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/relay/publish", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "a2a-test-publisher")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, nil
}

// registerA2AAgent registers an agent with a real ed25519 key and returns the
// private half so the test can sign as that identity. optIn sets the `a2a` block
// (the per-agent half of the gate); webhook, when non-empty, is a raw
// configuration for a push-delivery target.
func registerA2AAgent(t *testing.T, base, agentID string, optIn bool, webhook json.RawMessage) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body := map[string]any{
		"id":           agentID,
		"public_key":   hex.EncodeToString(pub),
		"capabilities": []string{"consumer"},
	}
	if optIn {
		body["a2a"] = map[string]any{"enabled": true}
	}
	if len(webhook) > 0 {
		body["webhook"] = json.RawMessage(webhook)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	resp, err := http.Post(base+"/agents", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /agents returned %d: %s", resp.StatusCode, out)
	}
	return priv
}

// signedAs issues a request signed by the agent's own key (the per-agent
// signature crier requires on agent-scoped reads): the payload is
// "<METHOD>\n<path>\n<unix-seconds>" (internal/registry/agentsig.go).
func signedAs(t *testing.T, client *http.Client, method, base, path, agentID string, priv ed25519.PrivateKey, body []byte) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := method + "\n" + path + "\n" + ts
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Agent-Ts", ts)
	req.Header.Set("X-Agent-Sig", hex.EncodeToString(ed25519.Sign(priv, []byte(payload))))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// sendMessageBody is the JSON-RPC request the row's transcript is about: a
// 2-part A2A message whose first part carries alt/tags/caption in its metadata.
func sendMessageBody(tenant, method, messageID string) string {
	return fmt.Sprintf(`{
  "jsonrpc": "2.0",
  "id": "req-1",
  "method": %q,
  "params": {
    "tenant": %q,
    "message": {
      "messageId": %q,
      "contextId": "ctx-deploy",
      "role": "ROLE_USER",
      "parts": [
        {
          "text": "deploy the canary",
          "mediaType": "text/plain",
          "metadata": {"alt": "operator instruction", "tags": ["deploy", "canary"], "caption": "step 1"}
        },
        {"data": {"replicas": 2, "region": "eu-central"}, "metadata": {"alt": "rollout parameters"}}
      ]
    }
  }
}`, method, tenant, messageID)
}

// TestA2AJSONRPC_RouteExistsOnlyWhenTheOptionIsOn is the separability half: with
// CR_A2A_ENABLED unset the path answers the router's own 404 (exactly what it
// answered before this row), and with it set the path exists.
func TestA2AJSONRPC_RouteExistsOnlyWhenTheOptionIsOn(t *testing.T) {
	t.Run("switch unset (default off)", func(t *testing.T) {
		base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": ""})
		client := &http.Client{Timeout: 5 * time.Second}
		status, body, _ := a2aRPC(t, client, base, sendMessageBody("agent-b", a2a.MethodSendMessage, "msg-1"))
		if status != http.StatusNotFound {
			t.Fatalf("POST %s = %d with the option off, want 404: %s", a2a.JSONRPCBindingPath, status, body)
		}
	})

	t.Run("switch on", func(t *testing.T) {
		base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
		client := &http.Client{Timeout: 5 * time.Second}
		status, body, header := a2aRPC(t, client, base, `{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{}}`)
		if status != http.StatusOK {
			t.Fatalf("POST %s = %d with the option on, want 200: %s", a2a.JSONRPCBindingPath, status, body)
		}
		if got := header.Get("Content-Type"); got != a2aMediaType {
			t.Errorf("Content-Type = %q, want %q", got, a2aMediaType)
		}
		code, message := rpcError(t, body)
		if code != a2a.CodeMethodNotFound {
			t.Errorf("code = %d, want %d (%s)", code, a2a.CodeMethodNotFound, message)
		}
	})
}

// TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact is the row's evidence
// test: send → task, then the SAME delivery read by the agent through crier's own
// inbox, with the multi-part message and its alt/tags intact.
func TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	const agentID = "a2a-consumer"
	priv := registerA2AAgent(t, base, agentID, true, nil)

	status, body, _ := a2aRPCWithHeaders(t, client, base,
		sendMessageBody(agentID, a2a.MethodSendMessage, "msg-8f14e45f"),
		map[string]string{"X-Agent-ID": "a2a-client"})
	if status != http.StatusOK {
		t.Fatalf("SendMessage = %d, want 200 (a JSON-RPC response): %s", status, body)
	}
	result := rpcResult(t, body)
	task, ok := result["task"].(map[string]any)
	if !ok {
		t.Fatalf("result = %s, want a task (the durable inbox transport is asynchronous)", body)
	}

	// The Task's own contract.
	taskID, _ := task["id"].(string)
	if len(taskID) != 24 {
		t.Errorf("task.id = %q, want crier's message id (12 random bytes, hex)", taskID)
	}
	if got, _ := task["contextId"].(string); got != "ctx-deploy" {
		t.Errorf("task.contextId = %q, want the message's contextId", got)
	}
	statusObj, _ := task["status"].(map[string]any)
	if got, _ := statusObj["state"].(string); got != string(a2a.TaskStateSubmitted) {
		t.Errorf("task.status.state = %q, want %q", got, a2a.TaskStateSubmitted)
	}
	if _, ok := statusObj["timestamp"].(string); !ok {
		t.Errorf("task.status = %v, want an RFC 3339 timestamp", statusObj)
	}
	meta, _ := task["metadata"].(map[string]any)
	crierMeta, _ := meta["crier"].(map[string]any)
	if got, _ := crierMeta["transport"].(string); got != "inbox" {
		t.Errorf("task.metadata.crier = %v, want transport=inbox (crier's own accept facts)", crierMeta)
	}
	history, _ := task["history"].([]any)
	if len(history) != 1 {
		t.Fatalf("task.history = %v, want the message that created the task", history)
	}
	sent, _ := history[0].(map[string]any)
	if got, _ := sent["messageId"].(string); got != "msg-8f14e45f" || got == "" {
		t.Errorf("history[0].messageId = %q, want the sent message id", got)
	}

	// ▼ THE CONSUMER'S VIEW — crier's own retrieve, signed as the agent.
	code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+agentID+"/inbox", agentID, priv, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /agents/%s/inbox = %d: %s", agentID, code, raw)
	}
	var retrieved struct {
		Messages []struct {
			ID string `json:"id"`
			// Payload travels base64-encoded on this route: that is crier's
			// own, pre-existing retrieve contract (`Payload []byte` JSON
			// encoding, documented in docs/openapi.yaml — "decode each
			// entry's payload before use"), and this row changes nothing
			// about it.
			Payload     []byte `json:"payload"`
			Sender      string `json:"sender"`
			Idempotency string `json:"idempotency_key"`
		} `json:"messages"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(raw, &retrieved); err != nil {
		t.Fatalf("decode the retrieve body %s: %v", raw, err)
	}
	if len(retrieved.Messages) != 1 {
		t.Fatalf("retrieved %d messages, want 1", len(retrieved.Messages))
	}
	msg := retrieved.Messages[0]
	if msg.ID != taskID {
		t.Errorf("inbox entry id = %q, want the task id %q (the entry IS the task)", msg.ID, taskID)
	}
	if msg.Sender != "a2a-client" {
		t.Errorf("inbox entry sender = %q, want the X-Agent-ID the A2A client sent", msg.Sender)
	}
	if msg.Idempotency != "msg-8f14e45f" {
		t.Errorf("inbox entry idempotency_key = %q, want the A2A messageId", msg.Idempotency)
	}

	var payload struct {
		MessageID string `json:"message_id"`
		ContextID string `json:"context_id"`
		Role      string `json:"role"`
		Parts     []struct {
			Type      string   `json:"type"`
			Text      string   `json:"text"`
			Data      any      `json:"data"`
			Alt       string   `json:"alt"`
			Tags      []string `json:"tags"`
			Caption   string   `json:"caption"`
			MediaType string   `json:"media_type"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("decode the entry payload %s: %v", msg.Payload, err)
	}
	if payload.MessageID != "msg-8f14e45f" || payload.ContextID != "ctx-deploy" || payload.Role != "ROLE_USER" {
		t.Errorf("payload envelope = %+v, want the A2A message's ids and role", payload)
	}
	if len(payload.Parts) != 2 {
		t.Fatalf("payload parts = %d, want 2 (the A2A message had 2)", len(payload.Parts))
	}
	if got := payload.Parts[0]; got.Type != "text" || got.Text != "deploy the canary" {
		t.Errorf("part 0 = %+v, want the text part", got)
	}
	if got := payload.Parts[0]; got.Alt != "operator instruction" || strings.Join(got.Tags, ",") != "deploy,canary" || got.Caption != "step 1" {
		t.Errorf("part 0 alt/tags/caption = %q/%v/%q, want them intact", got.Alt, got.Tags, got.Caption)
	}
	if got := payload.Parts[1]; got.Type != "data" || got.Alt != "rollout parameters" {
		t.Errorf("part 1 = %+v, want the data part with its alt", got)
	}
	if replicas, ok := payload.Parts[1].Data.(map[string]any)["replicas"].(float64); !ok || replicas != 2 {
		t.Errorf("part 1 data = %v, want the opaque payload preserved", payload.Parts[1].Data)
	}

	// THE DELIVERY IS CRIER'S OWN — acknowledged exactly as any other inbox
	// message is, which is what proves the binding reused the delivery path
	// rather than parking a message somewhere of its own.
	ackBody, err := json.Marshal(map[string]any{"lease_id": retrieved.LeaseID, "message_ids": []string{taskID}})
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+agentID+"/inbox/ack", agentID, priv, ackBody)
	if code != http.StatusNoContent {
		t.Fatalf("POST .../inbox/ack = %d: %s", code, raw)
	}

	// crier's IDEMPOTENCY applies to an A2A send because the binding maps the
	// A2A messageId onto it (§3.3.1): the same message id twice is one
	// delivery — and, before the ack above, one entry.
	status, body, _ = a2aRPC(t, client, base, sendMessageBody(agentID, a2a.MethodSendMessage, "msg-8f14e45f"))
	if status != http.StatusOK {
		t.Fatalf("the repeated SendMessage = %d: %s", status, body)
	}
	if got, _ := rpcResult(t, body)["task"].(map[string]any)["id"].(string); got != taskID {
		t.Errorf("the repeated message id produced task %q, want the first delivery's task %q (crier's dedup)", got, taskID)
	}
}

// TestA2AJSONRPC_GatesAndRefusals pins every refusal, and — because a refusal
// that still delivered would be the worst possible bug — that none of them
// reaches the agent's inbox.
func TestA2AJSONRPC_GatesAndRefusals(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	const (
		optedIn    = "a2a-opted-in"
		notOptedIn = "a2a-not-opted-in"
	)
	privOptedIn := registerA2AAgent(t, base, optedIn, true, nil)
	_ = registerA2AAgent(t, base, notOptedIn, false, nil)

	cases := []struct {
		name       string
		body       string
		headers    map[string]string
		wantCode   int
		wantInMsg  string
		wantBadFld string
	}{
		{
			name:       "tenant names no A2A agent",
			body:       sendMessageBody("nobody-here", a2a.MethodSendMessage, "m-1"),
			wantCode:   a2a.CodeInvalidParams,
			wantInMsg:  "is not an A2A agent on this relay",
			wantBadFld: "params.tenant",
		},
		{
			name:       "the agent did not opt in",
			body:       sendMessageBody(notOptedIn, a2a.MethodSendMessage, "m-2"),
			wantCode:   a2a.CodeInvalidParams,
			wantInMsg:  "has not opted in",
			wantBadFld: "params.tenant",
		},
		{
			name:       "tenant missing",
			body:       `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}}`,
			wantCode:   a2a.CodeInvalidParams,
			wantInMsg:  "params.tenant is required",
			wantBadFld: "params.tenant",
		},
		{
			// A method no row of this series has landed yet: the push-
			// notification configs are INT-A2A-005, so they are the methods
			// that still answer MethodNotFound. (The task lifecycle answered
			// this until INT-A2A-004 landed it — those methods are exercised
			// for real in a2a_lifecycle_test.go.)
			name:      "unknown method",
			body:      `{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{}}`,
			wantCode:  a2a.CodeMethodNotFound,
			wantInMsg: "Method not found",
		},
		{
			name:      "malformed JSON",
			body:      `{"jsonrpc":"2.0","id":1,`,
			wantCode:  a2a.CodeParseError,
			wantInMsg: "Invalid JSON",
		},
		{
			name:      "a batch",
			body:      `[{"jsonrpc":"2.0","id":1,"method":"SendMessage"}]`,
			wantCode:  a2a.CodeInvalidRequest,
			wantInMsg: "batch",
		},
		{
			name:      "no id",
			body:      `{"jsonrpc":"2.0","method":"SendMessage","params":{}}`,
			wantCode:  a2a.CodeInvalidRequest,
			wantInMsg: "id is required",
		},
		{
			name:      "A2A-Version the server does not serve",
			body:      sendMessageBody(optedIn, a2a.MethodSendMessage, "m-3"),
			headers:   map[string]string{"A2A-Version": "0.5"},
			wantCode:  a2a.CodeVersionNotSupportedError,
			wantInMsg: "A2A-Version 0.5 is not supported",
		},
		{
			name:      "a push-notification config (INT-A2A-005)",
			body:      `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"tenant":"` + optedIn + `","message":{"messageId":"m-4","role":"ROLE_USER","parts":[{"text":"x"}]},"configuration":{"taskPushNotificationConfig":{"url":"https://example.com/hook"}}}}`,
			wantCode:  a2a.CodePushNotificationNotSupportedError,
			wantInMsg: "INT-A2A-005",
		},
		{
			// INT-A2A-004 resolves a task id against crier's OWN store before
			// anything is delivered (§5.5.3), and this id names no task this
			// agent ever had. The answer is the specification's
			// TaskNotFoundError (§3.4.2: "Agents MUST return a TaskNotFoundError
			// if the provided taskId does not correspond to an existing task")
			// and not a delivery: nothing here is accepted. The other two arms
			// of the same rule — a TERMINAL task is UnsupportedOperationError,
			// an OPEN one is UnsupportedOperationError because crier cannot
			// represent a continuation — are covered live in
			// a2a_lifecycle_test.go against a real inbox entry.
			name:      "a message naming a task that does not exist",
			body:      `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"tenant":"` + optedIn + `","message":{"messageId":"m-5","taskId":"t-1","role":"ROLE_USER","parts":[{"text":"x"}]}}}`,
			wantCode:  a2a.CodeTaskNotFoundError,
			wantInMsg: "no failure record",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := a2aRPCWithHeaders(t, client, base, tc.body, tc.headers)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 — a JSON-RPC error travels in the error object, not in an HTTP status: %s", status, body)
			}
			code, message := rpcError(t, body)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", code, tc.wantCode, message)
			}
			if !strings.Contains(message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", message, tc.wantInMsg)
			}
			if tc.wantBadFld != "" {
				var decoded struct {
					Error struct {
						Data []struct {
							Type            string `json:"@type"`
							FieldViolations []struct {
								Field string `json:"field"`
							} `json:"fieldViolations"`
						} `json:"data"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatalf("decode the error details %s: %v", body, err)
				}
				if len(decoded.Error.Data) != 1 || decoded.Error.Data[0].Type != a2a.BadRequestType {
					t.Fatalf("error data = %+v, want one %s detail", decoded.Error.Data, a2a.BadRequestType)
				}
				if got := decoded.Error.Data[0].FieldViolations[0].Field; got != tc.wantBadFld {
					t.Errorf("fieldViolation.field = %q, want %q", got, tc.wantBadFld)
				}
			}
		})
	}

	// NO REFUSAL DELIVERED ANYTHING: the opted-in agent's inbox is empty.
	code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+optedIn+"/inbox", optedIn, privOptedIn, nil)
	if code != http.StatusOK {
		t.Fatalf("GET inbox = %d: %s", code, raw)
	}
	var retrieved struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &retrieved); err != nil {
		t.Fatalf("decode the retrieve body: %v", err)
	}
	if len(retrieved.Messages) != 0 {
		t.Errorf("inbox holds %d message(s) after refusals only, want 0: %s", len(retrieved.Messages), raw)
	}
}

// TestA2AJSONRPC_ContentTypeIsEnforced pins the one HTTP-level rule of this
// binding: a body labelled as something else is refused before it is read.
func TestA2AJSONRPC_ContentTypeIsEnforced(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, base+a2a.JSONRPCBindingPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}

	// The two types the binding reads are accepted, and application/json is one
	// of them because §9.1 is what a plain A2A client sends.
	for _, ct := range []string{a2aMediaType, "application/json", "application/json; charset=utf-8"} {
		req, err := http.NewRequest(http.MethodPost, base+a2a.JSONRPCBindingPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{}}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", ct)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST with %s: %v", ct, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Content-Type %q → %d, want 200: %s", ct, resp.StatusCode, body)
		}
	}
}

// sseStream reads SSE frames off a streaming response.
type sseStream struct {
	events chan map[string]any
	closed chan struct{}
	status int
	ctype  string
	body   *http.Response
}

// openSSE starts a stream and returns a reader that yields the `result` of each
// JSON-RPC frame as it arrives (§9.4.2: every frame is a JSON-RPC response).
func openSSE(t *testing.T, base, body string) *sseStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+a2a.JSONRPCBindingPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the streaming request: %v", err)
	}
	req.Header.Set("Content-Type", a2aMediaType)
	client := &http.Client{} // no timeout: a stream is meant to stay open
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s (stream): %v", a2a.JSONRPCBindingPath, err)
	}
	s := &sseStream{
		events: make(chan map[string]any, 32),
		closed: make(chan struct{}),
		status: resp.StatusCode,
		ctype:  resp.Header.Get("Content-Type"),
		body:   resp,
	}
	go func() {
		defer close(s.closed)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var resp struct {
				JSONRPC string          `json:"jsonrpc"`
				Result  map[string]any  `json:"result"`
				Error   json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &resp); err != nil {
				continue
			}
			if len(resp.Error) > 0 {
				s.events <- map[string]any{"__error": string(resp.Error)}
				continue
			}
			s.events <- resp.Result
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return s
}

// next returns the next frame's `result`, failing the test if none arrives.
func (s *sseStream) next(t *testing.T, what string) map[string]any {
	t.Helper()
	select {
	case event := <-s.events:
		if errRaw, ok := event["__error"]; ok {
			t.Fatalf("the stream carried a JSON-RPC error while waiting for %s: %v", what, errRaw)
		}
		return event
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s on the stream", what)
		return nil
	}
}

// close waits for the stream to end (the server closing the response body).
func (s *sseStream) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-s.closed:
	case <-time.After(15 * time.Second):
		t.Fatal("the stream did not close")
	}
}

// TestA2AJSONRPCStreaming_CompletesTheTaskLifecycleAndCarriesRelayArtifacts is
// the "send → task → stream" acceptance: one SendStreamingMessage call opens a
// text/event-stream that carries the Task, the inbox lifecycle crier really goes
// through (submitted → working → completed), and any artifact published to the
// task's relay topic.
func TestA2AJSONRPCStreaming_CompletesTheTaskLifecycleAndCarriesRelayArtifacts(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	const agentID = "a2a-streamer"
	priv := registerA2AAgent(t, base, agentID, true, nil)

	stream := openSSE(t, base, sendMessageBody(agentID, a2a.MethodSendStreamingMessage, "msg-stream-1"))
	if stream.status != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", stream.status)
	}
	if !strings.HasPrefix(stream.ctype, "text/event-stream") {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", stream.ctype)
	}

	// Frame 1: the Task, before anything else (§3.1.2 pattern 2).
	first := stream.next(t, "the opening Task")
	task, ok := first["task"].(map[string]any)
	if !ok {
		t.Fatalf("first frame = %v, want a task", first)
	}
	taskID, _ := task["id"].(string)
	if taskID == "" {
		t.Fatalf("task = %v, want an id", task)
	}
	if got := task["status"].(map[string]any)["state"]; got != string(a2a.TaskStateSubmitted) {
		t.Errorf("first frame state = %v, want %q", got, a2a.TaskStateSubmitted)
	}
	if got := task["contextId"]; got != "ctx-deploy" {
		t.Errorf("task.contextId = %v, want the message's context", got)
	}

	// An artifact published to the task's relay topic reaches the stream — the
	// SSE adapter over crier's relay. The publish is the ordinary relay route;
	// nothing A2A-specific exists on the publish side.
	topic, ok := a2a.TaskTopic(taskID)
	if !ok {
		t.Fatalf("TaskTopic(%q) reported no topic", taskID)
	}
	gotArtifact := false
	artifactFrame := map[string]any{}
	for attempt := 0; attempt < 4 && !gotArtifact; attempt++ {
		pubBody := fmt.Sprintf(`{"topic":%q,"event":{"note":"canary is up","replicas":2}}`, topic)
		resp, err := publishToRelay(base, pubBody)
		if err != nil {
			t.Fatalf("POST /relay/publish: %v", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /relay/publish = %d, want 202", resp.StatusCode)
		}
		select {
		case event := <-stream.events:
			if update, ok := event["artifactUpdate"].(map[string]any); ok {
				gotArtifact = true
				artifactFrame = update
			} else {
				// A lifecycle frame may arrive first; keep it aside and
				// continue reading.
				stream.events <- event
				time.Sleep(150 * time.Millisecond)
			}
		case <-time.After(2 * time.Second):
		}
	}
	if !gotArtifact {
		t.Fatal("no artifactUpdate frame arrived: the relay event did not reach the stream")
	}
	artifact, _ := artifactFrame["artifact"].(map[string]any)
	if artifactFrame["taskId"] != taskID || artifactFrame["contextId"] != "ctx-deploy" {
		t.Errorf("artifactUpdate = %v, want the task id and context", artifactFrame)
	}
	if artifact == nil {
		t.Fatalf("artifactUpdate = %v, want an artifact", artifactFrame)
	}
	parts, _ := artifact["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("artifact parts = %v, want the relay event as one data part", parts)
	}
	dataPart, _ := parts[0].(map[string]any)
	if _, ok := dataPart["data"]; !ok {
		t.Errorf("artifact part = %v, want the event carried as a data part", dataPart)
	}
	meta, _ := artifact["metadata"].(map[string]any)
	if crierMeta, _ := meta["crier"].(map[string]any); crierMeta["relay_topic"] != topic {
		t.Errorf("artifact metadata = %v, want the relay topic recorded", meta)
	}

	// The consumer retrieves (leasing the message) → the stream reports WORKING.
	code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+agentID+"/inbox", agentID, priv, nil)
	if code != http.StatusOK {
		t.Fatalf("GET inbox = %d: %s", code, raw)
	}
	var retrieved struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(raw, &retrieved); err != nil {
		t.Fatalf("decode the retrieve body: %v", err)
	}
	if len(retrieved.Messages) != 1 || retrieved.Messages[0].ID != taskID {
		t.Fatalf("retrieved %+v, want the streamed task %q", retrieved.Messages, taskID)
	}
	working := stream.nextStatus(t, string(a2a.TaskStateWorking))

	// The consumer acks (removing the entry) → the stream reports COMPLETED and
	// closes, which is what §3.1.2 requires of a terminal state.
	ackBody, _ := json.Marshal(map[string]any{"lease_id": retrieved.LeaseID, "message_ids": []string{taskID}})
	code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+agentID+"/inbox/ack", agentID, priv, ackBody)
	if code != http.StatusNoContent {
		t.Fatalf("POST ack = %d: %s", code, raw)
	}
	completed := stream.nextStatus(t, string(a2a.TaskStateCompleted))
	if completed["taskId"] != taskID {
		t.Errorf("completed frame = %v, want the task id", completed)
	}
	stream.waitClosed(t)

	// The frames carry the state transitions, and the WORKING one is the lease
	// crier's own retrieve produced — no state was invented along the way.
	workingStatus, _ := working["status"].(map[string]any)
	if got := workingStatus["state"]; got != string(a2a.TaskStateWorking) {
		t.Errorf("working frame state = %v, want %q", got, a2a.TaskStateWorking)
	}
	completedStatus, _ := completed["status"].(map[string]any)
	if got := completedStatus["state"]; got != string(a2a.TaskStateCompleted) {
		t.Errorf("completed frame state = %v, want %q", got, a2a.TaskStateCompleted)
	}
}

// nextStatus reads frames until a statusUpdate with the wanted state arrives,
// so an interleaved artifact frame cannot make the assertion order-dependent.
func (s *sseStream) nextStatus(t *testing.T, state string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event := <-s.events:
			update, ok := event["statusUpdate"].(map[string]any)
			if !ok {
				continue
			}
			if got, _ := update["status"].(map[string]any)["state"].(string); got == state {
				return update
			}
		case <-time.After(time.Until(deadline)):
		}
	}
	t.Fatalf("no statusUpdate with state %s arrived", state)
	return nil
}

// TestA2AJSONRPCStreaming_RefusesBeforeItStreams pins that a request which cannot
// be delivered is answered as a JSON-RPC error rather than an empty stream: the
// stream's content type is a promise about the body, and a refusal is not one.
func TestA2AJSONRPCStreaming_RefusesBeforeItStreams(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	registerA2AAgent(t, base, "a2a-streamer-2", true, nil)

	status, body, header := a2aRPC(t, client, base, sendMessageBody("nobody-here", a2a.MethodSendStreamingMessage, "m-1"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := header.Get("Content-Type"); got != a2aMediaType {
		t.Errorf("Content-Type = %q, want %q (an error is not a stream)", got, a2aMediaType)
	}
	if code, _ := rpcError(t, body); code != a2a.CodeInvalidParams {
		t.Errorf("code = %d, want %d", code, a2a.CodeInvalidParams)
	}
}

// TestA2AJSONRPC_RelayWebSocketIsUnchangedWithTheOptionOn is the explicit
// non-regression statement about the relay's own protocol: with the A2A option
// ON, a WebSocket subscriber still receives exactly the frame
// POST /relay/publish fans out (the SSE adapter subscribes through the same
// Relay.Subscribe; it does not alter what a WebSocket subscriber sees).
func TestA2AJSONRPC_RelayWebSocketIsUnchangedWithTheOptionOn(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/relay/subscribe/a2a.regression.topic"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial the relay socket: %v", err)
	}
	defer conn.Close()

	// Subscribing is not instantaneous with respect to a publish on another
	// connection, so the publish is retried until the frame arrives — the
	// relay is a live fan-out bus with no replay, exactly as it was before this
	// row.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := publishToRelay(base, `{"topic":"a2a.regression.topic","event":{"hello":"world"}}`)
		if err != nil {
			t.Fatalf("POST /relay/publish: %v", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST /relay/publish = %d, want 202", resp.StatusCode)
		}
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, frame, err := conn.ReadMessage()
		if err == nil {
			want := `{"topic":"a2a.regression.topic","event":{"hello":"world"}}`
			if string(frame) != want {
				t.Errorf("relay frame = %s, want %s", frame, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no relay frame arrived on the WebSocket subscriber: %v", err)
		}
	}
}
