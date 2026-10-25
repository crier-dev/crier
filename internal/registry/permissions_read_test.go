package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/stretchr/testify/require"
)

type permissionsReadFailureStore struct {
	permissions.Store
	reads int
}

func (s *permissionsReadFailureStore) Snapshot(context.Context) (*permissions.Snapshot, error) {
	s.reads++
	return nil, errors.New("malformed record containing fixture-private-token-and-hash")
}

func TestPermissionsReadFailClosed(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(*Handler, http.ResponseWriter, *http.Request){
		"principals": (*Handler).HandleListPrincipals,
		"bindings":   (*Handler).HandleListBindings,
		"grants":     (*Handler).HandleListGrants,
		"class":      (*Handler).HandleGetAgentClass,
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(NewMemoryStore())
			store := &permissionsReadFailureStore{}
			h.SetPermissionsStore(store)
			h.SetPermissionsAdminToken("admin-read-test")
			for _, token := range []string{"", "message-read-test"} {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				call(h, rec, req)
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), "MANAGEMENT_FORBIDDEN")
				require.Zero(t, store.reads, "unauthorized requests must not even read the log")
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer admin-read-test")
			rec := httptest.NewRecorder()
			call(h, rec, req)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			require.JSONEq(t, `{"error":"store failure"}`, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "fixture-private")
			require.Equal(t, 1, store.reads)
			h.SetPermissionsStore(nil)
			rec = httptest.NewRecorder()
			call(h, rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code)
			h.SetPermissionsStore(store)
			h.SetPermissionsAdminToken("")
			rec = httptest.NewRecorder()
			call(h, rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.Equal(t, 1, store.reads)
		})
	}
}

func TestPermissionsReadPrincipalDescriptions(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	m.router.HandleFunc("/principals", m.handler.HandleListPrincipals).Methods(http.MethodGet)
	mint := func(body string) map[string]any {
		t.Helper()
		rec, p := m.post(t, "/principals", m.token, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		return p
	}
	first := mint(`{"display_name":"Nina Principal","namespace":"acme","role":"owner","status":"active"}`)
	second := mint(`{"display_name":"  Other Principal  ","namespace":"other-realm","role":"viewer","status":"invited"}`)
	require.NotEqual(t, first["id"], second["id"])

	const token = "fixture-only-principal-token-383"
	sum := sha256.Sum256([]byte(token))
	fullHash := hex.EncodeToString(sum[:])
	read := func() map[string]map[string]any {
		t.Helper()
		rec := doMgmtRequest(t, m.router, http.MethodGet, "/principals", m.token, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		raw := rec.Body.String()
		require.NotContains(t, raw, token)
		require.NotContains(t, raw, fullHash)
		require.NotRegexp(t, `[0-9a-fA-F]{64}`, raw)
		var list permissionList[map[string]any]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Equal(t, 2, list.Count)
		require.Len(t, list.Items, list.Count)
		byID := make(map[string]map[string]any)
		for _, p := range list.Items {
			require.NotContains(t, p, "token")
			require.NotContains(t, p, "token_hash")
			byID[p["id"].(string)] = p
		}
		require.Len(t, byID, list.Count, "the fold must return each principal only once")
		return byID
	}
	assertRecorded := func(want, got map[string]any) {
		t.Helper()
		for _, field := range []string{"id", "kind", "created_at", "display_name", "namespace", "role", "status"} {
			require.Equal(t, want[field], got[field], "recorded field %s for %s", field, want["id"])
		}
	}
	initial := read()
	assertRecorded(first, initial[first["id"].(string)])
	assertRecorded(second, initial[second["id"].(string)])
	for _, p := range initial {
		require.NotContains(t, p, "token_fingerprint", "minting does not create a token")
	}

	// Append actual stored versions, not a new write API. The OLDER envelope
	// timestamp proves append order, not max(ts), still decides keep-LAST.
	appendVersion := func(p map[string]any) {
		t.Helper()
		line, err := json.Marshal(map[string]any{
			"v": 1, "type": "permission.principal", "id": p["id"],
			"ts": "2000-01-01T00:00:00Z", "principal": p,
		})
		require.NoError(t, err)
		f, err := os.OpenFile(m.perms.Path(), os.O_WRONLY|os.O_APPEND, 0600)
		require.NoError(t, err)
		_, err = f.Write(append(line, '\n'))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	first["display_name"] = "Nina Renamed"
	first["namespace"] = "new-realm"
	first["role"] = "member"
	first["status"] = "suspended"
	first["remote"] = true
	first["remote_instance"] = "remote.example"
	first["remote_ref"] = "remote-nina"
	first["last_login_at"] = "2026-10-01T12:00:00Z"
	first["token"] = token
	first["token_hash"] = fullHash
	first["token_fingerprint"] = "forged"
	appendVersion(first)
	before, err := os.ReadFile(m.perms.Path())
	require.NoError(t, err)
	later := read()
	updated := later[first["id"].(string)]
	assertRecorded(first, updated)
	assertRecorded(second, later[second["id"].(string)])
	for _, field := range []string{"remote", "remote_instance", "remote_ref", "last_login_at"} {
		require.Equal(t, first[field], updated[field], "recorded field %s", field)
	}
	require.Equal(t, hex.EncodeToString(sum[:4]), updated["token_fingerprint"])
	require.Regexp(t, `^[0-9a-f]{8}$`, updated["token_fingerprint"])
	require.Len(t, updated, 12, "only the explicit non-secret fields may be exposed")
	after, err := os.ReadFile(m.perms.Path())
	require.NoError(t, err)
	require.Equal(t, before, after, "reading must not rewrite stored descriptions or secrets")

	// A later version may remove descriptions: do not merge stale fields back
	// in, invent a namespace/role, or retain the previous token fingerprint.
	for _, field := range []string{"display_name", "namespace", "role", "remote_instance", "remote_ref"} {
		first[field] = ""
	}
	first["remote"] = false
	first["last_login_at"] = nil
	delete(first, "token")
	delete(first, "token_hash")
	appendVersion(first)
	cleared := read()
	require.Equal(t, map[string]any{
		"id": first["id"], "kind": first["kind"], "created_at": first["created_at"], "status": "suspended",
	}, cleared[first["id"].(string)])
	assertRecorded(second, cleared[second["id"].(string)])
}
