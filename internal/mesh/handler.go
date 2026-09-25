package mesh

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

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

// inboxNotifyParam is the query parameter an agent sets on the mesh connect
// URL to be pinged when a message lands in its inbox (CR-FEAT-023):
// /mesh/connect/{agentID}?inbox_notify=1.
const inboxNotifyParam = "inbox_notify"

// parseInboxNotifyOptIn reads the new-message-ping opt-in from a connect
// request (CR-FEAT-023). Absent or empty means no pings — the behaviour every
// existing client gets, since an unasked-for frame would change what arrives on
// a socket it already reads. A value that is present must be a boolean
// (1/0/true/false), never a silently ignored typo.
func parseInboxNotifyOptIn(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get(inboxNotifyParam)
	if raw == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (1/0/true/false)", inboxNotifyParam)
	}
	return enabled, nil
}

// HandleConnect upgrades an incoming WebSocket connection from a peer agent.
// The agent ID is taken from the URL path: /mesh/connect/{agentID}
//
// `?inbox_notify=1` opts this CONNECTION into the new-message ping
// (CR-FEAT-023): when a message lands in the agent's durable inbox, the server
// writes one INBOX_NOTIFY frame (MessageType TypeInboxNotify) to this socket —
// a tick, not a payload, so an agent that would otherwise poll on a timer can
// be woken. The opt-in is per connection and dropped when the socket closes;
// without it the server never writes an unsolicited frame here.
func HandleConnect(m *Mesh) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agentID"]
		if agentID == "" {
			// The rejection body is JSON: net/http's Error helper would answer
			// "text/plain; charset=utf-8" (DF-CRIER-212).
			httperr.WriteJSONError(w, http.StatusBadRequest, "agentID is required")
			return
		}

		// Read the opt-in BEFORE the upgrade: after it the HTTP request has
		// become a WebSocket, and a bad parameter must be answered, not
		// silently ignored on a socket the caller believes is subscribed.
		inboxNotify, err := parseInboxNotifyOptIn(r)
		if err != nil {
			httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		pc := NewAcceptedPeerConnection(agentID, conn)
		pc.StartReadLoop()
		m.AcceptPeer(agentID, pc)
		if inboxNotify {
			m.SetInboxNotify(agentID, true)
		}

		// WS connect is an Info event (DF-CRIER-141); the accept/disconnect
		// pair around it is debug.
		slog.Info("mesh peer connected", "agent_id", agentID,
			"inbox_notify", inboxNotify,
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
