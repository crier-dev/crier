package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthNoTokenConfiguredPassesThrough(t *testing.T) {
	handler := Auth("")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), "ok")
	}
}

func TestAuthHealthEndpointBypassesWithTokenSet(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

// TestAuthVersionEndpointBypassesWithTokenSet: the build-identity endpoint is
// exempt like /health, so an operator can ask a live server what it runs
// without holding a token (DF-CRIER-101).
func TestAuthVersionEndpointBypassesWithTokenSet(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"version":"1.2.3"}`))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/version", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"version"`) {
		t.Errorf("body = %q, want the handler's version JSON", body)
	}
}

func TestAuthMissingAuthorizationHeaderReturns401(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/relay/publish", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["error"] != "missing Authorization header" {
		t.Fatalf("error = %q, want %q", body["error"], "missing Authorization header")
	}
}

func TestAuthNonBearerSchemeReturns401(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/relay/publish", nil)
	request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["error"] != "authorization scheme must be Bearer" {
		t.Fatalf("error = %q, want %q", body["error"], "authorization scheme must be Bearer")
	}
}

func TestAuthWrongTokenReturns401(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Header.Set("Authorization", "Bearer wrong-token")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["error"] != "invalid token" {
		t.Fatalf("error = %q, want %q", body["error"], "invalid token")
	}
}

func TestAuthCorrectTokenPassesThrough(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authenticated"))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Header.Set("Authorization", "Bearer secret-token")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "authenticated" {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), "authenticated")
	}
}

func TestAuthCorrectTokenOnPostPassesThrough(t *testing.T) {
	handler := Auth("shared-secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"published"}`))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/relay/publish", nil)
	request.Header.Set("Authorization", "Bearer shared-secret")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
}

func TestAuthTokenWithLeadingWhitespaceIsRejected(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mesh/peers", nil)
	request.Header.Set("Authorization", "Bearer  secret-token")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestAuthEmptyTokenAfterBearerReturns401(t *testing.T) {
	handler := Auth("secret-token")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Header.Set("Authorization", "Bearer ")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["error"] != "invalid token" {
		t.Fatalf("error = %q, want %q", body["error"], "invalid token")
	}
}

func TestAuthTokenWithTrailingSpacesIsRejected(t *testing.T) {
	handler := Auth("key")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mesh/peers", nil)
	request.Header.Set("Authorization", "Bearer key  ")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (token must be exact, no TrimSpace)", recorder.Code, http.StatusUnauthorized)
	}
}

// TestAuthConstantTimeComparisonPreservesTokenSemantics pins the accept/reject
// semantics of the constant-time token comparison (DF-CRIER-185): only the
// exact configured token reaches the handler; every wrong token — including a
// same-length token differing in a single byte — answers 401 with the same
// body. Length mismatch remains observable because
// subtle.ConstantTimeCompare returns 0 when lengths differ; that is the
// accepted trade-off for a shared secret.
func TestAuthConstantTimeComparisonPreservesTokenSemantics(t *testing.T) {
	cases := []struct {
		name       string
		authorized string
		presented  string
		wantStatus int
	}{
		{
			name:       "exact token is accepted",
			authorized: "secret-token",
			presented:  "secret-token",
			wantStatus: http.StatusOK,
		},
		{
			name:       "same-length token differing only in final byte is rejected",
			authorized: "secret-token",
			presented:  "secret-tokeN",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "longer token is rejected",
			authorized: "secret-token",
			presented:  "secret-tokenXX",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "shorter token is rejected",
			authorized: "secret-token",
			presented:  "secret-toke",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := Auth(tc.authorized)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("authenticated"))
			}))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/agents", nil)
			request.Header.Set("Authorization", "Bearer "+tc.presented)

			handler.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if !called {
					t.Fatal("handler should have been called for the exact token")
				}
				if recorder.Body.String() != "authenticated" {
					t.Fatalf("body = %q, want %q", recorder.Body.String(), "authenticated")
				}
				return
			}
			if called {
				t.Fatal("handler should not be called for a wrong token")
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			if body["error"] != "invalid token" {
				t.Fatalf("error = %q, want %q", body["error"], "invalid token")
			}
		})
	}
}
