package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/hermesession"
)

// Hermes-session delivery (specs/HERMES-SESSION-DELIVERY.md): the transport is
// exercised through the SAME Dispatch choke point every other delivery uses —
// guard, sender idempotency, transport choice — against an httptest stand-in
// for the gateway. No real Hermes, no network.

// sessionGateway is the fake gateway: create, inspect, chat.
type sessionGateway struct {
	mu       sync.Mutex
	token    string
	sessions map[string]sessionRow
	created  int
	chats    int
}

type sessionRow struct {
	ID        string   `json:"id"`
	EndedAt   *float64 `json:"ended_at,omitempty"`
	EndReason string   `json:"end_reason,omitempty"`
}

func newSessionGateway(t *testing.T, token string) (*sessionGateway, *httptest.Server) {
	t.Helper()
	g := &sessionGateway{token: token, sessions: map[string]sessionRow{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorized(w, r) {
			return
		}
		g.mu.Lock()
		g.created++
		id := "hermes-" + strconv.Itoa(g.created)
		g.sessions[id] = sessionRow{ID: id}
		g.mu.Unlock()
		w.Header().Set("X-Hermes-Session-Id", id)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": id}})
	})
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if !g.authorized(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		id := strings.TrimSuffix(rest, "/chat")
		g.mu.Lock()
		row, ok := g.sessions[id]
		if strings.HasSuffix(rest, "/chat") && ok {
			g.chats++
		}
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Session not found","code":"session_not_found"}}`))
			return
		}
		if strings.HasSuffix(rest, "/chat") {
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": id,
				"message": map[string]any{"role": "assistant", "content": "ack"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session": row})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *sessionGateway) authorized(w http.ResponseWriter, r *http.Request) bool {
	if g.token == "" || r.Header.Get("Authorization") == "Bearer "+g.token {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","code":"unauthorized"}}`))
	return false
}

func (g *sessionGateway) counts() (created, chats int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.created, g.chats
}

func (g *sessionGateway) end(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	at := 1720000000.0
	row := g.sessions[id]
	row.EndedAt, row.EndReason = &at, "done"
	g.sessions[id] = row
}

// sessionToken writes a mode-0600 gateway credential and returns its path.
func sessionToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hermes-gateway.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod token: %v", err)
	}
	return path
}

// sessionHarness registers one agent with the given session config and returns
// a router that serves the deliver endpoint through a wired handler.
func sessionHarness(t *testing.T, cfg *hermesession.Config) (http.Handler, *Handler, Store) {
	t.Helper()
	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	agent := &Agent{ID: "session-agent", PublicKey: HexKey(pubKey), Hermes: cfg}
	if err := store.Register(agent); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandler(store)
	h.SetHermesSessionClient(hermesession.NewClient(2 * time.Second))
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	return router, h, store
}

func postHermesInbox(t *testing.T, router http.Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/session-agent/inbox", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d, body is not JSON: %v — raw: %s", rec.Code, err, rec.Body.String())
	}
	return rec, out
}

func TestHermesSessionDelivery_HTTPAcceptNamesTransportAndSession(t *testing.T) {
	gw, srv := newSessionGateway(t, "gw-secret")
	router, _, store := sessionHarness(t, &hermesession.Config{
		BaseURL: srv.URL, TokenFile: sessionToken(t, "gw-secret"), Policy: hermesession.PolicyNew,
	})

	rec, body := postHermesInbox(t, router, `{"payload":{"task":"summarise"},"sender":"agent-b","request_id":"req-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	if body["transport"] != hermesession.Transport {
		t.Fatalf("transport = %v, want %q", body["transport"], hermesession.Transport)
	}
	sessionID, _ := body["hermes_session_id"].(string)
	if sessionID == "" {
		t.Fatalf("accept carries no hermes_session_id: %s", rec.Body.String())
	}
	if body["reply"] != "ack" {
		t.Errorf("reply = %v, want the turn's answer", body["reply"])
	}
	if body["id"] == "" {
		t.Error("accept carries no crier message id")
	}
	created, chats := gw.counts()
	if created != 1 || chats != 1 {
		t.Fatalf("gateway created=%d chats=%d, want 1 and 1", created, chats)
	}

	// The durable inbox is bypassed, exactly like a webhook accept.
	entries, _, err := store.Retrieve("session-agent", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("session delivery also wrote %d inbox entr(y/ies); the inbox must be bypassed", len(entries))
	}
}

func TestHermesSessionDelivery_FourNamedFailuresOverHTTP(t *testing.T) {
	// 1) unknown session: 404 HERMES_SESSION_NOT_FOUND.
	t.Run("not found", func(t *testing.T) {
		_, srv := newSessionGateway(t, "gw-secret")
		router, _, _ := sessionHarness(t, &hermesession.Config{
			BaseURL: srv.URL, TokenFile: sessionToken(t, "gw-secret"),
			Policy: hermesession.PolicyExisting, SessionID: "gone",
		})
		rec, body := postHermesInbox(t, router, `{"payload":{"task":"x"}}`)
		if rec.Code != http.StatusNotFound || body["error"] != hermesession.CodeSessionNotFound {
			t.Fatalf("status/error = %d/%v, want 404/%s", rec.Code, body["error"], hermesession.CodeSessionNotFound)
		}
		if body["gateway_status"] != float64(http.StatusNotFound) {
			t.Fatalf("gateway_status = %v, want 404 verbatim", body["gateway_status"])
		}
	})

	// 2) closed session: 409 HERMES_SESSION_CLOSED.
	t.Run("closed", func(t *testing.T) {
		gw, srv := newSessionGateway(t, "gw-secret")
		gw.mu.Lock()
		gw.sessions["ended"] = sessionRow{ID: "ended"}
		gw.mu.Unlock()
		gw.end("ended")
		router, _, _ := sessionHarness(t, &hermesession.Config{
			BaseURL: srv.URL, TokenFile: sessionToken(t, "gw-secret"),
			Policy: hermesession.PolicyExisting, SessionID: "ended",
		})
		rec, body := postHermesInbox(t, router, `{"payload":{"task":"x"}}`)
		if rec.Code != http.StatusConflict || body["error"] != hermesession.CodeSessionClosed {
			t.Fatalf("status/error = %d/%v, want 409/%s", rec.Code, body["error"], hermesession.CodeSessionClosed)
		}
		if gw.counts2chats() != 0 {
			t.Fatal("a closed session was chatted into")
		}
	})

	// 3) bad credential: 502 HERMES_GATEWAY_UNAUTHORIZED with 401 verbatim.
	t.Run("unauthorized", func(t *testing.T) {
		_, srv := newSessionGateway(t, "gw-secret")
		router, _, _ := sessionHarness(t, &hermesession.Config{
			BaseURL: srv.URL, TokenFile: sessionToken(t, "wrong-secret"), Policy: hermesession.PolicyNew,
		})
		rec, body := postHermesInbox(t, router, `{"payload":{"task":"x"}}`)
		if rec.Code != http.StatusBadGateway || body["error"] != hermesession.CodeGatewayUnauthorized {
			t.Fatalf("status/error = %d/%v, want 502/%s", rec.Code, body["error"], hermesession.CodeGatewayUnauthorized)
		}
		if body["gateway_status"] != float64(http.StatusUnauthorized) {
			t.Fatalf("gateway_status = %v, want 401 verbatim", body["gateway_status"])
		}
	})

	// 4) gateway gone: 502 HERMES_GATEWAY_UNREACHABLE.
	t.Run("unreachable", func(t *testing.T) {
		router, _, _ := sessionHarness(t, &hermesession.Config{
			BaseURL: "http://127.0.0.1:1", TokenFile: sessionToken(t, "gw-secret"), Policy: hermesession.PolicyNew,
		})
		rec, body := postHermesInbox(t, router, `{"payload":{"task":"x"}}`)
		if rec.Code != http.StatusBadGateway || body["error"] != hermesession.CodeGatewayUnreachable {
			t.Fatalf("status/error = %d/%v, want 502/%s", rec.Code, body["error"], hermesession.CodeGatewayUnreachable)
		}
		if _, present := body["gateway_status"]; present {
			t.Fatalf("gateway_status = %v, want absent when no exchange happened", body["gateway_status"])
		}
	})
}

// counts2chats is a helper for the closed-session cell.
func (g *sessionGateway) counts2chats() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.chats
}

func TestHermesSessionDelivery_ReplayByMessageID(t *testing.T) {
	gw, srv := newSessionGateway(t, "gw-secret")
	_, h, _ := sessionHarness(t, &hermesession.Config{
		BaseURL: srv.URL, TokenFile: sessionToken(t, "gw-secret"), Policy: hermesession.PolicyNew,
	})

	entry := &InboxEntry{ID: "fixed-msg-1", Payload: []byte(`{"task":"once"}`), Sender: "agent-b"}
	if err := h.Deliver("session-agent", entry); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	replayed := &InboxEntry{ID: "fixed-msg-1", Payload: []byte(`{"task":"once"}`), Sender: "agent-b"}
	if err := h.Deliver("session-agent", replayed); err != nil {
		t.Fatalf("replayed delivery: %v", err)
	}
	created, chats := gw.counts()
	if created != 1 || chats != 1 {
		t.Fatalf("replay created=%d chats=%d, want 1 and 1", created, chats)
	}
}

func TestHermesSessionDelivery_IdempotencyKeyReplayNamesTheSession(t *testing.T) {
	gw, srv := newSessionGateway(t, "gw-secret")
	router, _, _ := sessionHarness(t, &hermesession.Config{
		BaseURL: srv.URL, TokenFile: sessionToken(t, "gw-secret"), Policy: hermesession.PolicyNew,
	})

	body := `{"payload":{"task":"once"},"idempotency_key":"retry-1"}`
	rec1, out1 := postHermesInbox(t, router, body)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d — %s", rec1.Code, rec1.Body.String())
	}
	rec2, out2 := postHermesInbox(t, router, body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay status = %d — %s", rec2.Code, rec2.Body.String())
	}
	if out2["idempotent_replay"] != true {
		t.Fatalf("second delivery is not marked idempotent_replay: %s", rec2.Body.String())
	}
	if out1["hermes_session_id"] != out2["hermes_session_id"] {
		t.Fatalf("replay names session %v, first named %v", out2["hermes_session_id"], out1["hermes_session_id"])
	}
	if created, chats := gw.counts(); created != 1 || chats != 1 {
		t.Fatalf("idempotent replay created=%d chats=%d, want 1 and 1", created, chats)
	}
}

func TestHermesSessionDelivery_UnwiredTransportIsRefusedNotInboxed(t *testing.T) {
	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	if err := store.Register(&Agent{ID: "session-agent", PublicKey: HexKey(pubKey),
		Hermes: &hermesession.Config{BaseURL: "http://127.0.0.1:1", TokenFile: "/run/secrets/gw", Policy: hermesession.PolicyNew}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandler(store) // no SetHermesSessionClient
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")

	rec, body := postHermesInbox(t, router, `{"payload":{"task":"x"}}`)
	if rec.Code != http.StatusBadGateway || body["error"] != hermesTransportUnavailable {
		t.Fatalf("status/error = %d/%v, want 502/%s", rec.Code, body["error"], hermesTransportUnavailable)
	}
	entries, _, err := store.Retrieve("session-agent", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(entries) != 0 {
		t.Fatal("a refused session delivery fell back to the inbox")
	}
}

func TestHermesSessionConfig_RegistrationValidation(t *testing.T) {
	store := setupTestStore(t)
	h := NewHandler(store)
	router := mux.NewRouter()
	router.HandleFunc("/agents", h.HandleRegister).Methods("POST")

	pubKeyHex, _ := newTestPubKey(t)
	bad := map[string]any{
		"id":         "session-agent",
		"public_key": pubKeyHex,
		"hermes_session": map[string]any{
			"base_url": "http://127.0.0.1:9", "token_file": "/run/secrets/gw", "policy": "existing",
		},
	}
	raw, _ := json.Marshal(bad)
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_id is required") {
		t.Fatalf("400 does not name the missing field: %s", rec.Body.String())
	}
}

// TestHermesSessionDelivery_FailureCarriesNamedCodeForGoCallers: a non-HTTP
// caller can branch on the code without parsing prose.
func TestHermesSessionDelivery_FailureCarriesNamedCodeForGoCallers(t *testing.T) {
	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	if err := store.Register(&Agent{ID: "session-agent", PublicKey: HexKey(pubKey),
		Hermes: &hermesession.Config{BaseURL: "http://127.0.0.1:1", TokenFile: sessionToken(t, "gw-secret"), Policy: hermesession.PolicyNew}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandler(store)
	h.SetHermesSessionClient(hermesession.NewClient(time.Second))

	err := h.Deliver("session-agent", &InboxEntry{ID: "m-1", Payload: []byte(`{"task":"x"}`)})
	var dispatch *DispatchError
	if !errors.As(err, &dispatch) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatch.Code != hermesession.CodeGatewayUnreachable || dispatch.StatusCode != http.StatusBadGateway {
		t.Fatalf("code/status = %s/%d, want %s/502", dispatch.Code, dispatch.StatusCode, hermesession.CodeGatewayUnreachable)
	}
}

// keep context imported for the in-process caller shape.
var _ = context.Background
