package relay

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Agents may connect from any origin in v0.1.0.
	CheckOrigin: func(r *http.Request) bool { return true },
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
// Returns 202 on success, 429 when rate limited.
func (r *Relay) HandlePublish(w http.ResponseWriter, req *http.Request) {
	// Rate limiting: check before parsing body to avoid wasted work.
	if r.RateLimiter != nil {
		agentID := req.Header.Get("X-Agent-ID")
		if agentID == "" {
			agentID = req.RemoteAddr
		}
		if !r.CheckRateLimit(agentID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
			return
		}
	}

	var body publishRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if body.Topic == "" {
		http.Error(w, `{"error":"topic is required"}`, http.StatusBadRequest)
		return
	}
	if len(body.Event) == 0 {
		http.Error(w, `{"error":"event is required"}`, http.StatusBadRequest)
		return
	}

	if err := r.Publish(body.Topic, body.Event); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// HandleSubscribe upgrades to WebSocket and streams events for the path topic.
func (r *Relay) HandleSubscribe(w http.ResponseWriter, req *http.Request) {
	topic := mux.Vars(req)["topic"]
	if err := validateTopic(topic); err != nil {
		http.Error(w, `{"error":"invalid topic"}`, http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		// Upgrade already wrote an error response when possible.
		return
	}
	defer conn.Close()

	events, unsub := r.Subscribe(topic)
	defer unsub()

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
