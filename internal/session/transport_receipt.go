package session

import (
	"encoding/json"
	"strings"

	"github.com/crier-dev/crier/internal/federation"
)

const codePeerClaimedDelivery = "PEER_CLAIMED_DELIVERY"

// transportReceiptOutcome classifies a successful exchange. Receipt vocabulary
// can prove a hold, not an answer. Delivery requires displayable reply content;
// policy permission (FED_ALLOWED) and peer assertions are not execution evidence.
func transportReceiptOutcome(body json.RawMessage, verdict, inboundVerdict string) (Outcome, string) {
	var receipt struct {
		Status         string `json:"status"`
		Outcome        string `json:"outcome"`
		Verdict        string `json:"verdict"`
		InboundVerdict string `json:"inbound_verdict"`
		Delivered      bool   `json:"delivered"`
	}
	// Preserve the existing receipt-word parsing, including held precedence.
	if err := json.Unmarshal(body, &receipt); err != nil {
		receipt.Status, receipt.Outcome, receipt.Verdict, receipt.InboundVerdict, receipt.Delivered = "", "", "", "", false
	}
	words := []string{verdict, inboundVerdict, receipt.Status, receipt.Outcome, receipt.Verdict, receipt.InboundVerdict}
	claimed := receipt.Delivered
	code := ""
	held := false
	for _, word := range words {
		switch strings.ToLower(strings.TrimSpace(word)) {
		case "held":
			held = true
		case "delivered":
			claimed = true
		case strings.ToLower(string(federation.FedTokenOnly)):
			code = string(federation.FedTokenOnly)
		}
	}
	// Preserve hold precedence and token-only acceptance. A delivery claim
	// without a reply must remain visible even beside token-only vocabulary.
	if held {
		return OutcomeHeld, code
	}
	text := transportReplyText(body)
	if text == "" && claimed {
		return OutcomeAccepted, codePeerClaimedDelivery
	}
	if code != "" {
		return OutcomeAccepted, code
	}
	if text != "" {
		return OutcomeDelivered, ""
	}
	return OutcomeAccepted, ""
}

func transportReceiptReason(code string) string {
	if code == codePeerClaimedDelivery {
		return "peer claimed delivery but no reply observed; transport accepted, not confirmed execution"
	}
	return ""
}

// transportReplyText is an allowlist, never an arbitrary JSON stringifier:
// nonblank JSON strings/plain text; top-level text/reply/answer strings; or an
// artifact/message object with those strings or text parts. Parts accept A2A's
// {text:...}, kind:text and type:text forms, joining text in wire order with \n.
// Status-only bodies, malformed JSON, opaque objects/arrays/scalars, diagnostic
// message strings and explicit failed/canned replies are not observed answers.
// This observes returned content, NOT model authorship or cryptographic proof.
func transportReplyText(body json.RawMessage) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if !json.Valid(body) {
		// Do not turn truncated JSON or structured markup into agent speech.
		if strings.ContainsAny(trimmed[:1], "{[\"<") || transportStatusWord(trimmed) {
			return ""
		}
		return string(body)
	}
	var text string
	if json.Unmarshal(body, &text) == nil {
		if strings.TrimSpace(text) == "" || transportStatusWord(text) {
			return ""
		}
		return text
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return ""
	}
	if outcome, _ := webhookReplyVerdict(body); outcome == OutcomeFailed || outcome == OutcomeCanned {
		return ""
	}
	if text = transportObjectText(fields); text != "" {
		return text
	}
	for _, key := range []string{"artifact", "message"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(fields[key], &nested) != nil || nested == nil {
			continue
		}
		if text = transportObjectText(nested); text != "" {
			return text
		}
		var parts []struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
			Type string `json:"type"`
		}
		if json.Unmarshal(nested["parts"], &parts) != nil {
			continue
		}
		var texts []string
		for _, part := range parts {
			if strings.TrimSpace(part.Text) != "" && (part.Kind == "" || part.Kind == "text") && (part.Type == "" || part.Type == "text") {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

func transportObjectText(fields map[string]json.RawMessage) string {
	for _, key := range []string{"text", "reply", "answer"} {
		var text string
		if json.Unmarshal(fields[key], &text) == nil && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func transportStatusWord(text string) bool {
	word := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(word, "fed_") {
		return true
	}
	switch word {
	case "accepted", "delivered", "held", "queued", "ok", "success", "completed", "pending", "failed":
		return true
	}
	return false
}
