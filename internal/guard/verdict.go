package guard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ParseVerdict parses raw LLM output with the transport tolerance of spec
// §3.6 — strip surrounding ```json fences and surrounding whitespace; if
// the first non-whitespace byte is not '{', scan forward to the first '{'
// and parse from there — then strict-validates per §3.1 (normalizing
// reason trim + pattern dedupe). Any remaining parse/validation failure is
// a guard error (fail-open default, fail-closed per policy).
func ParseVerdict(raw string) (Verdict, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		if i := strings.Index(s, "{"); i >= 0 {
			s = s[i:]
		}
	}
	var v Verdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return Verdict{}, fmt.Errorf("guard_error: verdict parse: %w", err)
	}
	if err := v.Validate(); err != nil {
		return Verdict{}, err
	}
	return v, nil
}

// Validate applies the spec §3.1 constraints. Normalization: reason
// trimmed; patterns trimmed + deduplicated (order preserved).
func (v *Verdict) Validate() error {
	switch v.Decision {
	case DecisionAllow, DecisionBlock, DecisionSanitize:
	default:
		return fmt.Errorf("guard_error: invalid decision %q", v.Decision)
	}
	switch v.RiskLevel {
	case RiskLow, RiskMedium, RiskHigh:
	default:
		return fmt.Errorf("guard_error: invalid risk_level %q", v.RiskLevel)
	}
	v.Reason = strings.TrimSpace(v.Reason)
	if n := utf8.RuneCountInString(v.Reason); n < 1 || n > 500 {
		return fmt.Errorf("guard_error: reason length %d runes (want 1..500)", n)
	}
	if len(v.MatchedPatterns) > 32 {
		return fmt.Errorf("guard_error: matched_patterns has %d entries (max 32)", len(v.MatchedPatterns))
	}
	seen := make(map[string]bool, len(v.MatchedPatterns))
	out := make([]string, 0, len(v.MatchedPatterns))
	for _, p := range v.MatchedPatterns {
		p = strings.TrimSpace(p)
		if n := utf8.RuneCountInString(p); n < 1 || n > 100 {
			return fmt.Errorf("guard_error: matched_patterns entry length %d runes (want 1..100)", n)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	v.MatchedPatterns = out
	return nil
}

// escalate applies the deterministic escalation table (spec §3.4) with the
// policy's block_risk threshold (default high). The LLM can never
// under-block below the threshold; it can always over-block.
func escalate(v Verdict, blockRisk RiskLevel) Verdict {
	if blockRisk == "" {
		blockRisk = RiskHigh
	}
	switch v.Decision {
	case DecisionAllow, DecisionSanitize:
		if riskAtLeast(v.RiskLevel, blockRisk) {
			v.Decision = DecisionBlock
			v.Reason = "escalated: risk above block_risk"
		}
	case DecisionBlock:
		// block stays block.
	}
	return v
}

// riskAtLeast reports whether risk r is at or above threshold t
// (low < medium < high).
func riskAtLeast(r, t RiskLevel) bool {
	order := map[RiskLevel]int{RiskLow: 0, RiskMedium: 1, RiskHigh: 2}
	return order[r] >= order[t]
}

// quarantinePayload builds the delivered payload for a sanitized message
// (spec §3.5): the original payload bytes are base64-encoded (std) into
// the guard meta; the delivered payload is replaced with a notice object
// so the recipient sees quarantine, never the original.
func quarantinePayload(in Input, reason string) (delivered []byte, originalB64 string) {
	originalB64 = base64.StdEncoding.EncodeToString(in.Payload)
	notice := map[string]any{
		"crier_guard": map[string]any{
			"quarantined": true,
			"message_id":  in.MessageID,
			"sender":      in.Sender,
			"reason":      reason,
		},
	}
	delivered, _ = json.Marshal(notice)
	return delivered, originalB64
}
