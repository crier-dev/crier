package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPermissionsManagementAuthorizedMatchesReadGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bearer string
		store, token bool
		want         bool
	}{
		{"no-store", "Bearer admin-secret", false, true, false},
		{"no-token", "Bearer admin-secret", true, false, false},
		{"missing", "", true, true, false},
		{"wrong", "Bearer wrong", true, true, false},
		{"wrong-scheme", "Basic admin-secret", true, true, false},
		{"exact", "Bearer admin-secret", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMgmtTest(t)
			if !tc.store {
				m.handler.SetPermissionsStore(nil)
			}
			if !tc.token {
				m.handler.SetPermissionsAdminToken("")
			}
			r := httptest.NewRequest(http.MethodGet, "/principals", nil)
			r.Header.Set("Authorization", tc.bearer)
			require.Equal(t, tc.want, m.handler.PermissionsManagementAuthorized(r))
			w := httptest.NewRecorder()
			m.handler.HandleListPrincipals(w, r)
			if tc.want {
				require.Equal(t, http.StatusOK, w.Code)
			} else {
				require.Equal(t, http.StatusForbidden, w.Code)
			}
			require.Zero(t, m.appendCount(t), "posture/read must not append fixture records")
		})
	}
}
