package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
