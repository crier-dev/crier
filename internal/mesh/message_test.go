package mesh

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func BenchmarkMarshal(b *testing.B) {
	reg := &Register{
		Envelope: Envelope{
			Type:      TypeRegister,
			Version:   1,
			MessageID: "bench-message-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		AgentID:    "bench-agent",
		LeaseID:    "bench-lease",
		LeaseTTLMs: 3600000,
		Capabilities: Capabilities{
			Version:               "0.1.0",
			Topics:                []string{"metrics", "logs", "events"},
			MaxConcurrentSessions: 10,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(reg); err != nil {
			b.Fatalf("Marshal: %v", err)
		}
	}
}

func TestRegisterRoundTrip(t *testing.T) {
	reg := &Register{
		Envelope: Envelope{
			Type:      TypeRegister,
			Version:   1,
			MessageID: "msg-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		AgentID:    "agent-A",
		LeaseID:    "",
		LeaseTTLMs: 3600000,
		Capabilities: Capabilities{
			Version:               "0.1.0",
			Topics:                []string{"metrics", "logs"},
			MaxConcurrentSessions: 10,
		},
	}
	data, err := Marshal(reg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-A"`) {
		t.Errorf("expected agent_id in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"topics":["metrics","logs"]`) {
		t.Errorf("expected topics in JSON, got %s", data)
	}
	if strings.Contains(string(data), "controller_id") {
		t.Errorf("controller_id should not appear, got %s", data)
	}
	if strings.Contains(string(data), "workspaces") {
		t.Errorf("workspaces should not appear, got %s", data)
	}

	// Trim trailing newline added by Marshal before unmarshaling.
	data = bytes.TrimRight(data, "\n")
	var got Register
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.AgentID != reg.AgentID {
		t.Errorf("AgentID = %q, want %q", got.AgentID, reg.AgentID)
	}
	if got.LeaseTTLMs != reg.LeaseTTLMs {
		t.Errorf("LeaseTTLMs = %d, want %d", got.LeaseTTLMs, reg.LeaseTTLMs)
	}
	if len(got.Capabilities.Topics) != 2 {
		t.Errorf("Topics len = %d, want 2", len(got.Capabilities.Topics))
	}
}

func TestRequestRoundTrip(t *testing.T) {
	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: "req-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Source:    PeerRef{AgentID: "agent-A"},
		Target:    PeerRef{AgentID: "agent-B"},
		Method:    "GET",
		Path:      "/v1/state",
		Body:      nil,
		TraceID:   "trace-001",
		TimeoutMs: 30000,
	}
	data, err := Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-A"`) {
		t.Errorf("expected source agent_id in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-B"`) {
		t.Errorf("expected target agent_id in JSON, got %s", data)
	}

	data = bytes.TrimRight(data, "\n")
	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Source.AgentID != "agent-A" {
		t.Errorf("Source.AgentID = %q, want agent-A", got.Source.AgentID)
	}
	if got.Target.AgentID != "agent-B" {
		t.Errorf("Target.AgentID = %q, want agent-B", got.Target.AgentID)
	}
	if got.Method != "GET" {
		t.Errorf("Method = %q, want GET", got.Method)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	body := json.RawMessage(`{"ok":true}`)
	resp := &Response{
		Envelope: Envelope{
			Type:      TypeResponse,
			Version:   1,
			MessageID: "resp-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		RequestID:  "req-001",
		Source:     PeerRef{AgentID: "agent-B"},
		StatusCode: 200,
		Body:       body,
		TraceID:    "trace-001",
	}
	data, err := Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-B"`) {
		t.Errorf("expected agent_id in JSON, got %s", data)
	}

	data = bytes.TrimRight(data, "\n")
	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	if string(got.Body) != string(body) {
		t.Errorf("Body = %s, want %s", got.Body, body)
	}
}

func TestErrorMessageRoundTrip(t *testing.T) {
	errMsg := &ErrorMessage{
		Envelope: Envelope{
			Type:      TypeError,
			Version:   1,
			MessageID: "err-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		RequestID: "req-001",
		Error: ErrorDetail{
			Code:         ErrCodeRateLimited,
			Message:      "slow down",
			RetryAfterMs: 1000,
		},
		TraceID: "trace-001",
	}
	data, err := Marshal(errMsg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"code":"RATE_LIMITED"`) {
		t.Errorf("expected RATE_LIMITED code, got %s", data)
	}

	data = bytes.TrimRight(data, "\n")
	var got ErrorMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Error.Code != ErrCodeRateLimited {
		t.Errorf("Code = %q, want %q", got.Error.Code, ErrCodeRateLimited)
	}
	if got.Error.RetryAfterMs != 1000 {
		t.Errorf("RetryAfterMs = %d, want 1000", got.Error.RetryAfterMs)
	}
}

func TestKeepaliveRoundTrip(t *testing.T) {
	ka := &Keepalive{
		Envelope: Envelope{
			Type:      TypeKeepalive,
			Version:   1,
			MessageID: "ka-001",
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		LeaseID: "",
		AgentID: "agent-A",
	}
	data, err := Marshal(ka)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-A"`) {
		t.Errorf("expected agent_id in JSON, got %s", data)
	}
	// Keepalive must not carry Status field anymore.
	if strings.Contains(string(data), `"status"`) {
		t.Errorf("keepalive should not carry status, got %s", data)
	}

	data = bytes.TrimRight(data, "\n")
	var got Keepalive
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.AgentID != "agent-A" {
		t.Errorf("AgentID = %q, want agent-A", got.AgentID)
	}
}

func TestMarshalAddsNewline(t *testing.T) {
	data, err := Marshal(&Request{
		Envelope: Envelope{Type: TypeRequest, Version: 1, MessageID: "x"},
		Method:   "GET",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if data[len(data)-1] != '\n' {
		t.Errorf("Marshal must append newline; got trailing %q", data[len(data)-1])
	}
}

func TestNewMessageIDUnique(t *testing.T) {
	a := newMessageID()
	b := newMessageID()
	if a == b {
		t.Errorf("newMessageID not unique: %s == %s", a, b)
	}
	if len(a) != 24 {
		t.Errorf("newMessageID len = %d, want 24", len(a))
	}
}
