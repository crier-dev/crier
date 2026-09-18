package relay

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/middleware"
)

// captureLogs redirects the default slog logger into a buffer for the duration
// of the test so handler log lines can be asserted (DF-CRIER-141).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})

	return &buf
}

// publishServer wires the relay routes behind the request-id middleware the
// same way cmd/server does (RequestID outermost). The subscribe route is wired
// here too so a rejected subscription pattern is exercised through the real
// router rather than by calling the handler directly (DF-CRIER-212).
func publishServer(r *Relay) http.Handler {
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", r.HandlePublish).Methods("POST")
	router.HandleFunc("/relay/subscribe/{topic}", r.HandleSubscribe)
	return middleware.RequestID(router)
}

func TestHandlePublishAcceptedLogsTopicAndRequestID(t *testing.T) {
	logs := captureLogs(t)

	// Rate limiting on (limit > 0) so the X-Agent-ID sender is exercised.
	handler := publishServer(New(100))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/relay/publish",
		strings.NewReader(`{"topic":"agent.status","event":{"message_id":"msg-7"}}`))
	req.Header.Set("X-Agent-ID", "agent-a")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	rid := rec.Header().Get(middleware.HeaderRequestID)
	if rid == "" {
		t.Fatalf("response has no %s header", middleware.HeaderRequestID)
	}

	out := logs.String()
	for _, want := range []string{
		"relay: publish accepted",
		"topic=agent.status",
		"message_id=msg-7",
		"sender=agent-a",
		"request_id=" + rid,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

func TestHandlePublishRejectedLogsReason(t *testing.T) {
	logs := captureLogs(t)
	handler := publishServer(New(0))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/relay/publish",
		strings.NewReader(`{"event":{"id":"1"}}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	rid := rec.Header().Get(middleware.HeaderRequestID)

	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "relay: publish rejected") {
		t.Errorf("log %q: want a warn rejection line", out)
	}
	if !strings.Contains(out, "topic is required") {
		t.Errorf("log %q: want the rejection reason", out)
	}
	if !strings.Contains(out, "request_id="+rid) {
		t.Errorf("log %q: want request_id=%s", out, rid)
	}
}

// TestHandleRejectionsDeclareJSONContentType is the DF-CRIER-212 regression
// guard for the relay rejections. Every one of them answered with a JSON body
// served as Content-Type text/plain; charset=utf-8: net/http's Error helper
// hard-codes that header (plus nosniff) before WriteHeader, so it overwrites
// anything the handler set and cannot carry a JSON body — the drift was in the
// header only, next to a rate-limit rejection in the same handler that already
// declared application/json.
//
// Each row drives the real router (RequestID outermost, exactly as cmd/server
// wires it) and asserts the status, the exact Content-Type, the decoded
// {"error": ...} value, and the exact wire bytes.
//
// The expected bodies are literals captured from the pre-fix build (b140a6f),
// which makes this test a headers-only gate as well: the trailing newline is
// part of the body the stdlib helper already produced (that is why the live 400
// for {"error":"invalid json"} reported Content-Length: 25 for 24 bytes of
// JSON).
func TestHandleRejectionsDeclareJSONContentType(t *testing.T) {
	captureLogs(t)

	cases := []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		wantError  string
		wantBody   string
	}{
		{
			name:   "publish_invalid_json",
			method: http.MethodPost, target: "/relay/publish", body: "not-json",
			wantStatus: http.StatusBadRequest, wantError: "invalid json",
			wantBody: "{\"error\":\"invalid json\"}\n",
		},
		{
			name:   "publish_missing_topic",
			method: http.MethodPost, target: "/relay/publish", body: `{"event":{"a":1}}`,
			wantStatus: http.StatusBadRequest, wantError: "topic is required",
			wantBody: "{\"error\":\"topic is required\"}\n",
		},
		{
			name:   "publish_missing_event",
			method: http.MethodPost, target: "/relay/publish", body: `{"topic":"x"}`,
			wantStatus: http.StatusBadRequest, wantError: "event is required",
			wantBody: "{\"error\":\"event is required\"}\n",
		},
		{
			// The path is percent-encoded exactly as the live repro sent it:
			// the router sees "bad topic!" and rejects the pattern.
			name:   "subscribe_invalid_topic",
			method: http.MethodGet, target: "/relay/subscribe/bad%20topic%21",
			wantStatus: http.StatusBadRequest, wantError: "invalid topic",
			wantBody: "{\"error\":\"invalid topic\"}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Rate limiting off: the sender header must not become the reason
			// for the rejection under test.
			handler := publishServer(New(0))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want %q — the rejection body is JSON", ct, "application/json")
			}
			var payload map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response body %q is not valid JSON: %v", rec.Body.String(), err)
			}
			if payload["error"] != tc.wantError {
				t.Errorf("error = %q, want %q", payload["error"], tc.wantError)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want %q (pre-fix wire bytes — this change is headers-only)", got, tc.wantBody)
			}
		})
	}
}
