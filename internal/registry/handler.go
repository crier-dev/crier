package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

// registerRequest is the JSON body for POST /agents.
type registerRequest struct {
	ID           string   `json:"id"`
	PublicKey    string   `json:"public_key"`
	Capabilities []string `json:"capabilities"`
}

// agentsResponse is the JSON body for GET /agents.
type agentsResponse struct {
	Agents []*Agent `json:"agents"`
}

// deliverRequest is the JSON body for POST /agents/{id}/inbox.
type deliverRequest struct {
	Payload json.RawMessage `json:"payload"`
}

// deliverResponse is the JSON body for POST /agents/{id}/inbox.
type deliverResponse struct {
	ID string `json:"id"`
}

// retrieveResponse is the JSON body for GET /agents/{id}/inbox.
type retrieveResponse struct {
	Messages []*InboxEntry `json:"messages"`
	LeaseID  string        `json:"lease_id"`
}

// ackRequest is the JSON body for POST /agents/{id}/inbox/ack.
type ackRequest struct {
	LeaseID    string   `json:"lease_id"`
	MessageIDs []string `json:"message_ids"`
}

// statsResponse is the JSON body for GET /agents/{id}/inbox/stats.
type statsResponse struct {
	QueueDepth  int   `json:"queue_depth"`
	LeasedCount int   `json:"leased_count"`
	OldestAgeMs int64 `json:"oldest_age_ms"`
}

// HandleRegister handles POST /agents — registers a new agent.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if req.PublicKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public_key is required"})
		return
	}

	rawKey, err := hex.DecodeString(req.PublicKey)
	if err != nil || len(rawKey) != ed25519.PublicKeySize {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public_key must be 64 hex characters (ed25519)"})
		return
	}

	agent := &Agent{
		ID:           req.ID,
		PublicKey:    HexKey(rawKey),
		Capabilities: req.Capabilities,
	}
	if agent.Capabilities == nil {
		agent.Capabilities = []string{}
	}

	if err := h.store.Register(agent); err != nil {
		if errors.Is(err, ErrAgentExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusCreated, agent)
}

// HandleListAgents handles GET /agents — lists all registered agents.
func (h *Handler) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := h.store.List()
	if agents == nil {
		agents = []*Agent{}
	}
	writeJSON(w, http.StatusOK, agentsResponse{Agents: agents})
}

// HandleGetAgent handles GET /agents/{id} — returns agent detail.
func (h *Handler) HandleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	agent, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// HandleUnregister handles DELETE /agents/{id} — removes an agent.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleUnregister(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}
	if err := h.store.Unregister(id); err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleDeliver handles POST /agents/{id}/inbox — delivers a message to an agent.
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	var req deliverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	msgID := make([]byte, 12)
	rand.Read(msgID)

	entry := &InboxEntry{
		ID:      hex.EncodeToString(msgID),
		Payload: req.Payload,
	}

	if err := h.store.Deliver(id, entry); err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusCreated, deliverResponse{ID: entry.ID})
}

// HandleRetrieve handles GET /agents/{id}/inbox — retrieves leased messages.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleRetrieve(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	maxMsgs := 10
	if v := r.URL.Query().Get("max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMsgs = n
		}
	}
	// Cap max at 100 per spec §9
	if maxMsgs > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max must be <= 100"})
		return
	}

	leaseSecs := 30 * time.Second
	if v := r.URL.Query().Get("lease"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			leaseSecs = time.Duration(n) * time.Second
		}
	}

	messages, leaseID, err := h.store.Retrieve(id, leaseSecs, maxMsgs)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	if messages == nil {
		messages = []*InboxEntry{}
	}

	writeJSON(w, http.StatusOK, retrieveResponse{
		Messages: messages,
		LeaseID:  leaseID,
	})
}

// HandleAck handles POST /agents/{id}/inbox/ack — acknowledges messages.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleAck(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.LeaseID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lease_id is required"})
		return
	}

	if err := h.store.Ack(id, req.LeaseID, req.MessageIDs); err != nil {
		switch {
		case errors.Is(err, ErrAgentNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrLeaseConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrInvalidStoreInput):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		default:
			writeStoreError(w, err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleStats handles GET /agents/{id}/inbox/stats — returns queue stats.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleStats(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	depth, leased, age, err := h.store.Stats(id)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		QueueDepth:  depth,
		LeasedCount: leased,
		OldestAgeMs: age.Milliseconds(),
	})
}

// writeStoreError logs a store-level error and returns a generic 500 to the client.
func writeStoreError(w http.ResponseWriter, err error) {
	slog.Error("registry store error", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "registry storage unavailable",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
