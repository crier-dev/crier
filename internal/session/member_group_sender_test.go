package session

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionGroupExpansionExcludesAuthor(t *testing.T) {
	h := newGroupHarness(t, "atlas", "nimbus")
	room := h.createRoom(t)
	var group groupView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{Name: "self-and-echo", CreatedBy: "human", Members: []string{"atlas", "nimbus"}}, &group, nil))
	var sent transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), Targets: []AudienceTarget{{Kind: TargetGroup, ID: "self-and-echo"}}}, &sent, nil))
	require.Len(t, sent.Outcomes, 1)
	require.Equal(t, "nimbus", sent.Outcomes[0].Target)
	require.Equal(t, "accepted", sent.Outcomes[0].Outcome)
	entries, _, err := h.reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries)
	entries, _, err = h.reg.Retrieve("nimbus", time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
