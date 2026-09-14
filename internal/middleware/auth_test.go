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
