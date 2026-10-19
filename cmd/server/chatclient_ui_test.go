// CR-CHAT-038 pins the embedded operator shell. Behavioral and overflow coverage
// lives in scripts/chat-ui-browser.cjs and drives a scratch stock server.
package main

import (
	"strings"
	"testing"
)

func TestChatClientOperatorShell(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		`data-ui-revision="CR-CHAT-038"`,
		`aria-label="Session navigation"`,
		`aria-label="Conversation context"`,
		`id="thread-filter"`,
		`id="participants"`,
		`id="permissions"`,
		`id="tasks"`,
		`id="capabilities"`,
		`id="capability-matrix-panel"`,
		`id="capability-matrix-title"`,
		`id="capability-matrix"`,
		`id="capability-matrix-table"`,
		`id="capability-matrix-legend"`,
		`id="capability-matrix-more"`,
		`@media(max-width:900px)`,
		`@media(max-width:480px)`,
		`grid-template-columns:260px minmax(0,1fr) 280px`,
		`overflow-wrap:anywhere`,
		`A tag addresses; it never executes.`,
		`There is no browser login or cookie exchange in the current API`,
		`No per-recipient delivery outcomes returned; delivery is unconfirmed.`,
		`id="identity-gate"`,
		`id="use-suggested"`,
		`id="composer-why"`,
		`Set an acting agent ID (Connection & identity, left) to enable sending.`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing contract marker %q", marker)
		}
	}
}

// DF-CRIER-332 replaces the blanket storage ban with a single attribution-only
// slot. Browser coverage exercises its opt-in, reload, clear and denial paths.
func TestChatClientAttributionOptIn(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		`id="remember-agent" type="checkbox"`,
		`Remember this attribution ID on this device`,
		`Stored only in this browser tab; attribution, not a login.`,
		`id="remember-disclosure"`, `aria-describedby="remember-disclosure"`,
		`id="remember-status"`, `id="forget-agent"`, `>Forget</button>`,
		`Attribution ID restored for this browser tab.`,
		`const ATTRIBUTION_KEY = "crier.chat.attribution-id";`,
		`sessionStorage.setItem(ATTRIBUTION_KEY, state.agent)`,
		`sessionStorage.getItem(ATTRIBUTION_KEY)`,
		`sessionStorage.removeItem(ATTRIBUTION_KEY)`,
		`if ($("remember-agent").checked && state.agent)`,
		`$("remember-agent").addEventListener("change",rememberAttribution)`,
		`$("forget-agent").addEventListener("click",forgetAttribution)`,
		`restoreAttribution();`,
		`id="initial-members" multiple`,
		`Additional registered agents (optional)`,
		`members:initialMembers`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing opt-in/member contract %q", marker)
		}
	}
	if strings.Contains(html, `id="remember-agent" type="checkbox" checked`) {
		t.Error("remember attribution must default off")
	}
	// No second storage slot or other browser persistence may creep in. All
	// access goes through these three explicit operations on the same key.
	if strings.Count(html, "sessionStorage") != 3 {
		t.Error("only the three audited attribution storage operations are allowed")
	}
	for _, forbidden := range []string{`type="password"`, `headers["Authorization"]`, `access_token`, `authToken`, `sessionStorage.clear(`, `Nothing is saved in browser storage.`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("embedded chat contains forbidden persistence/auth construct %q", forbidden)
		}
	}
}

func TestChatClientParticipantAndTaskActions(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		`id="add-participant-form"`, `id="participant-agent" list="registered-agents"`,
		`id="registered-agents"`, `id="add-participant"`, `id="participant-result"`,
		`sessionPath(id,"participants")`, `member_type:"agent", member_id:memberID, role:"member"`,
		`esc(p.member_type)`, `esc(p.role)`, `e.serverMessage || e.message`,
		`data-task-action="claim"`, `data-task-action="complete"`,
		`sessionPath(id,"tasks/" + encodeURIComponent(taskID) + "/" + action)`,
		`actor:state.agent, actor_kind:"agent"`, `result.state`, `await refresh()`,
		`function composerReason()`, `const why = composerReason()`,
		`document.querySelectorAll("[data-task-action]")`, `aria-describedby="composer-why"`,
		`t.state === "open"`, `t.state === "claimed" || t.state === "running"`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing participant/task action contract %q", marker)
		}
	}
}

func TestChatClientNoRecipientConfirmation(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		`id="audience-warning"`, `no agent will receive this — the session has no agent member`,
		`p.active && p.member_type === "agent" && p.member_id !== state.agent`,
		`const roster = await call(sessionPath(id,"participants"))`,
		`const confirmation = JSON.stringify(`, `state.noAgentConfirmation !== confirmation`,
		`state.noAgentConfirmation = confirmation`, `Click Send again to record a note-to-self.`,
		`function resetAudienceWarning()`, `$("text").addEventListener("input",resetAudienceWarning)`,
		`$("recipient").addEventListener("change",resetAudienceWarning)`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing no-recipient confirmation contract %q", marker)
		}
	}
	// The first click returns without recording. Arming must precede the write,
	// inside send itself, not merely appear in an unused helper or comment.
	sendStart := strings.Index(html, "async function send(event)")
	if sendStart < 0 {
		t.Fatal("send handler missing")
	}
	sendEnd := strings.Index(html[sendStart:], `$("sessions").addEventListener`)
	if sendEnd < 0 {
		t.Fatal("send handler boundary missing")
	}
	send := strings.Join(strings.Fields(html[sendStart:sendStart+sendEnd]), " ")
	arm := strings.Index(send, "state.noAgentConfirmation = confirmation")
	stop := strings.Index(send, `$("audience-warning").hidden = false; return;`)
	write := strings.Index(send, `const result = await call(sessionPath(id,task ? "tasks" : "messages")`)
	if arm < 0 || stop <= arm || write <= stop {
		t.Error("first click must arm and return before the message/task POST")
	}
}

func TestChatClientDeclaredCapabilityMatrix(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		`Declared capabilities only — crier records declarations and has no approval state, so "ask" is not shown.`,
		`renderCapabilityMatrix(capabilities);`,
		`state.agents.slice(0,4)`,
		`[...capabilities.keys()].sort()`,
		`unavailable("capability-matrix",e)`,
		`aria-describedby="capability-matrix-legend"`,
		`<td aria-label="Declared">✓</td>`,
		`<td aria-label="Not declared"></td>`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing declaration-only matrix contract %q", marker)
		}
	}
}

func TestChatClientNoParallelAuthOrExecution(t *testing.T) {
	html := string(chatClientHTML)
	for _, forbidden := range []string{
		`localStorage`, `document.cookie`, `Authorization:`, `headers.Authorization`,
		`onclick=`, `onerror=`, `<script src=`, `<link rel="stylesheet"`,
		`overflow-x:hidden`, `overflow-x: hidden`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("embedded chat contains forbidden shortcut %q", forbidden)
		}
	}
	for _, marker := range []string{
		`if (task && !recipient)`,
		`sessionPath(id,task ? "tasks" : "messages")`,
		`if (!task) payload.message_kind`,
		`if (state.reply) payload.parent_id = state.reply`,
		`generation === state.generation`,
		`credentials:"same-origin"`,
		`o.outcome === "refused"`,
		`outcomes.every(o => o.outcome === "refused" || o.outcome === "failed" || o.outcome === "budget_cut")`,
		`if (!task && !retryable) pendingMessageRequest = null`,
		`result.replay_action === "redelivered"`,
		`replayed without another delivery`,
		`its outcomes will be replaced, not a second operator row added.`,
		`.replace(/"/g,"&quot;").replace(/'/g,"&#39;")`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing safety boundary %q", marker)
		}
	}
}
