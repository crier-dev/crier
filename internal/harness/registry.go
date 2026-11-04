package harness

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultTimeout bounds one delivery when config does not say (900s, §4.1).
const DefaultTimeout = 900 * time.Second

// DefaultMaxConcurrent bounds in-flight deliveries when config does not say.
const DefaultMaxConcurrent = 4

// NudgeHint is the session-start hint's wiring (CRIER-MCP-NUDGE, §2.2): when
// SessionStart is true and Counts is set, Deliver prepends nudgePrefix to the
// message text it hands the adapter.
//
// It is a hook rather than a store dependency because the Crier stores the
// numbers live in (inboxes + read cursors) are not the harness package's to
// know. The default — the zero value — is OFF: no hook, no prefix, and the
// delivered text is byte-identical to a build without this feature.
type NudgeHint struct {
	// SessionStart is mcp.nudge.session_start. Default false: a prefix
	// changes the agent's prompt, and that is a per-deployment product
	// decision, not a default.
	SessionStart bool
	// MaxCount is mcp.nudge.max_count. Zero means NudgeMaxCountDefault.
	MaxCount int
	// Counts answers (total unread, categories) for the message about to be
	// delivered. Nil with SessionStart true delivers no prefix.
	Counts func(ctx context.Context, msg Message) (total int, categories []string)
}

// RegistryOptions is what a caller wires from config. Zero values take the
// documented defaults, so a Registry built with no options still behaves.
type RegistryOptions struct {
	// Enabled is harness.enabled. When false, Deliver refuses with
	// HARNESS_DISABLED and every existing code path behaves as it did before
	// this feature existed.
	Enabled bool
	// Default is harness.default: the adapter name used when a request does
	// not name one.
	Default string
	// Timeout is the per-delivery budget (harness.timeout_seconds).
	Timeout time.Duration
	// MaxConcurrent is harness.max_concurrent.
	MaxConcurrent int
	// Now is the clock for Receipt.At; nil means time.Now.
	Now func() time.Time
	// KnownUnavailable names adapters whose config block was accepted but
	// whose implementation is a later row (pi, codex). They are registered
	// with a Capability that says so, so a caller never sees "unknown".
	KnownUnavailable map[string]string
	// Nudge is the session-start unread hint (CRIER-MCP-NUDGE). The zero
	// value is OFF, which is the shipped default.
	Nudge NudgeHint
}

// Registry resolves a harness name to an adapter, serialises deliveries per
// Crier session and bounds how many run at once (§6).
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
	disabled map[string]string

	enabled   bool
	def       string
	timeout   time.Duration
	sem       chan struct{}
	perSess   keyedMutex
	now       func() time.Time
	nudge     NudgeHint
	delivered uint64
}

// DispatchRequest is the ingress hook's call shape (CRIER-INGRESS §4.2).
type DispatchRequest struct {
	// Harness names the adapter; empty means the configured default.
	Harness string
	// SessionRef carries the harness binding recorded on the Crier session.
	SessionRef SessionRef
	// Message is the stored message; its SessionID serialises delivery.
	Message Message
}

// NewRegistry builds an empty registry from config.
func NewRegistry(opts RegistryOptions) *Registry {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	r := &Registry{
		adapters: make(map[string]Adapter),
		disabled: make(map[string]string),
		enabled:  opts.Enabled,
		def:      opts.Default,
		timeout:  timeout,
		sem:      make(chan struct{}, maxConcurrent),
		now:      now,
		nudge:    opts.Nudge,
	}
	for name, reason := range opts.KnownUnavailable {
		r.adapters[name] = unavailableAdapter{name: name, reason: reason}
	}
	return r
}

// Register adds an adapter. A later registration of the same name replaces the
// earlier one, which is what a probe-time override needs; the registry never
// panics on a duplicate.
func (r *Registry) Register(a Adapter) {
	if a == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get resolves a harness name to an adapter.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Names lists the registered adapter names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultName is the configured default adapter ("" when none).
func (r *Registry) DefaultName() string { return r.def }

// Enabled reports whether delivery is switched on.
func (r *Registry) Enabled() bool { return r.enabled }

// Capabilities probes every registered adapter. Plain Get is used inside so a
// slow adapter cannot block registration or reading.
func (r *Registry) Capabilities(ctx context.Context) map[string]Capability {
	r.mu.RLock()
	adapters := make(map[string]Adapter, len(r.adapters))
	for name, a := range r.adapters {
		adapters[name] = a
	}
	r.mu.RUnlock()

	out := make(map[string]Capability, len(adapters))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, a := range adapters {
		wg.Add(1)
		go func(name string, a Adapter) {
			defer wg.Done()
			cap, err := a.Probe(ctx)
			if err != nil && cap.Reason == "" {
				cap.Reason = err.Error()
			}
			if cap.Features == nil {
				cap.Features = map[string]bool{}
			}
			mu.Lock()
			out[name] = cap
			mu.Unlock()
		}(name, a)
	}
	wg.Wait()
	return out
}

// Probe measures one adapter by name. An unregistered name is HARNESS_UNKNOWN.
func (r *Registry) Probe(ctx context.Context, name string) (Capability, error) {
	a, ok := r.Get(name)
	if !ok {
		return Capability{Reason: "unknown harness " + name}, fmt.Errorf("%s: %q", CodeUnknown, name)
	}
	return a.Probe(ctx)
}

// Deliver is the exported hook CRIER-INGRESS §4.2 calls after the message is
// durably appended. It never returns an error: a delivery problem is a
// Receipt with State=failed, because storage already succeeded and the HTTP
// status must not change.
//
// Every completed call is counted exactly once (CRIER-OBS-1): the deferred
// observation runs on ALL return paths — the refusals this body can take and
// the adapter's receipt alike — so a failed dispatch is a counter with its
// error class, not only a log line. The series live in metrics.go; the
// returned Receipt is untouched either way, and the observation reads the
// registry's own clock so an injected clock stays deterministic.
func (r *Registry) Deliver(ctx context.Context, req DispatchRequest) (receipt Receipt) {
	start := r.now()
	defer func() { observeDispatchMetrics(r.now().Sub(start), receipt) }()
	if !r.enabled {
		return FailedReceipt(req.Harness, CodeDisabled, "harness.enabled is false").withTime(r.now())
	}
	name := req.Harness
	if name == "" {
		name = r.def
	}
	if name == "" {
		return FailedReceipt("", CodeUnknown, "no harness named and no harness.default configured").withTime(r.now())
	}
	adapter, ok := r.Get(name)
	if !ok {
		return FailedReceipt(name, CodeUnknown, "no adapter registered for "+name).withTime(r.now())
	}

	// One goroutine per delivery, each with its own deadline (§4.2). The
	// session lock comes FIRST: waiting for a session must not consume a
	// concurrency slot that the running turn needs.
	unlock := r.perSess.lock(sessionKey(name, req.Message))
	defer unlock()

	deliveryCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-deliveryCtx.Done():
		return FailedReceipt(name, CodeConcurrencyFull,
			fmt.Sprintf("max_concurrent saturated for %s", r.timeout)).withTime(r.now())
	}

	sess := req.SessionRef
	sess.Harness = name
	msg := r.applyNudgeHint(deliveryCtx, req.Message)
	rc, err := adapter.Deliver(deliveryCtx, sess, msg)
	if err != nil {
		// A returned error is reserved for "the call could not be attempted";
		// the harness-side vocabulary has no better word for it than
		// unavailable.
		rc = FailedReceipt(name, CodeUnavailable, err.Error())
	}
	if rc.Harness == "" {
		rc.Harness = name
	}
	if rc.State == "" {
		rc.State = StateFailed
	}
	if rc.SessionID == "" && !rc.Created {
		rc.SessionID = sess.ID
	}
	if rc.DurationMS == 0 {
		rc.DurationMS = r.now().Sub(start).Milliseconds()
	}
	if rc.At.IsZero() {
		rc.At = r.now().UTC()
	}
	r.mu.Lock()
	r.delivered++
	r.mu.Unlock()
	return rc
}

// DeliveredCount reports how many deliveries this registry has completed.
// It exists for tests and metrics; it is not a durable counter.
func (r *Registry) DeliveredCount() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.delivered
}

// sessionKey serialises deliveries per Crier session — a message without a
// session still gets its own key from its message id so two retries of the
// same message cannot overlap either.
func sessionKey(harnessName string, msg Message) string {
	id := msg.SessionID
	if id == "" {
		id = "msg:" + msg.ID
	}
	return harnessName + "\x00" + id
}

// keyedMutex is a map of per-key mutexes with reference counting, so the map
// does not grow with every session the relay has ever seen.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until key is free and returns the release func.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedEntry)
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// unavailableAdapter stands in for a harness whose config block is accepted
// but whose adapter is a later row (pi: CRIER-HARNESS-2, codex:
// CRIER-HARNESS-3). It never runs anything.
type unavailableAdapter struct {
	name   string
	reason string
}

func (a unavailableAdapter) Name() string { return a.name }

func (a unavailableAdapter) Probe(context.Context) (Capability, error) {
	return Capability{
		Available: false,
		Reason:    a.reason,
		Features:  map[string]bool{"create": false, "continue": false},
	}, nil
}

func (a unavailableAdapter) Deliver(ctx context.Context, sess SessionRef, msg Message) (Receipt, error) {
	return FailedReceipt(a.name, CodeUnavailable, a.reason), nil
}
