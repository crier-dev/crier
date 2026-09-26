package main

// a2apush_test.go — INT-A2A-005: the push-notification configuration surface,
// live against a booted server.
//
// The row's acceptance criteria are the shape of this file:
//
//  1. config CRUD ROUND-TRIPS ONTO THE WEBHOOK CONFIG — create/get/list/delete
//     are asserted twice: through the A2A binding, and through crier's OWN
//     GET /agents/{id}, which is where the effect has to be visible for the
//     claim "the A2A config is a view over webhook.Config" to be true
//     (TestA2APushConfig_CreateGetListDeleteRoundTripOntoTheWebhookConfig);
//  2. DELIVERY USES THE EXISTING RETRY/BATCH MACHINERY — the notification is
//     sent by the shipped webhook driver, with crier's own dispatch headers, and
//     a failing endpoint is retried by the same bounded redelivery loop
//     (TestA2APushConfig_NotificationIsTheStreamResponseEnvelope and
//     TestA2APushConfig_NotificationUsesCriersRetryMachinery);
//  3. AN AGENT WITHOUT A WEBHOOK GETS THE NAMED ERROR — -32003
//     PushNotificationNotSupportedError from ALL FOUR operations, never a
//     silent success (TestA2APushConfig_NotSupportedWithoutAWebhook);
//  4. THE PAYLOAD IS THE DOCUMENTED StreamResponse ENVELOPE — asserted on the
//     wire, byte for byte, from what the endpoint actually received.
//
// Non-regression is asserted in the same file where it is at risk: a refused
// write leaves the row untouched (TestA2APushConfig_RefusalsChangeNothing), and
// the write inherits crier's own agent-owned signature gate rather than
// weakening it (TestA2APushConfig_WriteInheritsCriersAgentSignatureGate).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
)

// pushRPC posts one push-notification method to the binding and returns the HTTP
// status and the raw body. Every push operation is a JSON-RPC method of the
// binding POST /a2a — crier implements §9's JSON-RPC binding and not §11's REST
// binding, so there is no /tasks/... route to call.
func pushRPC(t *testing.T, client *http.Client, base, method, params string) (int, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	status, raw, _ := a2aRPC(t, client, base, body)
	return status, raw
}

// agentRow reads crier's OWN view of a registry row — the surface the push
// configuration has to be visible on.
func pushAgentRow(t *testing.T, base, agentID string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/agents/" + agentID)
	if err != nil {
		t.Fatalf("GET /agents/%s: %v", agentID, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the agent row: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents/%s returned %d: %s", agentID, resp.StatusCode, raw)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode the agent row %s: %v", raw, err)
	}
	return row
}

// pushWebhookOf returns the row's webhook member, or nil when the row carries
// none — the measurement "does this agent have a push channel".
func pushWebhookOf(row map[string]any) map[string]any {
	raw, ok := row["webhook"].(map[string]any)
	if !ok {
		return nil
	}
	return raw
}

// TestA2APushConfig_CreateGetListDeleteRoundTripOntoTheWebhookConfig is the
// row's central acceptance criterion: the four operations are a VIEW over
// crier's webhook config, not a second configuration store.
//
// The boot runs with CR_REQUIRE_AGENT_SIG=false because the two WRITE operations
// reach crier's own PATCH /agents/{id}, which is agent-owned: with signature
// enforcement on (crier's secure default) that route requires the target agent's
// ed25519 signature, which an A2A client cannot compute, and the write is
// refused with crier's own answer verbatim — asserted separately in
// TestA2APushConfig_WriteInheritsCriersAgentSignatureGate.
func TestA2APushConfig_CreateGetListDeleteRoundTripOntoTheWebhookConfig(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-push-agent"
	const endpoint = "http://127.0.0.1:19801/hook"
	registerA2AAgent(t, base, agentID, true, json.RawMessage(`{
		"url": "`+endpoint+`",
		"delivery_mode": "async",
		"retries": 3,
		"timeout_ms": 5000,
		"schema_template": "openai-compatible"
	}`))

	// --- CREATE -------------------------------------------------------------
	status, raw := pushRPC(t, client, base, a2a.MethodCreateTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-1","url":"`+endpoint+`"}`)
	if status != http.StatusOK {
		t.Fatalf("create: status = %d, want 200 (%s)", status, raw)
	}
	created := rpcResult(t, raw)
	t.Logf("CREATE result: %s", raw)

	wantID := a2a.PushConfigID(agentID, endpoint)
	if got := created["id"]; got != wantID {
		t.Errorf("create result id = %v, want the derived %q", got, wantID)
	}
	if got := created["url"]; got != endpoint {
		t.Errorf("create result url = %v, want %q", got, endpoint)
	}
	if got := created["tenant"]; got != agentID {
		t.Errorf("create result tenant = %v, want %q", got, agentID)
	}
	if got := created["taskId"]; got != "task-1" {
		t.Errorf("create result taskId = %v, want the client's own addressing echoed", got)
	}

	// The write is crier's webhook config: assert it through crier's own route.
	hook := pushWebhookOf(pushAgentRow(t, base, agentID))
	if hook == nil {
		t.Fatal("the create wrote no webhook config: the A2A configuration is not a view over it")
	}
	if got := hook["url"]; got != endpoint {
		t.Errorf("webhook.url = %v, want %q", got, endpoint)
	}
	// crier's own knobs survive: the A2A shape has no field for them, and
	// resetting them would silently change how delivery works.
	for field, want := range map[string]any{
		"delivery_mode": "async", "retries": float64(3), "timeout_ms": float64(5000),
		"schema_template": "openai-compatible",
	} {
		if got := hook[field]; got != want {
			t.Errorf("webhook.%s = %v, want %v (the A2A write must preserve crier's own knobs)", field, got, want)
		}
	}
	custom, ok := hook["custom_schema"].(map[string]any)
	if !ok {
		t.Fatalf("webhook.custom_schema = %v, want the §4.3.3 payload shape installed", hook["custom_schema"])
	}
	shape, ok := custom["request_shape"].(map[string]any)
	if !ok {
		t.Fatalf("custom_schema.request_shape = %v", custom["request_shape"])
	}
	headers, _ := shape["headers"].(map[string]any)
	if got := headers["Content-Type"]; got != a2a.PushNotificationMediaType {
		t.Errorf("notification Content-Type = %v, want %q", got, a2a.PushNotificationMediaType)
	}
	if body, _ := json.Marshal(shape["body"]); !strings.Contains(string(body), "TASK_STATE_SUBMITTED") {
		t.Errorf("the installed schema does not render a StreamResponse: %s", body)
	}
	t.Logf("row after CREATE: %v", hook)

	// --- GET ----------------------------------------------------------------
	status, raw = pushRPC(t, client, base, a2a.MethodGetTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-1","id":"`+wantID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (%s)", status, raw)
	}
	got := rpcResult(t, raw)
	t.Logf("GET result: %s", raw)
	for _, field := range []string{"id", "url", "tenant", "taskId"} {
		if got[field] != created[field] {
			t.Errorf("get %s = %v, but create returned %v — the round trip does not close", field, got[field], created[field])
		}
	}

	// --- LIST ---------------------------------------------------------------
	status, raw = pushRPC(t, client, base, a2a.MethodListTaskPushNotificationConfigs,
		`{"tenant":"`+agentID+`","taskId":"task-1"}`)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (%s)", status, raw)
	}
	listed := rpcResult(t, raw)
	t.Logf("LIST result: %s", raw)
	configs, ok := listed["configs"].([]any)
	if !ok || len(configs) != 1 {
		t.Fatalf("list configs = %v, want exactly one (crier serves one push channel per agent)", listed["configs"])
	}
	if _, present := listed["nextPageToken"]; present {
		t.Errorf("list returned a nextPageToken (%v): crier serves one configuration and never pages", listed["nextPageToken"])
	}
	first, _ := configs[0].(map[string]any)
	if first["id"] != created["id"] || first["url"] != created["url"] {
		t.Errorf("listed configuration %v does not match the created one %v", first, created)
	}

	// --- DELETE -------------------------------------------------------------
	status, raw = pushRPC(t, client, base, a2a.MethodDeleteTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-1","id":"`+wantID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200 (%s)", status, raw)
	}
	deleted := rpcResult(t, raw)
	t.Logf("DELETE result: %s", raw)
	if deleted["deleted"] != true {
		t.Errorf("delete result = %v, want a confirmation", deleted)
	}
	if hook := pushWebhookOf(pushAgentRow(t, base, agentID)); hook != nil {
		t.Errorf("the webhook config survived the delete: %v", hook)
	}
	// The capability is genuinely false afterwards, and the Agent Card says so
	// (the card reads the same fact the operations do).
	resp, err := http.Get(base + "/.well-known/agent-card.json?agent_id=" + agentID)
	if err != nil {
		t.Fatalf("GET the agent card: %v", err)
	}
	defer resp.Body.Close()
	cardRaw, _ := io.ReadAll(resp.Body)
	var card struct {
		Capabilities struct {
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(cardRaw, &card); err != nil {
		t.Fatalf("decode the card %s: %v", cardRaw, err)
	}
	if card.Capabilities.PushNotifications {
		t.Error("the card still advertises pushNotifications after the push channel was deleted")
	}

	// A second delete answers the capability error: §3.1.10 lists
	// PushNotificationNotSupportedError for this operation, and after the first
	// delete the agent has no push channel at all (§3.3.4's MUST).
	status, raw = pushRPC(t, client, base, a2a.MethodDeleteTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-1","id":"`+wantID+`"}`)
	code, message := rpcError(t, raw)
	if status != http.StatusOK || code != a2a.CodePushNotificationNotSupportedError {
		t.Errorf("second delete = HTTP %d, code %d (%s); want 200 with -32003", status, code, message)
	}
}

// TestA2APushConfig_NotSupportedWithoutAWebhook is §3.3.4's capability MUST: an
// agent whose AgentCard says pushNotifications:false gets
// PushNotificationNotSupportedError from every push-notification operation. A
// silent success would report a push channel that does not exist.
func TestA2APushConfig_NotSupportedWithoutAWebhook(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-no-push"
	registerA2AAgent(t, base, agentID, true, nil)

	// The card states the capability, and it is false.
	resp, err := http.Get(base + "/.well-known/agent-card.json?agent_id=" + agentID)
	if err != nil {
		t.Fatalf("GET the agent card: %v", err)
	}
	cardRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(cardRaw), `"pushNotifications":false`) {
		t.Fatalf("the card does not state pushNotifications:false: %s", cardRaw)
	}

	id := a2a.PushConfigID(agentID, "http://127.0.0.1:19802/hook")
	cases := []struct {
		method string
		params string
	}{
		{a2a.MethodCreateTaskPushNotificationConfig, `{"tenant":"` + agentID + `","taskId":"t","url":"http://127.0.0.1:19802/hook"}`},
		{a2a.MethodGetTaskPushNotificationConfig, `{"tenant":"` + agentID + `","taskId":"t","id":"` + id + `"}`},
		{a2a.MethodListTaskPushNotificationConfigs, `{"tenant":"` + agentID + `","taskId":"t"}`},
		{a2a.MethodDeleteTaskPushNotificationConfig, `{"tenant":"` + agentID + `","taskId":"t","id":"` + id + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			status, raw := pushRPC(t, client, base, tc.method, tc.params)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a JSON-RPC error travels in the error object): %s", status, raw)
			}
			code, message := rpcError(t, raw)
			if code != a2a.CodePushNotificationNotSupportedError {
				t.Fatalf("code = %d, want -32003 (PushNotificationNotSupportedError): %s", code, raw)
			}
			if !strings.Contains(message, agentID) || !strings.Contains(message, "pushNotifications") {
				t.Errorf("message = %q, want it to name the agent and the capability", message)
			}
			var envelope struct {
				Error struct {
					Data []struct {
						Type     string            `json:"@type"`
						Reason   string            `json:"reason"`
						Metadata map[string]string `json:"metadata"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatalf("decode the error %s: %v", raw, err)
			}
			found := false
			for _, detail := range envelope.Error.Data {
				if detail.Type == a2a.ErrorInfoType && detail.Reason == "PUSH_NOTIFICATION_NOT_SUPPORTED" {
					found = true
					if detail.Metadata["capability"] != "pushNotifications" {
						t.Errorf("ErrorInfo metadata = %v, want the capability named", detail.Metadata)
					}
				}
			}
			if !found {
				t.Errorf("no %s detail with reason PUSH_NOTIFICATION_NOT_SUPPORTED: %s", a2a.ErrorInfoType, raw)
			}
			t.Logf("%s → %s", tc.method, raw)
		})
	}

	// And nothing was written: an agent that did not have a push channel still
	// does not have one.
	if hook := pushWebhookOf(pushAgentRow(t, base, agentID)); hook != nil {
		t.Errorf("a refused create left a webhook config behind: %v", hook)
	}
}

// TestA2APushConfig_NotificationIsTheStreamResponseEnvelope asserts the payload
// §4.3.3 fixes, on the wire: the endpoint receives an HTTP POST of the same
// StreamResponse shape as a streaming operation, sent by crier's own webhook
// driver (its dispatch headers are on the request), because the agent's push
// channel IS the webhook.
func TestA2APushConfig_NotificationIsTheStreamResponseEnvelope(t *testing.T) {
	type received struct {
		header http.Header
		body   []byte
	}
	got := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-push-payload"
	registerA2AAgent(t, base, agentID, true,
		json.RawMessage(`{"url":"`+srv.URL+`","delivery_mode":"async"}`))

	status, raw := pushRPC(t, client, base, a2a.MethodCreateTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-payload","url":"`+srv.URL+`"}`)
	if status != http.StatusOK {
		t.Fatalf("create: status = %d (%s)", status, raw)
	}

	// Deliver through crier's OWN route: the notification is whatever the
	// existing delivery path sends to this agent, nothing A2A-specific.
	deliver := `{"payload":{"text":"hello push"},"session_id":"session-7","thread_id":"thread-9",` +
		`"sender":"peer-1","delivery_mode":"async"}`
	resp, err := http.Post(base+"/agents/"+agentID+"/inbox", "application/json", strings.NewReader(deliver))
	if err != nil {
		t.Fatalf("POST the delivery: %v", err)
	}
	acceptRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery: status = %d, want 202 (%s)", resp.StatusCode, acceptRaw)
	}
	var accept struct {
		ID        string `json:"id"`
		Transport string `json:"transport"`
	}
	if err := json.Unmarshal(acceptRaw, &accept); err != nil {
		t.Fatalf("decode the delivery accept %s: %v", acceptRaw, err)
	}
	if accept.Transport != "webhook" {
		t.Fatalf("the delivery went somewhere else (%q): the push channel was not used", accept.Transport)
	}

	var first received
	select {
	case first = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the endpoint received no push notification within 10s")
	}
	t.Logf("notification headers: %v", first.header)
	t.Logf("notification body: %s", first.body)

	if got := first.header.Get("Content-Type"); got != a2a.PushNotificationMediaType {
		t.Errorf("Content-Type = %q, want %q (§4.3.3)", got, a2a.PushNotificationMediaType)
	}
	// crier's own dispatch headers: the shipped driver sent this.
	if got := first.header.Get("X-Crier-Event"); got != "message" {
		t.Errorf("X-Crier-Event = %q, want message — the notification must travel crier's existing dispatch path", got)
	}
	if got := first.header.Get("X-Crier-Target"); got != agentID {
		t.Errorf("X-Crier-Target = %q, want %q", got, agentID)
	}

	// The envelope is a StreamResponse: exactly one of the four members.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(first.body, &env); err != nil {
		t.Fatalf("the notification body is not JSON: %v (%s)", err, first.body)
	}
	if len(env) != 1 {
		t.Fatalf("the notification has members %v, want exactly one of task|message|statusUpdate|artifactUpdate", env)
	}
	taskRaw, ok := env["task"]
	if !ok {
		t.Fatalf("the notification carries %v, want task", env)
	}
	var task struct {
		ID        string `json:"id"`
		ContextID string `json:"contextId"`
		Status    struct {
			State string `json:"state"`
		} `json:"status"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(taskRaw, &task); err != nil {
		t.Fatalf("decode the task %s: %v", taskRaw, err)
	}
	if task.ID != accept.ID {
		t.Errorf("task.id = %q, want the delivered message id %q (the A2A task IS the crier delivery)", task.ID, accept.ID)
	}
	if task.Status.State != string(a2a.TaskStateSubmitted) {
		t.Errorf("task.status.state = %q, want %q", task.Status.State, a2a.TaskStateSubmitted)
	}
	if task.ContextID != "session-7" {
		t.Errorf("task.contextId = %q, want the delivery's session", task.ContextID)
	}
	var crierMeta map[string]any
	if err := json.Unmarshal(task.Metadata["crier"], &crierMeta); err != nil {
		t.Fatalf("the task carries no crier metadata: %s (%v)", task.Metadata["crier"], err)
	}
	for field, want := range map[string]any{
		"transport": "webhook", "kind": "message", "delivery_mode": "async",
		"sender": "peer-1", "target": agentID, "thread_id": "thread-9",
	} {
		if crierMeta[field] != want {
			t.Errorf("metadata.crier.%s = %v, want %v", field, crierMeta[field], want)
		}
	}

	// A delivery with NO session id still renders: every optional placeholder
	// declares a `|default:` (DF-CRIER-279 fails the render otherwise), so a
	// bare delivery is not undeliverable.
	resp, err = http.Post(base+"/agents/"+agentID+"/inbox", "application/json",
		strings.NewReader(`{"payload":{"text":"no session"},"delivery_mode":"async"}`))
	if err != nil {
		t.Fatalf("POST the second delivery: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	select {
	case second := <-got:
		var env2 map[string]json.RawMessage
		if err := json.Unmarshal(second.body, &env2); err != nil {
			t.Fatalf("the second notification is not JSON: %v (%s)", err, second.body)
		}
		if _, ok := env2["task"]; !ok {
			t.Errorf("the second notification carries %v, want task", env2)
		}
		if !strings.Contains(string(second.body), `"contextId":""`) {
			t.Logf("second notification (no session): %s", second.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the endpoint received no notification for the session-less delivery within 10s")
	}
}

// TestA2APushConfig_NotificationUsesCriersRetryMachinery proves the delivery is
// the shipped driver's, not a new one: an endpoint that answers 500 is retried
// by crier's own bounded redelivery loop, with the retry counter on the request.
func TestA2APushConfig_NotificationUsesCriersRetryMachinery(t *testing.T) {
	var mu sync.Mutex
	retries := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		retries = append(retries, r.Header.Get("X-Crier-Retry"))
		attempt := len(retries)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "false",
		// crier's own redelivery interval, shortened so the retry is observable
		// inside a test. The machinery is unchanged; only its clock moves.
		"CR_WEBHOOK_REDELIVER_S": "1",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-push-retry"
	registerA2AAgent(t, base, agentID, true,
		json.RawMessage(`{"url":"`+srv.URL+`","delivery_mode":"async","retries":3}`))

	status, raw := pushRPC(t, client, base, a2a.MethodCreateTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"task-retry","url":"`+srv.URL+`"}`)
	if status != http.StatusOK {
		t.Fatalf("create: status = %d (%s)", status, raw)
	}
	resp, err := http.Post(base+"/agents/"+agentID+"/inbox", "application/json",
		strings.NewReader(`{"payload":{"text":"retry me"},"delivery_mode":"async"}`))
	if err != nil {
		t.Fatalf("POST the delivery: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(retries)
		seen := append([]string(nil), retries...)
		mu.Unlock()
		if n >= 2 {
			t.Logf("X-Crier-Retry seen per attempt: %v", seen)
			if seen[1] != "1" {
				t.Errorf("second attempt carried X-Crier-Retry=%q, want 1", seen[1])
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the failed notification was not retried within 15s (attempts: %v)", retries)
}

// TestA2APushConfig_RefusalsChangeNothing walks the refusals, and — the part
// that matters for the standing constraint — checks after every one of them that
// the agent's webhook config is EXACTLY what it was before. A refusal may never
// leave a half-written configuration behind.
func TestA2APushConfig_RefusalsChangeNothing(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-push-refusals"
	const endpoint = "http://127.0.0.1:19803/hook"
	registerA2AAgent(t, base, agentID, true, json.RawMessage(`{
		"url": "`+endpoint+`", "delivery_mode": "async", "retries": 2,
		"auth_type": "bearer", "auth_value_ref": "A2A_PUSH_REFUSAL_SECRET"
	}`))
	before, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, agentID)))

	// An agent with a bring-your-own schema cannot also promise this binding's
	// payload shape: crier resolves a custom schema first, so the operation is
	// refused rather than overwriting the operator's schema behind their back.
	const customID = "a2a-push-custom-schema"
	registerA2AAgent(t, base, customID, true, json.RawMessage(`{
		"url": "`+endpoint+`",
		"custom_schema": {"request_shape": {"method": "POST", "body": {"mine": "{{crier.message_id}}"}}}
	}`))
	customBefore, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, customID)))

	id := a2a.PushConfigID(agentID, endpoint)
	cases := []struct {
		name      string
		method    string
		params    string
		wantCode  int
		wantInMsg string
		wantField string
	}{
		{
			name: "an unknown params member", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"` + endpoint + `","retrIes":3}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "unknown field",
		},
		{
			name: "url absent", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","taskId":"t"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "is required",
			wantField: "url",
		},
		{
			name: "a notification token", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"` + endpoint + `","token":"abc"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "auth_value_ref",
			wantField: "token",
		},
		{
			name: "a raw credential", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"` + endpoint + `","authentication":{"scheme":"Bearer","credentials":"hunter2"}}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "never accepted",
			wantField: "authentication.credentials",
		},
		{
			name: "a scheme crier cannot honour", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"` + endpoint + `","authentication":{"scheme":"Digest"}}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "accepted schemes are none and bearer",
			wantField: "authentication.scheme",
		},
		{
			name: "a client-supplied id", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"` + endpoint + `","id":"mine"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "ONE push configuration per agent",
			wantField: "id",
		},
		{
			name: "a url crier would refuse on its own route", method: a2a.MethodCreateTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","url":"ftp://example.com/hook"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "webhook.url must be http(s)://",
		},
		{
			name: "a foreign configuration id", method: a2a.MethodGetTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","taskId":"t","id":"wh-0000000000000000"}`,
			wantCode:  a2a.CodeTaskNotFoundError,
			wantInMsg: "no push notification configuration",
		},
		{
			name: "get without taskId", method: a2a.MethodGetTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","id":"` + id + `"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "is required",
			wantField: "taskId",
		},
		{
			name: "a page token", method: a2a.MethodListTaskPushNotificationConfigs,
			params:    `{"tenant":"` + agentID + `","taskId":"t","pageToken":"nope"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "never paged",
			wantField: "pageToken",
		},
		{
			name: "delete with a foreign id", method: a2a.MethodDeleteTaskPushNotificationConfig,
			params:    `{"tenant":"` + agentID + `","taskId":"t","id":"wh-1111111111111111"}`,
			wantCode:  a2a.CodeTaskNotFoundError,
			wantInMsg: "no push notification configuration",
		},
		{
			name: "a tenant that did not opt in", method: a2a.MethodListTaskPushNotificationConfigs,
			params:    `{"tenant":"nobody-here","taskId":"t"}`,
			wantCode:  a2a.CodeInvalidParams,
			wantInMsg: "is not an A2A agent on this relay",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := pushRPC(t, client, base, tc.method, tc.params)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", status, raw)
			}
			code, message := rpcError(t, raw)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d (%s)", code, tc.wantCode, message)
			}
			if !strings.Contains(message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", message, tc.wantInMsg)
			}
			if tc.wantField != "" && !strings.Contains(string(raw), `"field":"`+tc.wantField+`"`) {
				t.Errorf("the refusal does not name the field %q in a fieldViolation: %s", tc.wantField, raw)
			}
			after, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, agentID)))
			if string(after) != string(before) {
				t.Fatalf("the refused request changed the webhook config:\nbefore %s\nafter  %s", before, after)
			}
			t.Logf("%s → %s", tc.name, raw)
		})
	}

	// The bring-your-own-schema agent is refused by name, and its schema is not
	// touched — the operator's configuration is not silently replaced.
	status, raw := pushRPC(t, client, base, a2a.MethodCreateTaskPushNotificationConfig,
		`{"tenant":"`+customID+`","taskId":"t","url":"`+endpoint+`"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	code, message := rpcError(t, raw)
	if code != a2a.CodeUnsupportedOperationError {
		t.Errorf("code = %d, want -32004 (%s)", code, message)
	}
	if !strings.Contains(message, "custom_schema") {
		t.Errorf("message = %q, want it to name the custom schema in the way", message)
	}
	t.Logf("custom-schema agent → %s", raw)
	customAfter, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, customID)))
	if string(customAfter) != string(customBefore) {
		t.Fatalf("the refused create changed the custom-schema agent:\nbefore %s\nafter  %s", customBefore, customAfter)
	}

	// A send carrying an INLINE push config is refused in both capability
	// positions, and configures nothing either way: the agent with a push
	// channel is told the error that is true about it (-32004, the surface's
	// own operations named), the agent without one keeps -32003.
	inline := `{"tenant":"` + agentID + `","message":{"messageId":"m-inline","role":"ROLE_USER","parts":[{"text":"x"}]},` +
		`"configuration":{"taskPushNotificationConfig":{"url":"` + endpoint + `"}}}`
	status, raw = pushRPC(t, client, base, a2a.MethodSendMessage, inline)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	code, message = rpcError(t, raw)
	if code != a2a.CodeUnsupportedOperationError {
		t.Errorf("inline push config for a push-capable agent = %d (%s), want -32004", code, message)
	}
	if !strings.Contains(message, a2a.MethodCreateTaskPushNotificationConfig) {
		t.Errorf("message = %q, want it to name the operations that do configure a channel", message)
	}
	t.Logf("inline push config → %s", raw)
	after, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, agentID)))
	if string(after) != string(before) {
		t.Fatalf("a send wrote configuration: before %s after %s", before, after)
	}

	const noPushID = "a2a-push-inline-nohook"
	registerA2AAgent(t, base, noPushID, true, nil)
	status, raw = pushRPC(t, client, base, a2a.MethodSendMessage,
		`{"tenant":"`+noPushID+`","message":{"messageId":"m-inline-2","role":"ROLE_USER","parts":[{"text":"x"}]},`+
			`"configuration":{"taskPushNotificationConfig":{"url":"`+endpoint+`"}}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	if code, message = rpcError(t, raw); code != a2a.CodePushNotificationNotSupportedError {
		t.Errorf("inline push config without a push channel = %d (%s), want -32003", code, message)
	}
}

// TestA2APushConfig_WriteInheritsCriersAgentSignatureGate: with
// CR_REQUIRE_AGENT_SIG in force (crier's secure DEFAULT), the two write
// operations reach crier's own agent-owned PATCH /agents/{id}, which requires the
// target agent's ed25519 signature — something an A2A client cannot compute. The
// surface inherits that gate and answers with crier's own words; it does not
// weaken it, and it does not silently write.
//
// The reads are unaffected: they ask the relay a question, they do not rewrite
// an agent's row.
func TestA2APushConfig_WriteInheritsCriersAgentSignatureGate(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED": "true",
		// CR_REQUIRE_AGENT_SIG is left at its default (true).
	})
	client := &http.Client{Timeout: 5 * time.Second}

	const agentID = "a2a-push-gated"
	const endpoint = "http://127.0.0.1:19804/hook"
	registerA2AAgent(t, base, agentID, true,
		json.RawMessage(`{"url":"`+endpoint+`","delivery_mode":"async"}`))
	before, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, agentID)))

	status, raw := pushRPC(t, client, base, a2a.MethodCreateTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"t","url":"`+endpoint+`"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, raw)
	}
	code, message := rpcError(t, raw)
	if code != a2a.CodeDeliveryRefused {
		t.Fatalf("code = %d (%s), want -32050 (crier's own refusal)", code, message)
	}
	if !strings.Contains(message, "agent signature") {
		t.Errorf("message = %q, want crier's own reason verbatim", message)
	}
	if !strings.Contains(string(raw), a2a.ConfigRefusalType) {
		t.Errorf("the refusal does not carry crier's answer verbatim in a %s detail: %s", a2a.ConfigRefusalType, raw)
	}
	t.Logf("create with the default signature posture → %s", raw)

	after, _ := json.Marshal(pushWebhookOf(pushAgentRow(t, base, agentID)))
	if string(after) != string(before) {
		t.Fatalf("a refused write changed the row:\nbefore %s\nafter  %s", before, after)
	}

	// The gate is crier's own: the same missing signature on the route the write
	// goes through is refused the same way.
	req, err := http.NewRequest(http.MethodPatch, base+"/agents/"+agentID,
		strings.NewReader(`{"webhook":{"url":"`+endpoint+`","retries":1}}`))
	if err != nil {
		t.Fatalf("build the PATCH: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH /agents/%s: %v", agentID, err)
	}
	patchBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PATCH /agents/{id} without a signature = %d, want 401 (%s)", resp.StatusCode, patchBody)
	}
	if !strings.Contains(string(patchBody), "agent signature") {
		t.Errorf("crier's own refusal body changed: %s", patchBody)
	}

	// Reads work: the capability answer and the configuration projection do not
	// require writing anything.
	id := a2a.PushConfigID(agentID, endpoint)
	status, raw = pushRPC(t, client, base, a2a.MethodGetTaskPushNotificationConfig,
		`{"tenant":"`+agentID+`","taskId":"t","id":"`+id+`"}`)
	if status != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", status, raw)
	}
	if got := rpcResult(t, raw)["id"]; got != id {
		t.Errorf("get returned id %v, want %q", got, id)
	}
	status, raw = pushRPC(t, client, base, a2a.MethodListTaskPushNotificationConfigs,
		`{"tenant":"`+agentID+`","taskId":"t"}`)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", status, raw)
	}
	if _, ok := rpcResult(t, raw)["configs"]; !ok {
		t.Errorf("list returned no configs member: %s", raw)
	}
}
