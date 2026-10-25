package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDatabaseURLSQLiteProduction(t *testing.T) {
	parallelBoot(t)
	root := t.TempDir()
	path := filepath.Join(root, "from-url.sqlite")
	base := startTestServerWithEnv(t, map[string]string{
		"CR_DATABASE_URL":     "sqlite://" + path,
		"CR_REGISTRY_BACKEND": "", "CR_REGISTRY_SQLITE_PATH": "",
		"CR_SESSION_BACKEND": "", "CR_SQLITE_PATH": "",
		"CR_GROUP_ROOT":        filepath.Join(root, "groups"),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	code, body := sessionDo(t, http.DefaultClient, "GET", base+"/status", nil, nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"registry_backend":"sqlite"`)
	code, body = sessionDo(t, http.DefaultClient, "POST", base+"/agents", map[string]any{"id": "url-agent"}, nil)
	require.Equal(t, http.StatusCreated, code, body)
	code, body = sessionDo(t, http.DefaultClient, "POST", base+"/sessions", map[string]any{
		"title": "URL-backed session", "created_by": map[string]string{"agent": "url-agent"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, body)
	_, err := os.Stat(path)
	require.NoError(t, err, "production boot must open the URL-derived database")
	t.Logf("registry and session writes HTTP 201 at URL-derived file %s", path)
}
