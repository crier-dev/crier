package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// captureEndpoint records one request's headers and body.
func captureEndpoint(t *testing.T) (*httptest.Server, *http.Header, *[]byte) {
	t.Helper()
	var headers http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		body = buf
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &headers, &body
}

func TestPost_EmitsGuardHeaders(t *testing.T) {
	srv, headers, _ := captureEndpoint(t)
	client := NewClient(2*time.Second, nil)
	cfg := &Config{URL: srv.URL}

	meta := guard.Meta{
		Decision:  guard.DecisionAllow,
		RiskLevel: guard.RiskMedium,
		Reason:    "guard_error: provider down: status 500",
		Patterns:  []string{"p1", "p2"},
		Policy:    "test-policy",
		Provider:  "custom",
		Model:     "mock-model",
		Errored:   true,
	}
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m1", Guard: &meta},
		Payload: json.RawMessage(`{"x":1}`),
	}
	res := client.Post(cfg, env, 0)
	if res.Err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("Post: %+v", res)
	}
	h := *headers
	if got := h.Get("X-Crier-Guard-Decision"); got != "allow" {
		t.Errorf("Decision = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Risk"); got != "medium" {
		t.Errorf("Risk = %q", got)
	}
	// Reason is RFC 3986 percent-encoded (spaces → %20, not +).
	if got := h.Get("X-Crier-Guard-Reason"); got != "guard_error%3A%20provider%20down%3A%20status%20500" {
		t.Errorf("Reason = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Patterns"); got != "p1,p2" {
		t.Errorf("Patterns = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Policy"); got != "test-policy" {
		t.Errorf("Policy = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Provider"); got != "custom" {
		t.Errorf("Provider = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Model"); got != "mock-model" {
		t.Errorf("Model = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Error"); got != "true" {
		t.Errorf("Error = %q", got)
	}
}

func TestPost_NoGuardMetaNoHeaders(t *testing.T) {
	srv, headers, _ := captureEndpoint(t)
	client := NewClient(2*time.Second, nil)
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m1"}, Payload: json.RawMessage(`{}`)}
	res := client.Post(&Config{URL: srv.URL}, env, 0)
	if res.Err != nil {
		t.Fatalf("Post: %v", res.Err)
	}
	h := *headers
	for _, k := range []string{
		"X-Crier-Guard-Decision", "X-Crier-Guard-Risk", "X-Crier-Guard-Reason",
		"X-Crier-Guard-Patterns", "X-Crier-Guard-Policy", "X-Crier-Guard-Provider",
		"X-Crier-Guard-Model", "X-Crier-Guard-Error",
	} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s = %q, want absent", k, v)
		}
	}
}

func TestPostBatch_WorstCaseHeaders(t *testing.T) {
	srv, headers, body := captureEndpoint(t)
	client := NewClient(2*time.Second, nil)
	cfg := &Config{URL: srv.URL, DeliveryMode: "batch"}

	allowLow := &guard.Meta{Decision: guard.DecisionAllow, RiskLevel: guard.RiskLow, Reason: "a", Policy: "p1"}
	sanitizeHigh := &guard.Meta{Decision: guard.DecisionSanitize, RiskLevel: guard.RiskHigh, Reason: "s", Patterns: []string{"b64_blob"}, Policy: "p2", Errored: true}
	allowMedium := &guard.Meta{Decision: guard.DecisionAllow, RiskLevel: guard.RiskMedium, Reason: "b", Patterns: []string{"control_keys"}}

	envs := []*Envelope{
		{Crier: EnvelopeMeta{MessageID: "m1", Guard: allowLow}, Payload: json.RawMessage(`{}`)},
		{Crier: EnvelopeMeta{MessageID: "m2", Guard: sanitizeHigh}, Payload: json.RawMessage(`{}`)},
		{Crier: EnvelopeMeta{MessageID: "m3", Guard: allowMedium}, Payload: json.RawMessage(`{}`)},
		{Crier: EnvelopeMeta{MessageID: "m4"}, Payload: json.RawMessage(`{}`)}, // no guard
	}
	res := client.PostBatch(cfg, envs, 0)
	if res.Err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PostBatch: %+v", res)
	}
	h := *headers
	if got := h.Get("X-Crier-Guard-Decision"); got != "sanitize" {
		t.Errorf("Decision = %q, want sanitize (worst case)", got)
	}
	if got := h.Get("X-Crier-Guard-Risk"); got != "high" {
		t.Errorf("Risk = %q, want high", got)
	}
	// Patterns unioned across inner messages.
	if got := h.Get("X-Crier-Guard-Patterns"); got != "b64_blob,control_keys" {
		t.Errorf("Patterns = %q", got)
	}
	if got := h.Get("X-Crier-Guard-Error"); got != "true" {
		t.Errorf("Error = %q, want true (any inner errored)", got)
	}
	// Per-message verdicts stay in the batch body's inner envelopes.
	var batch batchEnvelopeBody
	if err := json.Unmarshal(*body, &batch); err != nil {
		t.Fatalf("batch body: %v", err)
	}
	if len(batch.Messages) != 4 {
		t.Fatalf("batch messages = %d", len(batch.Messages))
	}
	if batch.Messages[1].Crier.Guard == nil || batch.Messages[1].Crier.Guard.Decision != guard.DecisionSanitize {
		t.Errorf("inner envelope guard not preserved")
	}
	if batch.Messages[3].Crier.Guard != nil {
		t.Errorf("unguarded message gained guard metadata")
	}
}

func TestPostBatch_AllAllow(t *testing.T) {
	srv, headers, _ := captureEndpoint(t)
	client := NewClient(2*time.Second, nil)
	envs := []*Envelope{
		{Crier: EnvelopeMeta{MessageID: "m1", Guard: &guard.Meta{Decision: guard.DecisionAllow, RiskLevel: guard.RiskLow, Reason: "a"}}, Payload: json.RawMessage(`{}`)},
		{Crier: EnvelopeMeta{MessageID: "m2", Guard: &guard.Meta{Decision: guard.DecisionAllow, RiskLevel: guard.RiskMedium, Reason: "b"}}, Payload: json.RawMessage(`{}`)},
	}
	res := client.PostBatch(&Config{URL: srv.URL, DeliveryMode: "batch"}, envs, 0)
	if res.Err != nil {
		t.Fatalf("PostBatch: %v", res.Err)
	}
	h := *headers
	if got := h.Get("X-Crier-Guard-Decision"); got != "allow" {
		t.Errorf("Decision = %q, want allow", got)
	}
	if got := h.Get("X-Crier-Guard-Risk"); got != "medium" {
		t.Errorf("Risk = %q, want medium (highest)", got)
	}
}

func TestPostBatch_NoGuardMeta(t *testing.T) {
	srv, headers, _ := captureEndpoint(t)
	client := NewClient(2*time.Second, nil)
	envs := []*Envelope{
		{Crier: EnvelopeMeta{MessageID: "m1"}, Payload: json.RawMessage(`{}`)},
	}
	res := client.PostBatch(&Config{URL: srv.URL, DeliveryMode: "batch"}, envs, 0)
	if res.Err != nil {
		t.Fatalf("PostBatch: %v", res.Err)
	}
	h := *headers
	if got := h.Get("X-Crier-Guard-Decision"); got != "" {
		t.Errorf("Decision = %q, want absent", got)
	}
}

func TestPercentEncode_RFC3986(t *testing.T) {
	got := percentEncode("guard_error: timeout after 10s, budget spent")
	if strings.Contains(got, "+") {
		t.Errorf("percent-encoding must not use '+' for spaces: %q", got)
	}
	if !strings.Contains(got, "%20") {
		t.Errorf("spaces must encode as %%20: %q", got)
	}
}
