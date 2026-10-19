package session

import (
	"context"
	"encoding/json"
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

func (h *Handler) recordWebhookReply(ctx context.Context, parent *Message, target string, raw json.RawMessage) error {
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
