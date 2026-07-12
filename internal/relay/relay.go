package relay

import (
	"encoding/json"
	"fmt"
	"sync"
	"unicode"
)

// TopicInfo describes an active topic and its subscriber count.
type TopicInfo struct {
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
}

// Relay manages publish/subscribe topic routing.
// Thread-safe, zero external dependencies (no Redis, no NATS).
type Relay struct {
	mu   sync.RWMutex
	// topic -> set of subscriber channels
	subs map[string]map[chan []byte]struct{}
}

// New creates a new Relay ready to serve.
func New() *Relay {
	return &Relay{
		subs: make(map[string]map[chan []byte]struct{}),
	}
}

// Publish sends an event to a topic. All subscribers receive it.
// Returns an error if the topic is invalid.
func (r *Relay) Publish(topic string, event json.RawMessage) error {
	if err := validateTopic(topic); err != nil {
		return err
	}
	if event == nil {
		event = json.RawMessage("null")
	}

	// Copy payload so subscribers own independent slices.
	payload := make([]byte, len(event))
	copy(payload, event)

	r.mu.RLock()
	defer r.mu.RUnlock()

	for ch := range r.subs[topic] {
		// Non-blocking send: drop if subscriber is slow / full.
		select {
		case ch <- payload:
		default:
		}
	}
	return nil
}

// Subscribe registers a channel to receive events for a topic.
// Returns a channel that receives events. Call the returned function to unsubscribe.
func (r *Relay) Subscribe(topic string) (<-chan []byte, func()) {
	if err := validateTopic(topic); err != nil {
		// Invalid topic: closed channel + no-op unsubscribe so callers can still clean up.
		ch := make(chan []byte)
		close(ch)
		return ch, func() {}
	}

	ch := make(chan []byte, 64)

	r.mu.Lock()
	if r.subs[topic] == nil {
		r.subs[topic] = make(map[chan []byte]struct{})
	}
	r.subs[topic][ch] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if m, ok := r.subs[topic]; ok {
				if _, exists := m[ch]; exists {
					delete(m, ch)
					close(ch)
				}
				if len(m) == 0 {
					delete(r.subs, topic)
				}
			}
		})
	}
	return ch, unsub
}

// Topics returns a list of active topics with subscriber counts.
func (r *Relay) Topics() []TopicInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]TopicInfo, 0, len(r.subs))
	for name, m := range r.subs {
		out = append(out, TopicInfo{
			Name:        name,
			Subscribers: len(m),
		})
	}
	return out
}

// validateTopic checks that a topic is a non-empty dot-separated name
// of letters, digits, underscore, hyphen, and dots (exact match only in v0.1.0).
func validateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("topic is required")
	}
	if len(topic) > 256 {
		return fmt.Errorf("topic too long")
	}
	// Disallow leading/trailing dots and empty segments.
	if topic[0] == '.' || topic[len(topic)-1] == '.' {
		return fmt.Errorf("invalid topic: %q", topic)
	}
	prevDot := false
	for _, r := range topic {
		if r == '.' {
			if prevDot {
				return fmt.Errorf("invalid topic: %q", topic)
			}
			prevDot = true
			continue
		}
		prevDot = false
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("invalid topic: %q", topic)
	}
	return nil
}
