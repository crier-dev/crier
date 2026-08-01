package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// TestWebSocketUpgradeThroughMiddlewareChain pins the Hijacker contract: a
// WebSocket upgrade MUST succeed when the handler is wrapped in the full
// middleware chain (Logging + Recovery). Regression for BUG-001 — the
// responseWriter wrapper lacked Hijack(), so gorilla/websocket Upgrade
// returned 500 "response does not implement http.Hijacker" on every WS
// endpoint (relay subscribe, mesh connect) served through middleware.
func TestWebSocketUpgradeThroughMiddlewareChain(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	r := mux.NewRouter()
	r.Use(Recovery)
	r.Use(Logging)
	r.HandleFunc("/ws", func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			t.Errorf("upgrade inside handler: %v", err)
			return
		}
		defer conn.Close()
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Errorf("write: %v", err)
		}
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial through middleware chain: %v", err)
	}
	defer conn.Close()

	// Echo round-trip proves the connection is fully usable post-upgrade.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, reply, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(reply) != "ping" {
		t.Fatalf("echo: got %q, want %q", reply, "ping")
	}
}
