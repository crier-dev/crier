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
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing contract marker %q", marker)
		}
	}
}

func TestChatClientNoParallelAuthOrExecution(t *testing.T) {
	html := string(chatClientHTML)
	for _, forbidden := range []string{
		`localStorage`, `sessionStorage`, `Authorization:`, `headers.Authorization`,
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
