package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminStatusAccessIsReadOnlyAndExact(t *testing.T) {
	for _, tc := range []struct {
		method, path, bearer string
		want                 int
	}{
		{http.MethodGet, "/status", "admin", 200},
		{http.MethodGet, "/status?view=management", "admin", 200},
		{http.MethodGet, "/status", "reach", 200},
		{http.MethodGet, "/status", "wrong", 401},
		{http.MethodGet, "/status", "", 401},
		{http.MethodPost, "/status", "admin", 403},
		{http.MethodHead, "/status", "admin", 403},
		{http.MethodGet, "/status/", "admin", 403},
		{http.MethodGet, "/status/anything", "admin", 403},
		{http.MethodPost, "/relay/publish", "admin", 403},
		{http.MethodGet, "/sessions", "admin", 403},
		{http.MethodGet, "/agents", "admin", 403},
	} {
		t.Run(tc.method+tc.path+tc.bearer, func(t *testing.T) {
			called := false
			handler := AuthTokens("reach", "admin")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want || called != (tc.want == http.StatusOK) {
				t.Fatalf("status=%d called=%v; want status=%d", w.Code, called, tc.want)
			}
		})
	}
}
