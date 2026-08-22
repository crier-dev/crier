package guard

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict_Valid(t *testing.T) {
	v, err := ParseVerdict(`{"decision":"allow","risk_level":"low","reason":"  benign  ","matched_patterns":["a","b","a"]}`)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Decision != DecisionAllow || v.RiskLevel != RiskLow {
		t.Errorf("verdict = %+v", v)
	}
	if v.Reason != "benign" {
		t.Errorf("reason not trimmed: %q", v.Reason)
	}
	if len(v.MatchedPatterns) != 2 || v.MatchedPatterns[0] != "a" || v.MatchedPatterns[1] != "b" {
		t.Errorf("patterns not deduped order-preserved: %v", v.MatchedPatterns)
	}
}

func TestParseVerdict_FenceTolerance(t *testing.T) {
	for name, raw := range map[string]string{
		"json fence":   "```json\n{\"decision\":\"block\",\"risk_level\":\"high\",\"reason\":\"x\",\"matched_patterns\":[]}\n```",
		"plain fence":  "```\n{\"decision\":\"block\",\"risk_level\":\"high\",\"reason\":\"x\",\"matched_patterns\":[]}\n```",
		"leading text": "Here is the result:\n{\"decision\":\"block\",\"risk_level\":\"high\",\"reason\":\"x\",\"matched_patterns\":[]}",
		"whitespace":   "  \n\t{\"decision\":\"block\",\"risk_level\":\"high\",\"reason\":\"x\",\"matched_patterns\":[]}  ",
	} {
		t.Run(name, func(t *testing.T) {
			v, err := ParseVerdict(raw)
			if err != nil {
				t.Fatalf("ParseVerdict: %v", err)
			}
			if v.Decision != DecisionBlock {
				t.Errorf("decision = %q", v.Decision)
			}
		})
	}
}

func TestParseVerdict_Invalid(t *testing.T) {
	cases := map[string]string{
		"garbage":         "not json at all",
		"no brace":        "garbage without brace",
		"bad decision":    `{"decision":"maybe","risk_level":"low","reason":"x","matched_patterns":[]}`,
		"bad risk":        `{"decision":"allow","risk_level":"extreme","reason":"x","matched_patterns":[]}`,
		"empty reason":    `{"decision":"allow","risk_level":"low","reason":"   ","matched_patterns":[]}`,
		"reason too long": `{"decision":"allow","risk_level":"low","reason":"` + strings.Repeat("r", 501) + `","matched_patterns":[]}`,
		"too many patterns": `{"decision":"allow","risk_level":"low","reason":"x","matched_patterns":["` +
			strings.Join(make([]string, 33), `","`) + `"]}`,
		"pattern too long": `{"decision":"allow","risk_level":"low","reason":"x","matched_patterns":["` + strings.Repeat("p", 101) + `"]}`,
		"missing fields":   `{"decision":"allow"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseVerdict(raw); err == nil {
				t.Fatalf("ParseVerdict(%q): want error, got nil", raw)
			}
		})
	}
}

// TestEscalation covers all 6 rows of the §3.4 table plus a lower
// block_risk threshold.
func TestEscalation(t *testing.T) {
	high := RiskHigh
	cases := []struct {
		name      string
		decision  Decision
		risk      RiskLevel
		blockRisk RiskLevel
		want      Decision
	}{
		{"allow low under high", DecisionAllow, RiskLow, high, DecisionAllow},
		{"allow high at high", DecisionAllow, RiskHigh, high, DecisionBlock},
		{"sanitize low under high", DecisionSanitize, RiskLow, high, DecisionSanitize},
		{"sanitize high at high", DecisionSanitize, RiskHigh, high, DecisionBlock},
		{"block low stays block", DecisionBlock, RiskLow, high, DecisionBlock},
		{"block high stays block", DecisionBlock, RiskHigh, high, DecisionBlock},
		{"allow medium at medium", DecisionAllow, RiskMedium, RiskMedium, DecisionBlock},
		{"allow low under medium", DecisionAllow, RiskLow, RiskMedium, DecisionAllow},
		{"sanitize medium at medium", DecisionSanitize, RiskMedium, RiskMedium, DecisionBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := escalate(Verdict{Decision: tc.decision, RiskLevel: tc.risk}, tc.blockRisk)
			if v.Decision != tc.want {
				t.Fatalf("escalate(%s, %s, block_risk=%s) = %s, want %s",
					tc.decision, tc.risk, tc.blockRisk, v.Decision, tc.want)
			}
			if tc.want == DecisionBlock && tc.decision != DecisionBlock {
				if !strings.HasPrefix(v.Reason, "escalated:") {
					t.Errorf("reason = %q, want escalated prefix", v.Reason)
				}
			}
		})
	}
}

func TestQuarantinePayload(t *testing.T) {
	in := Input{MessageID: "m1", Sender: "s1", Payload: []byte(`{"text":"secret"}`)}
	delivered, b64 := quarantinePayload(in, "risky")
	// The original rides base64-encoded in the meta.
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || string(raw) != `{"text":"secret"}` {
		t.Fatalf("quarantined payload round-trip failed: %v %q", err, raw)
	}
	// The delivered payload is the §3.5 notice object.
	var notice map[string]any
	if err := json.Unmarshal(delivered, &notice); err != nil {
		t.Fatalf("delivered payload not JSON: %v", err)
	}
	cg, ok := notice["crier_guard"].(map[string]any)
	if !ok {
		t.Fatalf("delivered payload missing crier_guard: %v", notice)
	}
	if cg["quarantined"] != true || cg["message_id"] != "m1" || cg["sender"] != "s1" || cg["reason"] != "risky" {
		t.Errorf("notice = %v", cg)
	}
	// The delivered payload must NOT contain the original content.
	if strings.Contains(string(delivered), "secret") {
		t.Errorf("delivered payload leaks original content")
	}
}

func TestMetaResultRoundTrip(t *testing.T) {
	m := Meta{
		Decision: DecisionSanitize, RiskLevel: RiskMedium, Reason: "r",
		Patterns: []string{"p1"}, Policy: "pol", Provider: "deepseek",
		Model: "m", Errored: false, Quarantined: true,
		QuarantinedPayload: "b64",
	}
	r := m.Result()
	if r.Decision != DecisionSanitize || r.PolicyID != "pol" || r.QuarantinedPayload != "b64" {
		t.Fatalf("Result projection wrong: %+v", r)
	}
	back := r.Meta()
	if back.Policy != "pol" || back.QuarantinedPayload != "b64" {
		t.Fatalf("Meta round-trip wrong: %+v", back)
	}
}
