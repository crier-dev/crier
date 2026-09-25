package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/crier-dev/crier/internal/namespace"
)

// TopicInfo describes an active topic and its subscriber count.
type TopicInfo struct {
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
	// Namespace is the realm this topic belongs to (CR-FEAT-029). It is
	// empty for the default namespace, and `omitempty` keeps the /relay/topics
	// body byte-identical to a pre-CR-FEAT-029 response — the same reason the
	// agent and inbox rows spell the default realm as "".
	Namespace string `json:"namespace,omitempty"`
}

// Relay manages publish/subscribe topic routing.
// Thread-safe, zero external dependencies (no Redis, no NATS).
type Relay struct {
	mu sync.RWMutex
	// topic -> set of subscriber channels. Keys are literal topic names and
	// wildcard subscription patterns alike, so Subscribe/Unsubscribe/topic
	// inventory stay a single map.
	//
	// Since CR-FEAT-029 the key is NAMESPACE-SCOPED: subscriptionKey(ns, topic)
	// prefixes the realm, so two realms using the same literal topic name are
	// two disjoint sets of subscribers and a publish in one can never reach a
	// subscriber in the other. For the default namespace the key's prefix is
	// empty and the map is what it always was.
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
	// namespaces is the realm policy set (CR-FEAT-029). Nil — the default,
	// and the only state a deployment that declares no namespaces is in —
	// means one implicit realm whose publish cap is rateLimitPerMinute and
	// whose rate-limit key is the bare agent id, exactly as before.
	namespaces *namespace.Registry
}

// namespaceSep separates the realm from the topic in the relay's internal
// subscription key. It cannot occur in either part: validateTopic rejects the
// NUL byte and a namespace name is restricted to [a-z0-9_-].
const namespaceSep = "\x00"

// subscriptionKey is the relay's internal key for one (realm, topic) pair.
func subscriptionKey(nsName, topic string) string {
	return namespace.Canonical(nsName) + namespaceSep + topic
}

// topicOf strips the realm prefix off an internal subscription key.
func topicOf(key string) string {
	if i := strings.Index(key, namespaceSep); i >= 0 {
		return key[i+len(namespaceSep):]
	}
	return key
}

// namespaceOfKey returns the realm part of an internal subscription key.
func namespaceOfKey(key string) string {
	if i := strings.Index(key, namespaceSep); i >= 0 {
		return key[:i]
	}
	return ""
}

// SetNamespacePolicies wires the realm policy set onto the relay (CR-FEAT-029):
// publish and subscribe are scoped to a realm, and each realm's own rate limit
// (or the deployment's, when it declares none) applies to its publishers. Nil
// restores the single implicit realm — every existing behaviour, unchanged.
func (r *Relay) SetNamespacePolicies(reg *namespace.Registry) {
	r.namespaces = reg
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

// CheckRateLimit checks whether the given agent ID is within the rate limit of
// the DEFAULT namespace. Returns true if the request is allowed, false if rate
// limited. When rate limiting is disabled, always returns true.
func (r *Relay) CheckRateLimit(agentID string) bool {
	return r.CheckRateLimitIn("", agentID)
}

// CheckRateLimitIn checks whether an agent may publish inside a namespace
// (CR-FEAT-029). The cap comes from the namespace's own policy when it
// declares one, else from the deployment's CR_RATE_LIMIT_PER_MINUTE; a cap of 0
// means that namespace is not rate limited at all.
//
// The counter key is (namespace, agent), which is the whole isolation claim:
// two realms' traffic cannot consume each other's budget, so one realm's flood
// cannot rate-limit a quiet realm's agents out of publishing — the "one noisy
// neighbour, one shared fate" failure the review measured.
func (r *Relay) CheckRateLimitIn(nsName, agentID string) bool {
	if r.RateLimiter == nil {
		return true
	}
	limit := r.rateLimitPerMinute
	if pol, ok := r.namespaces.Lookup(nsName); ok {
		limit = pol.RateLimit(r.rateLimitPerMinute)
	}
	if limit <= 0 {
		// A namespace whose effective cap is 0 has rate limiting switched off
		// for its publishers — a deliberate policy value, and the same meaning
		// 0 has deployment-wide.
		return true
	}
	return r.RateLimiter.Allow(subscriptionKey(nsName, agentID), limit, r.rateLimitWindow)
}

// Frame is the JSON object a relay subscription delivers: the LITERAL
// published topic plus the published event, unchanged.
//
// Every subscriber of a publish — exact topic or wildcard pattern — receives
// this same shape, so a wildcard subscriber (which only knows its own pattern)
// can always tell which topic actually matched. The frame is the relay's
// WebSocket wire contract (DOGFOOD-RELAY-1): before it, subscribers received
// the bare event with no topic, which made the matched topic unknowable for
// patterns such as "dogfood.*".
type Frame struct {
	Topic string          `json:"topic"`
	Event json.RawMessage `json:"event"`
}

// buildFrame returns the exact bytes sent to every subscriber of a publish to
// topic: {"topic":"<literal topic>","event":<event>}.
//
// The two fields are spliced in by hand rather than handed to json.Marshal
// precisely so the event stays byte-for-byte the JSON value the publisher sent
// — an object, string, array, number or null — and is never re-encoded as a
// JSON string (no double-encoding) nor re-escaped/compacted by the encoder. The
// topic is JSON-quoted here; topics are validated by validateTopic first.
func buildFrame(topic string, event json.RawMessage) []byte {
	f := Frame{Topic: topic, Event: event}

	quoted, err := json.Marshal(f.Topic)
	if err != nil {
		// json.Marshal of a string cannot fail; keep the frame valid JSON.
		quoted = []byte(`""`)
	}
	frame := make([]byte, 0, len(quoted)+len(f.Event)+len(`{"topic":,"event":}`))
	frame = append(frame, `{"topic":`...)
	frame = append(frame, quoted...)
	frame = append(frame, `,"event":`...)
	frame = append(frame, f.Event...)
	return append(frame, '}')
}

// Publish sends an event to a topic in the DEFAULT namespace. It is the
// pre-CR-FEAT-029 entry point, kept as the whole contract for callers that
// never heard of realms (and for every existing test).
func (r *Relay) Publish(topic string, event json.RawMessage) error {
	return r.PublishIn("", topic, event)
}

// PublishIn sends an event to a topic INSIDE one namespace (CR-FEAT-029).
// Every matching subscriber of that same namespace receives it exactly once:
// exact-topic subscribers via one map lookup, plus every subscription pattern
// (literal or wildcard) of the same namespace that matches. Topic names are
// always literal — wildcard tokens are subscriber-side only, so a publish to
// "demo.*" or "demo.>" is rejected.
//
// Realm isolation is a property of the LOOKUP, not a filter applied afterwards:
// subscribers are stored under a realm-prefixed key, so a publish in realm A
// cannot see — let alone deliver to — a subscriber in realm B even when both
// subscribed to the identical topic name. There is no bridging, no fallback and
// no "default realm" substitution: an unconfigured deployment uses the empty
// prefix and behaves exactly as before.
//
// Subscribers receive the topic-bearing frame built by buildFrame (the topic is
// the LITERAL published topic, without the realm). Returns an error if the
// topic is invalid.
func (r *Relay) PublishIn(nsName, topic string, event json.RawMessage) error {
	if err := validateTopic(topic); err != nil {
		return err
	}
	if event == nil {
		event = json.RawMessage("null")
	}

	// One immutable frame per publish, shared by every subscriber below: the
	// envelope is identical for all of them (the topic is the published one, not
	// the subscription pattern), so it is built and allocated exactly once.
	frame := buildFrame(topic, event)
	key := subscriptionKey(nsName, topic)

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact subscriptions, unchanged from the pre-wildcard hot path.
	// A wildcard key can never equal a valid publish topic — publish topics are
	// literal while pattern keys always carry a wildcard token — so no topic can
	// be delivered twice through this lookup.
	for ch := range r.subs[key] {
		// Non-blocking send: drop if subscriber is slow / full.
		select {
		case ch <- frame:
		default:
		}
	}

	// Wildcard subscriptions: one match per active pattern OF THIS REALM. The
	// realm prefix is compared first, so a pattern in another realm is never
	// even parsed against this topic. The topic segments are split only when a
	// pattern exists, so publishes on a wildcard-free relay keep their original
	// cost.
	if len(r.wildcards) > 0 {
		prefix := namespace.Canonical(nsName) + namespaceSep
		segments := strings.Split(topic, ".")
		for wkey, parsed := range r.wildcards {
			if !strings.HasPrefix(wkey, prefix) {
				continue
			}
			if !parsed.matches(segments) {
				continue
			}
			for ch := range r.subs[wkey] {
				select {
				case ch <- frame:
				default:
				}
			}
		}
	}
	return nil
}

// Subscribe registers a channel to receive frames for a topic name or a
// wildcard subscription pattern in the DEFAULT namespace ("*" = exactly one
// segment, ">" = one or more trailing segments in final position).
//
// Each channel value is a complete wire frame (see Frame): the LITERAL
// published topic plus the event exactly as it was published. The frame is
// what the WebSocket subscriber receives, so a wildcard subscriber can read the
// matched topic off it.
// Call the returned function to unsubscribe.
func (r *Relay) Subscribe(topic string) (<-chan []byte, func()) {
	return r.SubscribeIn("", topic)
}

// SubscribeIn registers a channel for a namespace-scoped subscription
// (CR-FEAT-029). A subscriber in one realm receives only that realm's
// publishes, even on an identical topic name — see PublishIn.
func (r *Relay) SubscribeIn(nsName, topic string) (<-chan []byte, func()) {
	pattern, err := parseSubscriptionPattern(topic)
	if err != nil {
		return closedSubscription()
	}
	return r.subscribePattern(nsName, topic, pattern)
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
// pattern inside a namespace. The pattern is parsed once by the caller
// (Subscribe, SubscribeIn or HandleSubscribe) and carried here, so registration
// never re-parses.
func (r *Relay) subscribePattern(nsName, topic string, pattern subscriptionPattern) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	key := subscriptionKey(nsName, topic)

	r.mu.Lock()
	if r.subs[key] == nil {
		r.subs[key] = make(map[chan []byte]struct{})
	}
	r.subs[key][ch] = struct{}{}
	// Non-literal patterns are indexed for publish-time matching: Subscribe
	// stores the parsed form once so Publish walks only this map (O(active
	// patterns)) instead of re-parsing every key on every event.
	if !pattern.literal {
		r.wildcards[key] = pattern
	}
	r.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if m, ok := r.subs[key]; ok {
				if _, exists := m[ch]; exists {
					delete(m, ch)
					close(ch)
				}
				if len(m) == 0 {
					delete(r.subs, key)
					// The pattern is gone with its last subscriber: publish
					// must stop matching it.
					delete(r.wildcards, key)
				}
			}
		})
	}
	return ch, unsub
}

// Topics returns a list of active topics with subscriber counts, each carrying
// the namespace it belongs to (CR-FEAT-029). The same literal topic name can
// appear more than once — once per realm that has subscribers on it — which is
// exactly the isolation this feature adds, made visible.
func (r *Relay) Topics() []TopicInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]TopicInfo, 0, len(r.subs))
	for key, m := range r.subs {
		out = append(out, TopicInfo{
			Name:        topicOf(key),
			Subscribers: len(m),
			Namespace:   namespaceOfKey(key),
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
