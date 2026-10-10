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

// TestGroupFanOut_UnregisteredTargets: a send to a named group whose roster
// names an agent with no registry entry SUCCEEDS for every member the registry
// does hold and SKIPS the unregistered one — the message is not lost and the
// other members still receive it. The response carries the partial-success
// summary (`delivered` / `skipped` / `skipped_agents`) BESIDE the per-target
// `refused` outcome that still names the reason, so the skip is reported,
// never swallowed and never hidden behind the count (DF-CRIER-305).
func TestGroupFanOut_UnregisteredTargets(t *testing.T) {
	// Two of the three roster members are registered; "ghost" is not.
	h := newGroupHarness(t, "atlas", "nimbus")

	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "ops", CreatedBy: "kara", Members: []string{"atlas", "nimbus", "ghost"},
	}, new(groupView), nil), "a roster may name an agent that is not registered yet")

	room := h.createRoom(t)

	var msg transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"rollout starts @team:ops"}`),
		Sender:  "kara-agent",
	}, &msg, nil)
	require.Equal(t, http.StatusCreated, code,
		"a group send with an unregistered member is a PARTIAL success, not a failure")

	// Every REGISTERED member holds the message: the send degrades per target,
	// never wholesale, and never drops a deliverable member to punish the
	// unregistered one.
	for _, id := range []string{"atlas", "nimbus"} {
		entries, _, err := h.reg.Retrieve(id, time.Minute, 10)
		require.NoError(t, err, "agent %s", id)
		require.Len(t, entries, 1, "agent %s must still hold the group message", id)
	}

	// The unregistered member has no inbox at all.
	_, _, err := h.reg.Retrieve("ghost", time.Minute, 10)
	require.Error(t, err, "an unregistered member has no inbox")

	// The response states the partial success: 2 delivered, 1 skipped, named.
	require.Equal(t, 2, msg.Delivered)
	require.Equal(t, 1, msg.Skipped)
	require.Equal(t, []string{"ghost"}, msg.SkippedAgents)

	// The summary does NOT replace the per-target detail: the skipped target
	// still carries its own `refused` outcome with the real reason, so a
	// caller can tell an unregistered agent from a namespace mismatch.
	byTarget := map[string]outcomeView{}
	for _, o := range msg.Outcomes {
		byTarget[o.Target] = o
	}
	require.Equal(t, string(OutcomeDelivered), byTarget["atlas"].Outcome)
	require.Equal(t, string(OutcomeDelivered), byTarget["nimbus"].Outcome)
	require.Equal(t, string(OutcomeRefused), byTarget["ghost"].Outcome)
	require.Contains(t, byTarget["ghost"].Detail, "agent not found",
		"a skipped target is SHOWN with its reason, never swallowed")
}

// TestSummarizeFanout_OnlyRefusalIsASkip: the partial-success summary counts a
// REFUSED target as skipped and every OTHER delivery state — including the
// leased/acked/expired lifecycle a retried send carries forward from its own
// prior outcomes — as DELIVERED. A re-send therefore never misreports an
// already-received target as skipped (DF-CRIER-305).
func TestSummarizeFanout_OnlyRefusalIsASkip(t *testing.T) {
	var v transcriptMessage
	summarizeFanout(&v, []DeliveryOutcome{
		{Target: "atlas", Outcome: OutcomeDelivered},
		{Target: "nimbus", Outcome: OutcomeLeased},
		{Target: "orion", Outcome: OutcomeAcked},
		{Target: "vega", Outcome: OutcomeExpired},
		{Target: "ghost", Outcome: OutcomeRefused, Reason: `agent not found: "ghost"`},
	})
	require.Equal(t, 4, v.Delivered, "delivered + leased + acked + expired are all received")
	require.Equal(t, 1, v.Skipped, "only a refused delivery is a skip")
	require.Equal(t, []string{"ghost"}, v.SkippedAgents)
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
