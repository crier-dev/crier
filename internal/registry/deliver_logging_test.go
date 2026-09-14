package registry

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

// TestHandleDeliver_InboxAcceptedLogsCorrelation is the DF-CRIER-141 gate on
// the inbox-store accept path: the 201 carries the target agent, sender and
// message id, and the same X-Request-Id the response echoes.
func TestHandleDeliver_InboxAcceptedLogsCorrelation(t *testing.T) {
	logs := captureLogs(t)

	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	if err := store.Register(&Agent{ID: "agent-inbox", PublicKey: HexKey(pubKey)}); err != nil {
		t.Fatalf("register: %v", err)
	}

	handler := NewHandler(store)
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	// RequestID outermost, exactly as cmd/server wires it.
	wrapped := middleware.RequestID(router)

	body, _ := json.Marshal(deliverRequest{Sender: "agent-a", Payload: json.RawMessage(`{"hello":"world"}`)})
	req := httptest.NewRequest(http.MethodPost, "/agents/agent-inbox/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(middleware.HeaderRequestID, "req-inbox-1")
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := rec.Header().Get(middleware.HeaderRequestID); got != "req-inbox-1" {
		t.Errorf("echoed %s = %q, want the inbound id", middleware.HeaderRequestID, got)
	}

	out := logs.String()
	for _, want := range []string{
		"inbox deliver accepted",
		"target=agent-inbox",
		"sender=agent-a",
		"transport=inbox",
		"request_id=req-inbox-1",
		"message_id=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}
