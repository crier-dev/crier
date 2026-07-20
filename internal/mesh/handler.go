package mesh

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
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
			http.Error(w, `{"error":"agentID is required"}`, http.StatusBadRequest)
			return
		}

		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		pc := NewAcceptedPeerConnection(agentID, conn)
		pc.StartReadLoop()
		m.AcceptPeer(agentID, pc)

		slog.Info("mesh peer connected", "agent_id", agentID)
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
