package session

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CR-CHAT-022 — @team:x mention resolution through the send path
// ---------------------------------------------------------------------------

// TestGroupAPI_TeamMentionResolvesToCurrentMembers: a body tag `@team:<name>`
// in a session message adds the group to the audience, and the fan-out
// resolves the tag to the group's CURRENT members — one delivery per member
// through the shipped inbox path, no explicit targets needed.
func TestGroupAPI_TeamMentionResolvesToCurrentMembers(t *testing.T) {
	agents := []string{"atlas", "nimbus", "orion", "vega", "lyra"}
	h := newGroupHarness(t, agents...)

	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "ops", CreatedBy: "kara", Members: agents,
	}, new(groupView), nil))

	room := h.createRoom(t)

	// ONE send whose body tags the group — no explicit targets.
	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"rollout starts @team:ops"}`),
		Sender:  "kara-agent",
	}, &msg, nil))

	// The audience records the group target the mention resolved to.
	found := false
	for _, tgt := range msg.Audience.Targets {
		if tgt.Kind == TargetGroup && tgt.ID == "ops" {
			found = true
		}
	}
	require.True(t, found, "the @team:ops mention must appear as a group audience target, got %+v", msg.Audience.Targets)

	// EVERY current member holds the message.
	for _, id := range agents {
		entries, _, err := h.reg.Retrieve(id, time.Minute, 10)
		require.NoError(t, err, "agent %s", id)
		require.Len(t, entries, 1, "agent %s must hold the mentioned message", id)
	}
}

// TestGroupAPI_TeamMentionEditRoutesToCurrentMembers: the mention resolves the
// roster FRESH at send time — after a member is removed, the NEXT send reaches
// the current members and the removed one receives nothing.
func TestGroupAPI_TeamMentionEditRoutesToCurrentMembers(t *testing.T) {
	h := newGroupHarness(t, "atlas", "nimbus", "orion")

	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "ops", CreatedBy: "kara", Members: []string{"atlas", "nimbus"},
	}, new(groupView), nil))

	room := h.createRoom(t)
	send := func() {
		t.Helper()
		var msg transcriptMessage
		require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
			Payload: json.RawMessage(`{"text":"ping @team:ops"}`),
			Sender:  "kara-agent",
		}, &msg, nil))
	}

	send()
	for _, id := range []string{"atlas", "nimbus"} {
		entries, _, err := h.reg.Retrieve(id, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, entries, 1, id)
	}

	// Remove nimbus.
	require.Equal(t, http.StatusOK, h.do(t, http.MethodPatch, "/groups/ops/members", updateGroupMembersRequest{
		Remove: []string{"nimbus"}, Actor: "sam",
	}, new(groupView), nil))

	send()
	// orion was never a member and still receives nothing.
	entries, _, err := h.reg.Retrieve("orion", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries)
	// nimbus received nothing NEW.
	empty, _, err := h.reg.Retrieve("nimbus", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, empty, "the removed member must not receive the post-edit send")
}

// TestGroupAPI_TeamMentionUnknownGroupIsASkip: a mention of a group with no
// roster is a recorded skip — nothing is delivered to anybody, and the
// message is still recorded (the named-error rule, never a silent drop).
func TestGroupAPI_TeamMentionUnknownGroupIsASkip(t *testing.T) {
	h := newGroupHarness(t, "atlas")
	room := h.createRoom(t)

	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"anyone? @team:ghost"}`),
		Sender:  "kara-agent",
	}, &msg, nil))

	entries, _, err := h.reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries, "an unknown group mention must not deliver to anybody")
}

// TestGroupAPI_MentionOfAgentLiteralNameIsNotAGroup: the parser's reserved
// prefix order means an agent literally named "team" (bare form) is still an
// agent addressee and is NOT expanded as a group (D8 / §2.1.1).
func TestGroupAPI_MentionOfAgentLiteralNameIsNotAGroup(t *testing.T) {
	h := newGroupHarness(t, "team", "atlas")
	room := h.createRoom(t)

	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"ask @team about it"}`),
		Sender:  "kara-agent",
	}, &msg, nil))

	// The bare `@team` mention resolved to the AGENT kind, not the group
	// kind: no group audience target is added.
	found := false
	for _, tgt := range msg.Audience.Targets {
		if tgt.Kind == TargetGroup {
			found = true
		}
	}
	require.False(t, found, "a bare @team mention is an agent addressee, never a group target")

	// No group named "team" was ever created, so the group fan-out contributed
	// nothing. The room's member agent (fixAgentA) receives exactly the
	// default-audience delivery; "team" — an agent registered but not in the
	// room — receives nothing from a group route.
	teamEntries, _, err := h.reg.Retrieve("team", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, teamEntries, "no group named team exists, so the agent named team receives nothing via a group route")
}
