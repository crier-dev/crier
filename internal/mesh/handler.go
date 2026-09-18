package mesh

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/crier-dev/crier/internal/httperr"
	"github.com/crier-dev/crier/internal/middleware"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Default: allow all origins. Override with SetWSCheckOrigin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// SetWSCheckOrigin replaces the WebSocket upgrader's CheckOrigin for mesh connections.
func SetWSCheckOrigin(fn func(r *http.Request) bool) {
	wsUpgrader.CheckOrigin = fn
}

// HandleConnect upgrades an incoming WebSocket connection from a peer agent.
// The agent ID is taken from the URL path: /mesh/connect/{agentID}
func HandleConnect(m *Mesh) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agentID"]
		if agentID == "" {
			// The rejection body is JSON: net/http's Error helper would answer
			// "text/plain; charset=utf-8" (DF-CRIER-212).
			httperr.WriteJSONError(w, http.StatusBadRequest, "agentID is required")
			return
		}

		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		pc := NewAcceptedPeerConnection(agentID, conn)
		pc.StartReadLoop()
		m.AcceptPeer(agentID, pc)

		// WS connect is an Info event (DF-CRIER-141); the accept/disconnect
		// pair around it is debug.
		slog.Info("mesh peer connected", "agent_id", agentID,
			"request_id", middleware.RequestIDFromContext(r.Context()))
		m.mu.Lock()
		peers := len(m.connections)
		m.mu.Unlock()
		slog.Debug("mesh: peer accepted", "agent_id", agentID, "peers", peers,
			"request_id", middleware.RequestIDFromContext(r.Context()))
	}
}

// HandlePeers returns the list of connected peers.
func HandlePeers(m *Mesh) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids := m.PeerIDs()
		peers := make([]map[string]string, len(ids))
		for i, id := range ids {
			peers[i] = map[string]string{"agent_id": id}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"peers": peers,
			"count": len(peers),
		})
	}
}
