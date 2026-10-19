package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func permissionsReadServer(t *testing.T) (string, string, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":              df297MessageToken,
		"CR_PERMISSIONS_ENABLED":     "true",
		"CR_PERMISSIONS_DIR":         dir,
		"CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken,
		"CR_REQUIRE_AGENT_SIG":       "false",
	})
	return base, dir, &http.Client{Timeout: 10 * time.Second}
}

func TestPermissionsReadRoundTrip(t *testing.T) {
	base, dir, client := permissionsReadServer(t)
	post := func(path, body string) map[string]any {
		t.Helper()
		code, raw := df297Request(t, client, http.MethodPost, base+path, df297AdminToken, "", body)
		require.Equal(t, http.StatusCreated, code, "%s: %s", path, raw)
		return df297JSON(t, raw)
	}
	first := post("/principals", `{"display_name":"First"}`)
	second := post("/principals", `{"display_name":"Second"}`)
	binding := post("/bindings", fmt.Sprintf(`{"principal":%q,"agent":"atlas"}`, first["id"]))
	post("/agents/atlas/class", fmt.Sprintf(`{"class":"personal","owner":%q}`, first["id"]))
	post("/agents/beacon/class", `{"class":"service"}`)
	grant := post("/grants", fmt.Sprintf(`{"principal":%q,"subject":{"type":"agent","ref":"beacon"},"actions":["send"]}`, first["id"]))

	// Append later versions to the actual durable log, deliberately with OLDER
	// envelope timestamps: order, not max(ts), decides keep-LAST. No API write
	// operation is added for these record versions or the optional stored token.
	const token = "fixture-only-principal-token-315"
	first["token"] = token
	first["display_name"] = "Later"
	binding["as_agent"] = false
	// A different binding id for the SAME pair must replace the old binding.
	binding["id"] = "bind_later"
	grant["actions"] = []string{"read"}
	appendRecord := func(kind string, payload map[string]any) {
		t.Helper()
		record := map[string]any{"v": 1, "type": "permission." + kind, "id": payload["id"], "ts": "2000-01-01T00:00:00Z", kind: payload}
		line, err := json.Marshal(record)
		require.NoError(t, err)
		f, err := os.OpenFile(filepath.Join(dir, "permissions.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
		require.NoError(t, err)
		_, err = f.Write(append(line, '\n'))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	appendRecord("principal", first)
	appendRecord("binding", binding)
	appendRecord("agent", map[string]any{"id": "atlas", "class": "service"})
	appendRecord("grant", grant)
	// Latest tombstones and expired grants must not appear as live grants.
	revoked := post("/grants", fmt.Sprintf(`{"principal":%q,"subject":{"type":"agent","ref":"atlas"},"actions":["send"]}`, second["id"]))
	code, raw := df297Request(t, client, http.MethodPost, base+"/grants/"+revoked["id"].(string)+"/revoke", df297AdminToken, "", `{}`)
	require.Equal(t, http.StatusOK, code, raw)
	post("/grants", fmt.Sprintf(`{"principal":%q,"subject":{"type":"agent","ref":"atlas"},"actions":["send"],"expires_at":"2000-01-01T00:00:00Z"}`, second["id"]))
	path := filepath.Join(dir, "permissions.jsonl")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	statBefore, err := os.Stat(path)
	require.NoError(t, err)
	get := func(path string) map[string]any {
		t.Helper()
		code, raw := df297Request(t, client, http.MethodGet, base+path, df297AdminToken, "", "")
		require.Equal(t, http.StatusOK, code, "GET %s: %s", path, raw)
		sum := sha256.Sum256([]byte(token))
		require.NotContains(t, raw, token)
		require.NotContains(t, raw, hex.EncodeToString(sum[:]))
		t.Logf("GET %s: %s", path, raw)
		return df297JSON(t, raw)
	}
	principals := get("/principals")
	require.EqualValues(t, 2, principals["count"])
	items := principals["items"].([]any)
	var found bool
	for _, item := range items {
		p := item.(map[string]any)
		require.NotContains(t, p, "token")
		require.NotContains(t, p, "token_hash")
		if p["id"] == first["id"] {
			require.Len(t, p, 4, "principal response is an explicit four-field allowlist")
			found = true
			sum := sha256.Sum256([]byte(token))
			require.Equal(t, hex.EncodeToString(sum[:4]), p["token_fingerprint"])
			require.Equal(t, "principal", p["kind"])
			require.Equal(t, first["created_at"], p["created_at"])
		} else {
			require.NotContains(t, p, "token_fingerprint", "tokenless principal has no fingerprint")
		}
	}
	require.True(t, found)
	bindings := get("/bindings")
	require.EqualValues(t, 1, bindings["count"])
	b := bindings["items"].([]any)[0].(map[string]any)
	require.Equal(t, "bind_later", b["id"])
	require.Equal(t, false, b["as_agent"])
	require.Equal(t, "service", get("/agents/atlas/class")["class"])
	require.Equal(t, "service", get("/agents/beacon/class")["class"])
	grants := get("/grants")
	require.EqualValues(t, 1, grants["count"])
	g := grants["items"].([]any)[0].(map[string]any)
	require.Equal(t, grant["id"], g["id"])
	require.Equal(t, []any{"read"}, g["actions"])
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "reads must not append or rewrite")
	statAfter, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, statBefore.ModTime(), statAfter.ModTime())
}

func TestPermissionsReadEmptyAndAdminGate(t *testing.T) {
	base, dir, client := permissionsReadServer(t)
	// buildPermissions creates a directory at boot, not a log. Remove that empty
	// directory to prove a GET does not create EITHER the directory or the file.
	require.NoError(t, os.Remove(dir))
	for _, path := range []string{"/principals", "/bindings", "/grants", "/agents/ghost/class"} {
		t.Run(path, func(t *testing.T) {
			code, raw := df297Request(t, client, http.MethodGet, base+path, df297AdminToken, "", "")
			if path == "/agents/ghost/class" {
				require.Equal(t, http.StatusNotFound, code, raw)
				body := df297JSON(t, raw)
				require.Equal(t, "AGENT_NOT_FOUND", body["error"])
				require.Contains(t, body["detail"], `no such agent "ghost"`)
			} else {
				require.Equal(t, http.StatusOK, code, raw)
				require.JSONEq(t, `{"items":[],"count":0}`, raw)
			}
			for _, credential := range []string{"", "wrong-token"} {
				code, raw = df297Request(t, client, http.MethodGet, base+path, credential, "", "")
				require.Equal(t, http.StatusUnauthorized, code, raw)
			}
			code, raw = df297Request(t, client, http.MethodGet, base+path, df297MessageToken, "", "")
			require.Equal(t, http.StatusForbidden, code, raw)
			require.Contains(t, raw, "MANAGEMENT_FORBIDDEN")
		})
	}
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "read created missing permissions directory: %v", err)
}
