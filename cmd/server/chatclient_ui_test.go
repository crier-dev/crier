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
		`.replace(/"/g,"&quot;").replace(/'/g,"&#39;")`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing safety boundary %q", marker)
		}
	}
}
