package session

import (
	"encoding/json"
	"strings"

	"github.com/crier-dev/crier/internal/federation"
)

// transportReceiptOutcome classifies a successful inbox/peer exchange, NOT an
// agent answer. Only exact receipt vocabulary can promote queue acceptance to
// delivered. Federation policy permission (FED_ALLOWED) is not such evidence.
// Remote prose/unknown codes are never copied into the durable transcript.
func transportReceiptOutcome(body json.RawMessage, verdict, inboundVerdict string) (Outcome, string) {
	var receipt struct {
		Status         string `json:"status"`
		Outcome        string `json:"outcome"`
		Verdict        string `json:"verdict"`
		InboundVerdict string `json:"inbound_verdict"`
		Delivered      bool   `json:"delivered"`
	}
	// Malformed/unknown receipt shapes remain accepted, never inferred delivery.
	if err := json.Unmarshal(body, &receipt); err != nil {
		receipt.Status, receipt.Outcome, receipt.Verdict, receipt.InboundVerdict, receipt.Delivered = "", "", "", "", false
	}
	words := []string{verdict, inboundVerdict, receipt.Status, receipt.Outcome, receipt.Verdict, receipt.InboundVerdict}
	code := ""
	held, delivered := false, receipt.Delivered
	for _, word := range words {
		switch strings.ToLower(strings.TrimSpace(word)) {
		case "held":
			held = true
		case "delivered":
			delivered = true
		case strings.ToLower(string(federation.FedTokenOnly)):
			code = string(federation.FedTokenOnly)
		}
	}
	// A hold is more specific than admission. Token-only acceptance can never
	// be promoted to delivered, even alongside an optimistic delivery marker.
	if held {
		return OutcomeHeld, code
	}
	if code != "" {
		return OutcomeAccepted, code
	}
	if delivered {
		return OutcomeDelivered, ""
	}
	return OutcomeAccepted, ""
}
