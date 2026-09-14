package relay

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/crier-dev/crier/internal/middleware"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Default: allow all origins. Override with SetWSCheckOrigin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// SetWSCheckOrigin replaces the WebSocket upgrader's CheckOrigin for relay subscriptions.
func SetWSCheckOrigin(fn func(r *http.Request) bool) {
	upgrader.CheckOrigin = fn
}

// publishRequest is the JSON body for POST /relay/publish.
type publishRequest struct {
	Topic string          `json:"topic"`
	Event json.RawMessage `json:"event"`
}

// topicsResponse is the JSON body for GET /relay/topics.
type topicsResponse struct {
	Topics []TopicInfo `json:"topics"`
}

// HandlePublish accepts {"topic": "...", "event": {...}} and fans out to subscribers.
// Returns 202 on success, 401 when X-Agent-ID is missing (rate limiting enabled),
// 429 when rate limited.
//
// Every outcome is logged with the request's correlation id (DF-CRIER-141):
// an accepted publish at info (topic, envelope id, sender, request id), a
// rejection at warn with the reason. The event body is never logged.
func (r *Relay) HandlePublish(w http.ResponseWriter, req *http.Request) {
	rid := middleware.RequestIDFromContext(req.Context())
	agentID := req.Header.Get("X-Agent-ID")

	// Rate limiting: check before parsing body to avoid wasted work.
	if r.RateLimiter != nil {
		if agentID == "" {
			slog.Warn("relay: publish rejected", "reason", "missing X-Agent-ID header",
				"request_id", rid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "X-Agent-ID header required for rate-limited publish"})
			return
		}
		if !r.CheckRateLimit(agentID) {
			slog.Warn("relay: publish rejected", "reason", "rate limit exceeded",
				"sender", agentID, "request_id", rid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
			return
		}
	}

	var body publishRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		slog.Warn("relay: publish rejected", "reason", "invalid json",
			"sender", agentID, "request_id", rid)
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Topic == "" {
		slog.Warn("relay: publish rejected", "reason", "topic is required",
			"sender", agentID, "request_id", rid)
		http.Error(w, `{"error":"topic is required"}`, http.StatusBadRequest)
		return
	}
	if len(body.Event) == 0 {
		slog.Warn("relay: publish rejected", "reason", "event is required",
			"topic", body.Topic, "sender", agentID, "request_id", rid)
		http.Error(w, `{"error":"event is required"}`, http.StatusBadRequest)
		return
	}

	if err := r.Publish(body.Topic, body.Event); err != nil {
		slog.Warn("relay: publish rejected", "reason", err.Error(),
			"topic", body.Topic, "sender", agentID, "request_id", rid)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	slog.Info("relay: publish accepted",
		"topic", body.Topic,
		"message_id", eventID(body.Event),
		"sender", agentID,
		"bytes", len(body.Event),
		"request_id", rid,
	)
	w.WriteHeader(http.StatusAccepted)
}

// eventID extracts a best-effort envelope id from a published event body: the
// conventional "message_id" or "id" field when the event is a JSON object,
// empty otherwise. Publish itself carries no id on the wire, so the log line
// reports whichever identifier the event already advertises.
func eventID(event json.RawMessage) string {
	if len(event) == 0 {
		return ""
	}
	var probe struct {
		MessageID string `json:"message_id"`
		ID        string `json:"id"`
	}
	if err := json.Unmarshal(event, &probe); err != nil {
		return ""
	}
	if probe.MessageID != "" {
		return probe.MessageID
	}
	return probe.ID
}

// HandleSubscribe upgrades to WebSocket and streams events for the path topic.
// The path accepts a literal topic name or a wildcard subscription pattern
// ("*" = exactly one segment, ">" = one or more trailing segments in final
// position). The pattern is validated before the upgrade, so an invalid
// subscription is refused with HTTP 400 and never holds a socket.
//
// The subscribe/unsubscribe lifecycle is logged (DF-CRIER-141): connect at
// info, disconnect at debug, both carrying the topic pattern, the subscriber's
// X-Agent-ID when the client sent one, and the request id.
func (r *Relay) HandleSubscribe(w http.ResponseWriter, req *http.Request) {
	topic := mux.Vars(req)["topic"]
	rid := middleware.RequestIDFromContext(req.Context())
	subscriber := req.Header.Get("X-Agent-ID")

	pattern, err := parseSubscriptionPattern(topic)
	if err != nil {
		slog.Warn("relay: subscribe rejected", "reason", "invalid topic",
			"topic", topic, "agent_id", subscriber, "request_id", rid)
		http.Error(w, `{"error":"invalid topic"}`, http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		// Upgrade already wrote an error response when possible.
		slog.Debug("relay: subscribe upgrade failed", "topic", topic,
			"agent_id", subscriber, "error", err, "request_id", rid)
		return
	}
	defer conn.Close()

	events, unsub := r.subscribePattern(topic, pattern)
	defer func() {
		unsub()
		slog.Debug("relay: subscriber disconnected", "topic", topic,
			"agent_id", subscriber, "request_id", rid)
	}()

	slog.Info("relay: subscriber connected", "topic", topic,
		"agent_id", subscriber, "request_id", rid)

	// Detect client disconnect via read pump.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case payload, ok := <-events:
			if !ok {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}
}

// HandleTopics returns active topics with subscriber counts.
func (r *Relay) HandleTopics(w http.ResponseWriter, req *http.Request) {
	topics := r.Topics()
	if topics == nil {
		topics = []TopicInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(topicsResponse{Topics: topics})
}
