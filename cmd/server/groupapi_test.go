package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupAPIRoutesServed is the WIRING test for CR-CHAT-013: the four group
// routes are registered on the served mux and answer the documented shapes.
// The behavioural battery lives in internal/session/groups_http_test.go; this
// one exists so a route that is built but never registered fails here rather
// than only in a client.
func TestGroupAPIRoutesServed(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_GROUP_ROOT":       t.TempDir(),
	})
	client := &http.Client{}

	// Create.
	var created struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name":       "infra",
		"created_by": "kara",
		"members":    []string{"atlas", "nimbus"},
	}, &created)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Equal(t, "infra", created.Name)
	require.Equal(t, []string{"atlas", "nimbus"}, created.Members)

	// Edit the roster.
	var edited struct {
		Members []string `json:"members"`
	}
	code, raw = sessionDo(t, client, http.MethodPatch, base+"/groups/infra/members", map[string]any{
		"add":   []string{"orion"},
		"actor": "sam",
	}, &edited)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, []string{"atlas", "nimbus", "orion"}, edited.Members)

	// Inspect.
	var got struct {
		Name      string   `json:"name"`
		Members   []string `json:"members"`
		CreatedBy string   `json:"created_by"`
		UpdatedBy string   `json:"updated_by"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/groups/infra", nil, &got)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, "kara", got.CreatedBy)
	require.Equal(t, "sam", got.UpdatedBy)

	// List.
	var list struct {
		Count int `json:"count"`
	}
	code, _ = sessionDo(t, client, http.MethodGet, base+"/groups", nil, &list)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, list.Count)

	// A duplicate create is a 409; an unknown group is a 404.
	code, _ = sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{"name": "infra", "created_by": "kara"}, nil)
	require.Equal(t, http.StatusConflict, code)
	code, _ = sessionDo(t, client, http.MethodGet, base+"/groups/nope", nil, nil)
	require.Equal(t, http.StatusNotFound, code)
}

// groupJSON is the groupView wire shape a client reads: name, the CURRENT
// roster and the recorded actors.
type groupJSON struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Members   []string `json:"members"`
	CreatedBy string   `json:"created_by"`
	UpdatedBy string   `json:"updated_by"`
}

// TestGroupAPICreateListAndRoster is the DF-CRIER-449 battery: a named group is
// CREATED through POST /groups and READ BACK through GET /groups (the list) and
// GET /groups/{name} (one roster). Every count a console could show is the
// length of a returned roster, so the count assertions here are over members the
// server actually stored — an empty group reads 0, never "unknown".
func TestGroupAPICreateListAndRoster(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_GROUP_ROOT":       t.TempDir(),
	})
	client := &http.Client{}

	// Create with a roster. The store de-duplicates and sorts, so the roster the
	// console counts is this exact list.
	var infra groupJSON
	code, raw := sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name":       "infra",
		"created_by": "kara",
		"members":    []string{"nimbus", "atlas", "atlas"},
	}, &infra)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Equal(t, "infra", infra.Name)
	require.Equal(t, []string{"atlas", "nimbus"}, infra.Members)
	require.Equal(t, "kara", infra.CreatedBy)

	// Create with NO roster: an empty group is a stored roster, not a refusal.
	var empty groupJSON
	code, raw = sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name":       "empty",
		"created_by": "kara",
	}, &empty)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Equal(t, []string{}, empty.Members)

	// List: both groups, sorted by name, each carrying its own roster — the
	// count is the server's, and each member list is readable in one read.
	var list struct {
		Groups []groupJSON `json:"groups"`
		Count  int         `json:"count"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/groups", nil, &list)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, 2, list.Count)
	require.Len(t, list.Groups, 2)
	require.Equal(t, "empty", list.Groups[0].Name)
	require.Equal(t, "infra", list.Groups[1].Name)
	require.Empty(t, list.Groups[0].Members)
	require.Equal(t, []string{"atlas", "nimbus"}, list.Groups[1].Members)

	// Read one roster back on its own route.
	var got groupJSON
	code, raw = sessionDo(t, client, http.MethodGet, base+"/groups/infra", nil, &got)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, []string{"atlas", "nimbus"}, got.Members)
	require.Equal(t, "kara", got.CreatedBy)
	require.Equal(t, "kara", got.UpdatedBy)

	// The create path records WHO opened the roster: the X-Agent-ID header is the
	// documented fallback when the body names no created_by, and neither is a
	// refusal rather than an anonymous group.
	code, raw = groupDo(t, client, http.MethodPost, base+"/groups", map[string]any{"name": "by-header"}, "atlas")
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Contains(t, raw, `"created_by":"atlas"`)
	code, raw = groupDo(t, client, http.MethodPost, base+"/groups", map[string]any{"name": "anonymous"}, "")
	require.Equal(t, http.StatusBadRequest, code, "body: %s", raw)
	require.Contains(t, raw, "CREATED_BY_REQUIRED")

	// An unaddressable name, and a member that is not an ident, are refusals —
	// a name that cannot be written as @team:<name> is not a group.
	code, raw = sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name": "not a name", "created_by": "kara",
	}, nil)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", raw)
	code, raw = sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name": "badmember", "created_by": "kara", "members": []string{"atlas@x"},
	}, nil)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", raw)
}

// TestGroupConsoleSurfaceServed pins the console half of DF-CRIER-449 against the
// SERVED artifact: the page must carry a create form, a list with a measured
// count, a roster read and an address picker, and it must obtain a group's size
// from the members the server returned (groupMembers) rather than a literal.
func TestGroupConsoleSurfaceServed(t *testing.T) {
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

	// The panel and its controls, in the served page itself.
	for _, marker := range []string{
		`aria-labelledby="groups-title"`, `>Named groups</h2>`,
		`id="groups-count"`, `id="groups"`, `id="group-create-form"`,
		`id="group-name"`, `id="group-members"`, `id="group-create"`,
		`id="group-roster"`, `id="group-result"`,
	} {
		require.Contains(t, page, marker, "served console is missing %q", marker)
	}

	script := page[strings.Index(page, "<script>"):]
	// The three operations the row names: create (POST), list (GET), roster (GET
	// by name) — plus the picker that only inserts the address token.
	for _, marker := range []string{
		`call("/groups", { method:"POST"`,
		`call("/groups")`,
		`"/groups/" + encodeURIComponent`,
		`data-group-read="`, `data-group-address="`,
		`created_by:state.agent`,
		`groupMembers`,
	} {
		require.Contains(t, script, marker, "served console script is missing %q", marker)
	}
	// A group is a roster, not an agent: the count is the stored members' length.
	require.Contains(t, script, `groupCountLabel(members.length)`)
	// The row must not regress into presenting a group as an agent identity.
	require.NotContains(t, script, `X-Agent-ID"] = g.name`)
}

// groupDo issues a group request with an explicit X-Agent-ID header, which
// sessionDo does not carry.
func groupDo(t *testing.T, c *http.Client, method, url string, body any, agentID string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if agentID != "" {
		req.Header.Set("X-Agent-ID", agentID)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}
