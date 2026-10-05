// External test package on purpose: a new internal test file is typechecked
// standalone by the diff-scoped lint lane and would fail on sibling symbols
// (envMap, countingHandler, ...). This file drives only the exported surface
// (guard.New / Guard.Check / AgentGuardConfig.Validate), so it needs none.
package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// ── canned decisions responses ──────────────────────────────────────────

const (
	// jevBenign: ordinary traffic — attack_class none, severity 0.
	jevBenign = `{"model":"typesafe/jev-1.13","answers":{
		"is_prompt_injection":{"type":"noul","noul":0,"confidence":0.99},
		"is_jailbreak":{"type":"noul","noul":0,"confidence":0.99},
		"severity":{"type":"score","score":0,"confidence":0.9},
		"attack_class":{"type":"choice","choice":"none","confidence":0.9},
		"is_quoted_or_discussed":{"type":"noul","noul":0,"confidence":0.9}
	},"usage":{"cost":0.0001}}`

	// jevInjection: attack_class instruction_injection, severity 2.
	jevInjection = `{"model":"typesafe/jev-1.13","answers":{
		"is_prompt_injection":{"type":"noul","noul":0.98,"confidence":0.97},
		"is_jailbreak":{"type":"noul","noul":0.02,"confidence":0.5},
		"severity":{"type":"score","score":2,"confidence":0.95},
		"attack_class":{"type":"choice","choice":"instruction_injection","confidence":0.95},
		"is_quoted_or_discussed":{"type":"noul","noul":0,"confidence":0.5}
	},"usage":{"cost":0.0002}}`

	// jevSeverityOnly: no class, but severity says dangerous → the mapping's
	// second block clause (severity >= 2), not the class clause.
	jevSeverityOnly = `{"model":"typesafe/jev-1.13","answers":{
		"is_prompt_injection":{"type":"noul","noul":0},
		"is_jailbreak":{"type":"noul","noul":0},
		"severity":{"type":"score","score":2},
		"attack_class":{"type":"choice","choice":"none"},
		"is_quoted_or_discussed":{"type":"noul","noul":0}
	},"usage":{"cost":0.0002}}`

	// jevUnusualButHarmless: severity 1, no class → allow with risk medium.
	jevUnusualButHarmless = `{"model":"typesafe/jev-1.13","answers":{
		"is_prompt_injection":{"type":"noul","noul":0},
		"is_jailbreak":{"type":"noul","noul":0},
		"severity":{"type":"score","score":1},
		"attack_class":{"type":"choice","choice":"none"},
		"is_quoted_or_discussed":{"type":"noul","noul":0}
	},"usage":{"cost":0.0001}}`

	// chatAllow is the OpenAI-compatible chat-completions envelope used by the
	// fallback provider in the chain test.
	chatAllow = `{"choices":[{"message":{"content":"{\"decision\":\"allow\",\"risk_level\":\"low\",\"reason\":\"clean\",\"matched_patterns\":[]}"}}]}`
)

// ── fixtures ────────────────────────────────────────────────────────────

// jevStub is a decisions-endpoint stand-in: it records the last request so a
// test can assert the wire shape (path, auth, body keys) and serves a canned
// response (or a status).
type jevStub struct {
	mu       sync.Mutex
	calls    int
	status   int
	response string
	lastBody map[string]any
	lastAuth string
	lastPath string
}

func (s *jevStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	s.lastPath = r.URL.Path
	s.lastAuth = r.Header.Get("Authorization")
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	s.lastBody = body
	status, response := s.status, s.response
	s.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"stub"}}`))
		return
	}
	_, _ = w.Write([]byte(response))
}

func (s *jevStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *jevStub) body() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody
}

func newJevStub(t *testing.T, status int, response string) (*jevStub, *httptest.Server) {
	t.Helper()
	s := &jevStub{status: status, response: response}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

// jevLookupEnv is the guard's env:VAR seam with both fixture keys.
func jevLookupEnv(k string) string {
	switch k {
	case "OPENROUTER_API_KEY":
		return "jev-fixture-key"
	case "CHAT_KEY":
		return "chat-fixture-key"
	}
	return ""
}

func newJevGuard(t *testing.T) *guard.Guard {
	t.Helper()
	g, err := guard.New(guard.Options{
		Timeout:       5 * time.Second,
		MaxConcurrent: 8,
		LookupEnv:     jevLookupEnv,
	})
	if err != nil {
		t.Fatalf("guard.New: %v", err)
	}
	return g
}

func jevCfg(baseURL string) *guard.AgentGuardConfig {
	return &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID: "jev-test",
		Providers: []guard.ProviderSpec{{
			// base URL override to the local stub; model + key ref come from
			// the preset (that resolution is part of what is under test).
			Provider: "jev",
			BaseURL:  baseURL,
		}},
	}}}
}

func jevInput() guard.Input {
	return guard.Input{
		AgentID:   "agent-1",
		MessageID: "msg-1",
		Sender:    "sender",
		Payload:   []byte(`{"text":"hello world"}`),
	}
}

func checkJev(t *testing.T, g *guard.Guard, cfg *guard.AgentGuardConfig, in guard.Input) guard.Result {
	t.Helper()
	res, err := g.Check(context.Background(), in.AgentID, cfg, in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

// ── AC1: the seam — validation + preset resolution + provenance ─────────

// TestJev_PolicyValidationAcceptsProvider proves a policy may NAME the jev
// provider: before the preset existed the validator's default arm answered
// "unknown provider"; now the preset table is the single source of truth and
// jev resolves through it (no policy.go change was needed). A control proves
// the unknown-provider rejection still fires.
func TestJev_PolicyValidationAcceptsProvider(t *testing.T) {
	ok := &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID:        "p",
		Providers: []guard.ProviderSpec{{Provider: "jev"}},
	}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("jev provider must validate: %v", err)
	}
	// Same chain, mixed with another preset — still valid.
	mixed := &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID: "p",
		Providers: []guard.ProviderSpec{
			{Provider: "jev"},
			{Provider: "deepseek"},
		},
	}}}
	if err := mixed.Validate(); err != nil {
		t.Fatalf("jev+deepseek chain must validate: %v", err)
	}
	bad := &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID:        "p",
		Providers: []guard.ProviderSpec{{Provider: "wat"}},
	}}}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("control: err = %v, want unknown provider", err)
	}
}

// TestJev_CheckResolvesPresetAndSendsDecisionsRequest proves the full seam:
// ProviderSpec{Provider:"jev"} with no model/base_url/key-ref set resolves the
// preset's model ("typesafe/jev-1.13") and env key ref, the transport POSTs
// the DECISIONS shape (no /chat/completions suffix, no messages/thinking/
// response_format), the state is the payload projection (not the chat-model
// scaffolding), and the provenance lands on the Result.
func TestJev_CheckResolvesPresetAndSendsDecisionsRequest(t *testing.T) {
	stub, srv := newJevStub(t, 0, jevBenign)
	g := newJevGuard(t)

	res := checkJev(t, g, jevCfg(srv.URL), jevInput())

	if res.Provider != "jev" {
		t.Errorf("provider = %q, want jev", res.Provider)
	}
	if res.Model != "typesafe/jev-1.13" {
		t.Errorf("model = %q, want typesafe/jev-1.13 (preset default)", res.Model)
	}
	if res.Errored {
		t.Errorf("verdict errored: reason = %q", res.Reason)
	}
	if res.Decision != guard.DecisionAllow || res.RiskLevel != guard.RiskLow {
		t.Errorf("decision/risk = %s/%s, want allow/low", res.Decision, res.RiskLevel)
	}

	// Wire shape: the decisions endpoint IS the full path — nothing is
	// appended (client.go's /chat/completions is NOT used on this class).
	if got := stub.lastPath; got != "/" {
		t.Errorf("request path = %q, want \"/\" (no path appended)", got)
	}
	if stub.lastAuth != "Bearer jev-fixture-key" {
		t.Errorf("auth = %q, want the preset's env:OPENROUTER_API_KEY value", stub.lastAuth)
	}
	body := stub.body()
	if got := body["model"]; got != "typesafe/jev-1.13" {
		t.Errorf("body model = %v", got)
	}
	for _, absent := range []string{"messages", "thinking", "response_format", "temperature"} {
		if _, has := body[absent]; has {
			t.Errorf("decisions request must not carry %q", absent)
		}
	}
	state, _ := body["state"].(string)
	if !strings.Contains(state, "hello world") {
		t.Errorf("state = %q, want the payload projection", state)
	}
	if strings.Contains(state, "<message_context>") || strings.Contains(state, "Respond with ONE JSON object") {
		t.Errorf("state must be the payload projection, not the chat-model scaffolding: %q", state)
	}
	questions, ok := body["questions"].(map[string]any)
	if !ok || len(questions) != 5 {
		t.Errorf("questions = %v, want the 5-question calibrated set", body["questions"])
	}
	if stub.count() != 1 {
		t.Errorf("stub calls = %d, want 1", stub.count())
	}
}

// ── AC2: the mapping, both ways ─────────────────────────────────────────

// TestJev_ClassificationMapping proves severity 0/1/2 → low/medium/high and
// the decision rule (attack_class != none OR severity >= 2 → block), with
// carve-outs for the class-named and severity-only block paths, and for the
// reason/pattern evidence.
func TestJev_ClassificationMapping(t *testing.T) {
	cases := []struct {
		name        string
		response    string
		wantDec     guard.Decision
		wantRisk    guard.RiskLevel
		wantReason  string // substring
		wantPattern []string
	}{
		{
			name: "benign", response: jevBenign,
			wantDec: guard.DecisionAllow, wantRisk: guard.RiskLow,
			wantReason: "jev: none (severity 0)", wantPattern: nil,
		},
		{
			name: "unusual but harmless", response: jevUnusualButHarmless,
			wantDec: guard.DecisionAllow, wantRisk: guard.RiskMedium,
			wantReason: "jev: none (severity 1)", wantPattern: nil,
		},
		{
			name: "injection", response: jevInjection,
			wantDec: guard.DecisionBlock, wantRisk: guard.RiskHigh,
			wantReason: "jev: instruction_injection (severity 2", wantPattern: []string{"instruction_injection"},
		},
		{
			name: "severity only", response: jevSeverityOnly,
			wantDec: guard.DecisionBlock, wantRisk: guard.RiskHigh,
			wantReason: "jev: none (severity 2)", wantPattern: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newJevStub(t, 0, tc.response)
			g := newJevGuard(t)
			res := checkJev(t, g, jevCfg(srv.URL), jevInput())

			if res.Decision != tc.wantDec || res.RiskLevel != tc.wantRisk {
				t.Fatalf("decision/risk = %s/%s, want %s/%s", res.Decision, res.RiskLevel, tc.wantDec, tc.wantRisk)
			}
			if res.Errored {
				t.Errorf("verdict errored: %q", res.Reason)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want substring %q", res.Reason, tc.wantReason)
			}
			if res.Decision == guard.DecisionSanitize {
				t.Error("sanitize is reserved and must never be produced by the jev mapping")
			}
			if len(res.Patterns) != len(tc.wantPattern) {
				t.Fatalf("patterns = %v, want %v", res.Patterns, tc.wantPattern)
			}
			for i, p := range tc.wantPattern {
				if res.Patterns[i] != p {
					t.Errorf("patterns[%d] = %q, want %q", i, res.Patterns[i], p)
				}
			}
		})
	}
}

// TestJev_InjectionReasonNamesFiredAnswers pins that a block whose boolean
// questions fired names them (threshold-free: any noul > 0), so an operator
// reading the audit line sees WHY, not only the class.
func TestJev_InjectionReasonNamesFiredAnswers(t *testing.T) {
	_, srv := newJevStub(t, 0, jevInjection)
	g := newJevGuard(t)
	res := checkJev(t, g, jevCfg(srv.URL), jevInput())

	for _, want := range []string{"instruction_injection", "severity 2", "prompt_injection"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q missing %q", res.Reason, want)
		}
	}
}

// ── AC3: chain participation ────────────────────────────────────────────

// TestJev_ChainFallsThroughToChatProvider proves a jev spec whose endpoint
// 500s participates in the chain exactly like a chat spec: one retry on the
// retryable 5xx, then failover to the following custom chat provider, whose
// verdict wins. (The retry/failover mechanics and the request counts are the
// existing router semantics, unchanged by the new transport.)
func TestJev_ChainFallsThroughToChatProvider(t *testing.T) {
	jevStub, jevSrv := newJevStub(t, http.StatusInternalServerError, "")
	chatStub, chatSrv := newJevStub(t, 0, chatAllow)

	g := newJevGuard(t)
	cfg := &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID: "chain",
		Providers: []guard.ProviderSpec{
			{Provider: "jev", BaseURL: jevSrv.URL},
			{Provider: "custom", Model: "chat-model", BaseURL: chatSrv.URL, APIKeyRef: "env:CHAT_KEY"},
		},
	}}}

	res := checkJev(t, g, cfg, jevInput())

	if res.Provider != "custom" || res.Model != "chat-model" {
		t.Fatalf("provider/model = %s/%s, want custom/chat-model (fallback)", res.Provider, res.Model)
	}
	if res.Decision != guard.DecisionAllow || res.RiskLevel != guard.RiskLow {
		t.Errorf("fallback verdict = %s/%s, want allow/low", res.Decision, res.RiskLevel)
	}
	if res.Errored {
		t.Errorf("failover landed on a working provider, must not be errored: %q", res.Reason)
	}
	// 500 is retryable → attempt + one retry on the jev spec.
	if n := jevStub.count(); n != 2 {
		t.Errorf("jev stub calls = %d, want 2 (attempt + retry)", n)
	}
	if n := chatStub.count(); n != 1 {
		t.Errorf("chat stub calls = %d, want 1", n)
	}
	// The chat fallback spoke the chat-completions shape, not the decisions
	// shape — the transport is selected per spec, not per policy.
	if got := chatStub.lastPath; got != "/chat/completions" {
		t.Errorf("fallback path = %q, want /chat/completions", got)
	}
}

// TestJev_MissingKeySkipsAndFailsOver proves a jev spec whose env key is
// absent is SKIPPED (not called) and the chain continues — the "key missing →
// skip/failed with a logf line" requirement, observed end to end through the
// guard (the router's skip/failover lines are covered in router_test.go).
func TestJev_MissingKeySkipsAndFailsOver(t *testing.T) {
	jevStub, jevSrv := newJevStub(t, 0, jevBenign)
	_, chatSrv := newJevStub(t, 0, chatAllow)

	// A guard with NO OPENROUTER_API_KEY; the chat spec's key is present.
	g, err := guard.New(guard.Options{
		Timeout:   5 * time.Second,
		LookupEnv: func(k string) string { return map[string]string{"CHAT_KEY": "chat-key"}[k] },
	})
	if err != nil {
		t.Fatalf("guard.New: %v", err)
	}
	cfg := &guard.AgentGuardConfig{Policies: []guard.Policy{{
		ID: "no-key",
		Providers: []guard.ProviderSpec{
			{Provider: "jev", BaseURL: jevSrv.URL},
			{Provider: "custom", Model: "chat-model", BaseURL: chatSrv.URL, APIKeyRef: "env:CHAT_KEY"},
		},
	}}}

	res := checkJev(t, g, cfg, jevInput())
	if res.Provider != "custom" {
		t.Fatalf("provider = %q, want custom (jev skipped for a missing key)", res.Provider)
	}
	if n := jevStub.count(); n != 0 {
		t.Errorf("jev endpoint must not be called without a key (calls=%d)", n)
	}
}
