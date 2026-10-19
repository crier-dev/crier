package session

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// webhookReplyText accepts the raw {reply: string} / {text: string} response
// shapes and schema-extracted JSON strings (including raw plain-text bodies).
// Other JSON is not an answer bubble: never stringify arbitrary transport data.
func webhookReplyText(reply json.RawMessage) string {
	var text string
	if json.Unmarshal(reply, &text) == nil {
		return text
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(reply, &fields) != nil {
		return ""
	}
	for _, key := range []string{"text", "reply"} {
		if json.Unmarshal(fields[key], &text) == nil && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

// webhookReplyVerdict interprets the runtime's verdict, not HTTP acceptance.
// Failure wins over demo markers (failed live runs also report live:false).
// Missing/null flags are not false: legacy reply-only agents remain supported.
func webhookReplyVerdict(reply json.RawMessage) (Outcome, string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(reply, &fields) != nil {
		fields = nil
	}
	readText := func(key string) string {
		var value string
		_ = json.Unmarshal(fields[key], &value)
		return value
	}
	status, reason, runtimeError := readText("status"), readText("reason"), readText("error")
	if strings.EqualFold(strings.TrimSpace(status), "FAILED") || strings.TrimSpace(runtimeError) != "" {
		if strings.TrimSpace(reason) == "" {
			reason = runtimeError
		} else if strings.TrimSpace(runtimeError) != "" && runtimeError != reason {
			reason += "; " + runtimeError
		}
		if strings.TrimSpace(reason) == "" {
			reason = "webhook reported FAILED"
		}
		return OutcomeFailed, reason
	}
	var live, canned *bool
	if err := json.Unmarshal(fields["live"], &live); err != nil {
		live = nil
	}
	if err := json.Unmarshal(fields["canned"], &canned); err != nil {
		canned = nil
	}
	if (live != nil && !*live) || (canned != nil && *canned) {
		if strings.TrimSpace(reason) == "" {
			reason = "webhook reported a canned/demo reply (live:false or canned:true)"
		}
		return OutcomeCanned, reason
	}
	if strings.TrimSpace(webhookReplyText(reply)) == "" {
		// Report shape, not content: transport JSON is not agent speech. Sort
		// the keys so persisted diagnostics do not depend on map iteration.
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		keyList := "(none)"
		if len(keys) > 0 {
			keyList = strings.Join(keys, ", ")
		}
		return OutcomeUnreadableReply, "reply body had no displayable text field; keys: " + keyList
	}
	return OutcomeDelivered, ""
}

func (h *Handler) recordWebhookReply(ctx context.Context, parent *Message, target string, raw json.RawMessage) error {
	if outcome, _ := webhookReplyVerdict(raw); outcome != OutcomeDelivered {
		return nil
	}
	text := webhookReplyText(raw)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	id, err := h.newID()
	if err != nil {
		return err
	}
	seq, err := h.store.NextSeq(ctx, parent.SessionID)
	if err != nil {
		return err
	}
	// A Go string always marshals successfully; retain its text exactly.
	payload, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	// PostMessage is the existing append path, not sendMessage: this plain
	// reply is a record of the target's answer, not a new broadcast or task.
	// Its explicit EMPTY audience prevents reply-default/session expansion,
	// and it has no delivery outcomes. ParentID attributes it without branching.
	return h.store.PostMessage(ctx, &Message{
		ID: id, SessionID: parent.SessionID, ThreadID: parent.ThreadID,
		ParentID: parent.ID, Kind: MessagePlain, Author: AuthorRef{Agent: target},
		Payload: payload, Audience: Audience{Rule: AudienceExplicit, Targets: []AudienceTarget{}},
		Seq: seq, CreatedAt: h.now(),
	})
}
