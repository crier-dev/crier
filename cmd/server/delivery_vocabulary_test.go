package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/session"
)

// Catch both historical definitions, including Markdown emphasis and wrapped lines.
var inboxDeliveryDefinition = regexp.MustCompile("(?i)delivered(?:`|\\*)*\\s*(?:\\((?:the\\s+)?(?:inbox\\s+accepted|stored\\s+in\\s+the\\s+target's\\s+inbox)|(?:names|means)\\s+the\\s+inbox\\s+entries)")

func TestSessionOutcomeVocabularySpec(t *testing.T) {
	root := filepath.Join("..", "..", "specs")
	body, err := os.ReadFile(filepath.Join(root, "CHAT-SESSIONS.md"))
	require.NoError(t, err)
	for _, outcome := range []session.Outcome{
		session.OutcomeDelivered, session.OutcomeAccepted, session.OutcomeHeld,
		session.OutcomeRefused, session.OutcomeFailed, session.OutcomeCanned,
		session.OutcomeBudgetCut, session.OutcomeUnreadableReply,
		session.OutcomeLeased, session.OutcomeAcked, session.OutcomeExpired,
		session.OutcomeInProgress,
	} {
		require.Contains(t, string(body), "| `"+string(outcome)+"` |", "owning spec must define every wire outcome in its vocabulary table")
	}
	require.Contains(t, string(body), "WEBHOOK-DELIVERY.md")
	paths, err := filepath.Glob(filepath.Join(root, "*.md"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		text, err := os.ReadFile(path)
		require.NoError(t, err)
		normalized := strings.Join(strings.Fields(string(text)), " ")
		require.False(t, inboxDeliveryDefinition.MatchString(normalized), "%s defines delivered as inbox acceptance", path)
	}
	for _, path := range []string{
		filepath.Join(root, "WEBHOOK-DELIVERY.md"),
		filepath.Join(root, "CHAT-INTERFACE.md"),
		filepath.Join(root, "DETECTION.md"),
		filepath.Join(root, "CHAT-STORAGE.md"),
		filepath.Join("..", "..", "docs", "integration-guide.md"),
		filepath.Join("..", "..", "README.md"),
	} {
		text, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(text), "CHAT-SESSIONS.md#delivery-outcome-vocabulary", "%s must link to the single owning table", path)
	}
}

func TestSessionVocabularyTranscriptExample(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "specs", "CHAT-SESSIONS.md"))
	require.NoError(t, err)
	type exampleMessage struct {
		SessionID string                    `json:"session_id"`
		MessageID string                    `json:"message_id"`
		ThreadID  string                    `json:"thread_id"`
		ParentID  string                    `json:"parent_id"`
		Author    session.AuthorRef         `json:"author"`
		Audience  session.Audience          `json:"audience"`
		Outcomes  []session.DeliveryOutcome `json:"outcomes"`
	}
	var sent, answer exampleMessage
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, `{"v":1,"type":"session.message"`) {
			require.NoError(t, json.Unmarshal([]byte(line), &sent), "send example must be valid JSON")
		}
		if strings.HasPrefix(line, `{"v":1,"type":"session.thread.reply"`) {
			require.NoError(t, json.Unmarshal([]byte(line), &answer), "answer example must be valid JSON")
		}
	}
	require.NotEmpty(t, sent.MessageID)
	require.Len(t, sent.Outcomes, 2)
	require.Equal(t, session.OutcomeAccepted, sent.Outcomes[0].Outcome)
	require.NotEmpty(t, sent.Outcomes[0].InboxEntryID)
	require.Equal(t, session.OutcomeDelivered, sent.Outcomes[1].Outcome)
	require.Equal(t, sent.Outcomes[1].Target, answer.Author.Agent)
	require.Equal(t, sent.MessageID, answer.ParentID)
	require.Equal(t, sent.SessionID, answer.SessionID)
	require.Equal(t, sent.ThreadID, answer.ThreadID)
	require.Empty(t, answer.Audience.Targets)
	require.Empty(t, answer.Outcomes)
}

func TestInboxDeliveryDefinitionGuard(t *testing.T) {
	for _, text := range []string{
		"`delivered` (the inbox accepted it)",
		"`delivered` (stored in the target's inbox)",
		"**`delivered` names the inbox entries",
	} {
		require.True(t, inboxDeliveryDefinition.MatchString(text), "must reject %s", text)
	}
	require.False(t, inboxDeliveryDefinition.MatchString("`delivered` means the target produced a real reply"))
}
