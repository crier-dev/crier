package main

// DF-CRIER-445: fixture for the interface Status claims. The generic route
// claims prove registration (non-404, including a named refusal); this fixture
// additionally proves populated reads and writes on the real run() server with
// the ACL armed. It uses only test-owned temporary stores and credentials.
// No browser, external relay, database service or authentication flow is started.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDocsClaimsInterfaceAuthorityFixture(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":              df297MessageToken,
		"CR_PERMISSIONS_ENABLED":     "true",
		"CR_PERMISSIONS_DIR":         t.TempDir(),
		"CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken,
		"CR_REQUIRE_AGENT_SIG":       "false",
		"CR_REGISTRY_BACKEND":        "memory",
		"CR_SESSION_BACKEND":         "jsonl",
		"CR_SESSION_LOG_ROOT":        t.TempDir(),
		"CR_GROUP_ROOT":              t.TempDir(),
		"CR_FED_RECORDS_FILE":        "",
	})
	client := &http.Client{Timeout: 10 * time.Second}

	code, body := df297Request(t, client, http.MethodPost, base+"/principals",
		df297AdminToken, "", `{"display_name":"Status fixture","namespace":"docs","role":"owner"}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	principal, ok := df297JSON(t, body)["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, principal)
	t.Logf("POST /principals -> %d; principal stored with role=owner", code)

	for _, write := range []struct{ path, body string }{
		{"/bindings", fmt.Sprintf(`{"principal":%q,"agent":"atlas"}`, principal)},
		{"/grants", fmt.Sprintf(`{"principal":%q,"subject":{"type":"agent","ref":"atlas"},"actions":["send"]}`, principal)},
	} {
		code, body = df297Request(t, client, http.MethodPost, base+write.path,
			df297AdminToken, "", write.body)
		require.Equal(t, http.StatusCreated, code, "%s", body)
		t.Logf("POST %s -> %d; record stored", write.path, code)
	}

	for _, path := range []string{"/principals", "/bindings", "/grants"} {
		code, body = df297Request(t, client, http.MethodGet, base+path, df297AdminToken, "", "")
		require.Equal(t, http.StatusOK, code, "%s", body)
		list := df297JSON(t, body)
		items, ok := list["items"].([]any)
		require.True(t, ok, "%s items is not an array", path)
		require.Len(t, items, 1)
		require.Equal(t, float64(1), list["count"])
		item, ok := items[0].(map[string]any)
		require.True(t, ok)
		if path == "/principals" {
			require.Equal(t, principal, item["id"])
			require.Equal(t, "Status fixture", item["display_name"])
			require.Equal(t, "owner", item["role"])
		} else {
			require.Equal(t, principal, item["principal"])
		}
		t.Logf("GET %s -> %d; count=1; minted principal matched", path, code)

		code, body = df297Request(t, client, http.MethodGet, base+path, df297MessageToken, "", "")
		require.Equal(t, http.StatusForbidden, code, "%s", body)
		require.Equal(t, "MANAGEMENT_FORBIDDEN", df297JSON(t, body)["error"])
		t.Logf("GET %s with message credential -> %d MANAGEMENT_FORBIDDEN", path, code)
	}

	for _, read := range []struct{ path, credential string }{
		{"/fed/peers", df297MessageToken},
		{"/trust/anchors", df297AdminToken},
	} {
		code, body = df297Request(t, client, http.MethodGet, base+read.path, read.credential, "", "")
		require.Equal(t, http.StatusOK, code, "%s", body)
		t.Logf("GET %s -> %d", read.path, code)
	}
}

// The generic scanner substitutes docsclaims-probe for EVERY placeholder.
// Seed matching resources, just as seedProbeChatSession does, so an existing
// resource route is not misclassified as absent merely because its id is absent.
func seedDocsClaimsInterfaceResources(t *testing.T, client *http.Client, base string) {
	t.Helper()
	code, body := df297Request(t, client, http.MethodPost, base+"/groups", "test-token", "",
		`{"name":"docsclaims-probe","created_by":"docsclaims-probe","members":["docsclaims-probe"]}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "jsonl", cfg.ResolvedSessionBackend())
	store, err := session.NewJSONLStore(cfg.Session.LogRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	seq, err := store.NextSeq(context.Background(), docsClaimsProbeAgent)
	require.NoError(t, err)
	// Resource seed only, NOT a send/compile success claim: a plain message
	// makes expand answer its real 400 NOT_COMPILED, instead of resource 404.
	require.NoError(t, store.PostMessage(context.Background(), &session.Message{
		ID: docsClaimsProbeAgent, SessionID: docsClaimsProbeAgent,
		ThreadID: docsClaimsProbeAgent, Seq: seq, Kind: session.MessagePlain,
		Author: session.AuthorRef{Agent: docsClaimsProbeAgent}, CreatedAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"text":"docs scanner resource fixture"}`),
	}))
}
