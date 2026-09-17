package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
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
	mu sync.RWMutex
	// topic -> set of subscriber channels. Keys are literal topic names and
	// wildcard subscription patterns alike, so Subscribe/Unsubscribe/topic
	// inventory stay a single map.
	subs map[string]map[chan []byte]struct{}
	// wildcards holds the parsed form of every non-literal subscription key.
	// A pattern is parsed once at Subscribe time; publish matching only walks
	// this map (O(active patterns)), never re-parsing or touching exact keys.
	wildcards map[string]subscriptionPattern

	// RateLimiter tracks publish rates per agent.
	// nil when rate limiting is disabled (limit=0).
	RateLimiter        *RateLimiter
	rateLimitPerMinute int
	rateLimitWindow    time.Duration
}

// New creates a new Relay ready to serve.
// rateLimitPerMinute is the max publishes per agent per minute.
// Set to 0 to disable rate limiting entirely.
func New(rateLimitPerMinute int) *Relay {
	r := &Relay{
		subs:               make(map[string]map[chan []byte]struct{}),
		wildcards:          make(map[string]subscriptionPattern),
		rateLimitPerMinute: rateLimitPerMinute,
		rateLimitWindow:    time.Minute,
	}
	if rateLimitPerMinute > 0 {
		r.RateLimiter = NewRateLimiter(5 * time.Minute)
	}
	return r
}

// CheckRateLimit checks whether the given agent ID is within the rate limit.
// Returns true if the request is allowed, false if rate limited.
// When rate limiting is disabled, always returns true.
func (r *Relay) CheckRateLimit(agentID string) bool {
	if r.RateLimiter == nil {
		return true
	}
	return r.RateLimiter.Allow(agentID, r.rateLimitPerMinute, r.rateLimitWindow)
}

// Publish sends an event to a topic. Every matching subscriber receives it
// exactly once: exact-topic subscribers via one map lookup, plus every
// subscription pattern (literal or wildcard) that matches. Topic names are
// always literal — wildcard tokens are subscriber-side only, so a publish to
// "demo.*" or "demo.>" is rejected.
// Returns an error if the topic is invalid.
func (r *Relay) Publish(topic string, event json.RawMessage) error {
	if err := validateTopic(topic); err != nil {
		return err
	}
	if event == nil {
		event = json.RawMessage("null")
	}

	// Copy payload so subscribers own independent slices: every fan-out below
	// sends this same immutable copy.
	payload := make([]byte, len(event))
	copy(payload, event)

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact subscriptions, unchanged from the pre-wildcard hot path.
	// A wildcard key can never equal a valid publish topic — publish topics are
	// literal while pattern keys always carry a wildcard token — so no topic can
	// be delivered twice through this lookup.
	for ch := range r.subs[topic] {
		// Non-blocking send: drop if subscriber is slow / full.
		select {
		case ch <- payload:
		default:
		}
	}

	// Wildcard subscriptions: one match per active pattern. The topic segments
	// are split only when a pattern exists, so publishes on a wildcard-free
	// relay keep their original cost.
	if len(r.wildcards) > 0 {
		segments := strings.Split(topic, ".")
		for pattern, parsed := range r.wildcards {
			if !parsed.matches(segments) {
				continue
			}
			for ch := range r.subs[pattern] {
				select {
				case ch <- payload:
				default:
				}
			}
		}
	}
	return nil
}

// Subscribe registers a channel to receive events for a topic name or a
// wildcard subscription pattern ("*" = exactly one segment, ">" = one or more
// trailing segments in final position).
// Returns a channel that receives events. Call the returned function to unsubscribe.
func (r *Relay) Subscribe(topic string) (<-chan []byte, func()) {
	pattern, err := parseSubscriptionPattern(topic)
	if err != nil {
		return closedSubscription()
	}
	return r.subscribePattern(topic, pattern)
}

// closedSubscription is the inert subscription returned for an invalid topic:
// an already-closed channel plus a no-op unsubscribe, so callers can still run
// their cleanup path.
func closedSubscription() (<-chan []byte, func()) {
	ch := make(chan []byte)
	close(ch)
	return ch, func() {}
}

// subscribePattern registers a channel for an already-validated subscription
// pattern. The pattern is parsed once by the caller (Subscribe or
// HandleSubscribe) and carried here, so registration never re-parses.
func (r *Relay) subscribePattern(topic string, pattern subscriptionPattern) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)

	r.mu.Lock()
	if r.subs[topic] == nil {
		r.subs[topic] = make(map[chan []byte]struct{})
	}
	r.subs[topic][ch] = struct{}{}
	// Non-literal patterns are indexed for publish-time matching: Subscribe
	// stores the parsed form once so Publish walks only this map (O(active
	// patterns)) instead of re-parsing every key on every event.
	if !pattern.literal {
		r.wildcards[topic] = pattern
	}
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
					// The pattern is gone with its last subscriber: publish
					// must stop matching it.
					delete(r.wildcards, topic)
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

// SubscriberCount returns the number of live topic subscribers across all
// topics and patterns (DF-CRIER-142). It backs the ws_subscribers gauge in
// cmd/server; a topic's channel is counted once per subscription
// registration, matching how Topics() reports per-topic counts.
func (r *Relay) SubscriberCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, m := range r.subs {
		n += len(m)
	}
	return n
}

// validateTopic checks that a published topic is a non-empty dot-separated name
// of letters, digits, underscore, hyphen, and dots. Publish topics are literal:
// the wildcard tokens "*" and ">" are rejected here, so wildcards apply to
// subscription patterns only (see parseSubscriptionPattern in wildcard.go).
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
