package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// hexID matches the generated correlation id: 24 lowercase hex characters
// (12 bytes of crypto/rand), the same shape the registry and mesh use for
// message ids.
var hexID = regexp.MustCompile(`^[0-9a-f]{24}$`)

// serveRequest runs handler for one request and returns the recorder plus the
// correlation id the handler observed in its request context.
func serveRequest(t *testing.T, handler http.Handler, req *http.Request) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var seen string
	wrapped := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		handler.ServeHTTP(w, r)
	}))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec, seen
}

func TestRequestIDGeneratesAndEchoesID(t *testing.T) {
	rec, seen := serveRequest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), httptest.NewRequest(http.MethodGet, "/health", nil))

	echoed := rec.Header().Get(HeaderRequestID)
	if echoed == "" {
		t.Fatalf("response has no %s header", HeaderRequestID)
	}
	if !hexID.MatchString(echoed) {
		t.Errorf("generated id %q is not 24 lowercase hex chars", echoed)
	}
	if seen != echoed {
		t.Errorf("context id %q != echoed id %q", seen, echoed)
	}
}

func TestRequestIDPreservesInboundID(t *testing.T) {
	const inbound = "caller-trace-42"
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	req.Header.Set(HeaderRequestID, inbound)

	rec, seen := serveRequest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), req)

	if got := rec.Header().Get(HeaderRequestID); got != inbound {
		t.Errorf("echoed id = %q, want the inbound %q", got, inbound)
	}
	if seen != inbound {
		t.Errorf("context id = %q, want the inbound %q", seen, inbound)
	}
}

func TestRequestIDReplacesOversizedInboundID(t *testing.T) {
	oversized := strings.Repeat("a", maxRequestIDLen+1)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(HeaderRequestID, oversized)

	rec, seen := serveRequest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), req)

	echoed := rec.Header().Get(HeaderRequestID)
	if echoed == oversized {
		t.Fatalf("oversized inbound id was echoed verbatim: %q", echoed)
	}
	if !hexID.MatchString(echoed) || seen != echoed {
		t.Errorf("replacement id = %q (context %q), want a freshly generated 24-hex id", echoed, seen)
	}
}

func TestRequestIDGeneratesDistinctIDs(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := GenerateRequestID()
		if !hexID.MatchString(id) {
			t.Fatalf("GenerateRequestID() = %q, want 24 lowercase hex chars", id)
		}
		if seen[id] {
			t.Fatalf("GenerateRequestID() returned %q twice in 100 calls", id)
		}
		seen[id] = true
	}
}

// TestLoggingCarriesRequestID proves the per-request access log line and the
// handler's own correlation are the same id, which is what makes the handler
// log lines joinable to the request that caused them.
func TestLoggingCarriesRequestID(t *testing.T) {
	logs := captureLogs(t)
	handler := RequestID(Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))

	id := rec.Header().Get(HeaderRequestID)
	if id == "" {
		t.Fatal("response has no correlation id")
	}
	if !strings.Contains(logs.String(), "path=/agents") {
		t.Fatalf("log = %q, want the request line", logs.String())
	}
	if !strings.Contains(logs.String(), "request_id="+id) {
		t.Errorf("log = %q, want request_id=%s", logs.String(), id)
	}
}
