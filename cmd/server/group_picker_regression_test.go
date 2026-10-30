package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupPickerUsesExplicitRecipientAndNoInventedIDs pins the shipped page,
// not just its source file, for DF-CRIER-487/488. A group selection must be
// visible and replace any prior individual recipient rather than silently
// combining the two audiences.
func TestGroupPickerUsesExplicitRecipientAndNoInventedIDs(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_GROUP_ROOT":       t.TempDir(),
	})
	resp, err := http.Get(base + "/chat")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	page := string(body)

	require.NotContains(t, page, `placeholder="atlas, nimbus"`, "example agent IDs imply a live roster")
	require.Contains(t, page, `id="group-recipient"`, "the selected group must be visible and removable")
	require.Contains(t, page, `id="clear-group-recipient"`)
	script := page[strings.Index(page, "<script>"):]
	require.Contains(t, script, `state.selectedGroup = name`)
	require.Contains(t, script, `$("recipient").value = ""`, "selecting a group must clear the stale agent")
	require.Contains(t, script, `if (recipient) payload.targets = [{ kind:"agent", id:recipient }];`, "the POST must not emit an agent target after group selection")
	require.Contains(t, script, `@team:" + name`, "the chosen group remains the server-resolved address")
	require.Contains(t, script, `selectedGroup`, "composer state must represent the group independently")
}
