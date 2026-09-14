package relay

import (
	"bytes"
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

// publishServer wires the publish route behind the request-id middleware the
// same way cmd/server does (RequestID outermost).
func publishServer(r *Relay) http.Handler {
	router := mux.NewRouter()
	router.HandleFunc("/relay/publish", r.HandlePublish).Methods("POST")
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
