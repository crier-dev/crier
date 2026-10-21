// Package dispatchlive is the in-memory, per-message view of the fan-out
// passes THIS process is running right now (DF-CRIER-414).
//
// Why it exists: a send's DURABLE journal (session.DispatchEvidence) can only
// report what has already RETURNED. The boundary append proves entry to
// dispatch, and receipts land when a delivery finishes — so for the whole
// per-target budget of a blocking webhook (30s by default) the transcript says
// nothing at all about a send that is still in flight. An operator watching the
// composer therefore cannot tell "working" from "wedged", and the first
// evidence of a dead endpoint is the terminal budget_cut.
//
// This registry closes that window and nothing else. It is deliberately NOT a
// second source of truth about delivery:
//
//   - it holds no OUTCOME — only attempts a runtime actually made, and the pass
//     boundaries the caller registered (start, planned targets, budget);
//   - it is never persisted, and it is empty in a restarted process, so it can
//     never outlive the work it describes;
//   - a pass ENDS when its caller returns, so a reader either sees the live
//     pass or the durable record — never an indicator left spinning after the
//     budget (DF-CRIER-414 requirement 5);
//   - every timestamp it reports is the server's own, so no client computes an
//     elapsed time or a countdown it cannot know.
//
// Readers are read-only projections: the session transcript view publishes a
// live pass as `dispatch_live` on the message it belongs to.
package dispatchlive

import (
	"sort"
	"sync"
	"time"
)

// Class is the closed set naming WHY one outbound attempt did not produce an
// answer, from the runtime's own observation (DF-CRIER-414, requirement 2).
// It is deliberately small and independent of the outcome vocabulary: an
// attempt class says an attempt failed, never how a delivery ended. The empty
// class means the endpoint ANSWERED — the runtime is still classifying the
// reply, and the terminal outcome owns that judgement.
type Class string

const (
	// ClassUnreachable: no connection could be established at all — a dial
	// that was refused, had no route, or failed name resolution.
	ClassUnreachable Class = "unreachable"
	// ClassTimeout: the attempt, or the dial behind it, hit its own deadline
	// without an answer.
	ClassTimeout Class = "timeout"
	// ClassRejected: the endpoint answered with an HTTP status the runtime
	// cannot treat as a delivery.
	ClassRejected Class = "rejected"
	// ClassError: any other transport failure (a truncated body, a malformed
	// request the runtime refused to send).
	ClassError Class = "error"
)

// Attempt is one outbound attempt of one target's delivery, as the runtime
// observed it. SessionID and MessageID name the fan-out pass it belongs to —
// the envelope the runtime already carries (session + request/message id), so
// no new correlation identifier is minted.
type Attempt struct {
	SessionID string
	MessageID string
	AgentID   string
	// Attempt is the one-based ordinal of this attempt for this target,
	// counted by the runtime that made it.
	Attempt int
	At      time.Time
	// Status is the endpoint's HTTP status, 0 when no response was received.
	Status int
	// Class is empty when the endpoint answered (see Class).
	Class Class
	// Reason is the transport's own diagnostic. It is retained verbatim so an
	// operator reads the endpoint and the cause, e.g.
	// "dial tcp 127.0.0.1:39999: connect: connection refused".
	Reason string
}

// Target is one planned target of a live pass, with the last attempt the
// runtime reported for it. DeliveryClass distinguishes paths that publish
// webhook attempts from federation/inbox paths that do not. Missing evidence
// alone says nothing about whether a target is waiting or was skipped.
type Target struct {
	Target string `json:"target"`
	// DeliveryClass describes the dispatch path selected by the session handler.
	// It is separate from Class (one returned attempt's diagnostic). Federation
	// and inbox admission do not publish webhook attempt evidence.
	DeliveryClass string `json:"delivery_class,omitempty"`
	// Attempts is how many attempts the runtime has made for this target.
	Attempts int `json:"attempts"`
	// LastAttemptAt is when the last reported attempt returned.
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	// Status is the endpoint's last HTTP status; 0 means no response.
	Status int `json:"status,omitempty"`
	// Class names why the last attempt produced no answer; empty means the
	// endpoint answered and the terminal outcome is still to be recorded.
	Class Class `json:"class,omitempty"`
	// Reason is the last attempt's own diagnostic, retained verbatim.
	Reason string `json:"reason,omitempty"`
}

// Pass is one in-flight fan-out pass as a reader sees it. It carries the
// caller's pass boundaries (when dispatch started, what it is delivering to,
// and the budget it is bounded by) plus what the runtime has done so far.
// ElapsedMs is computed here, at read time, from the server's own clock.
type Pass struct {
	// Attempt is the caller's one-based journal index for this pass — the same
	// number session.DispatchEvidence records, so a reader can join the live
	// view to the durable journal.
	Attempt   int       `json:"attempt"`
	StartedAt time.Time `json:"started_at"`
	ElapsedMs int64     `json:"elapsed_ms"`
	// BudgetMs is the delivery budget this pass is bounded by, as the server
	// configured it. It is the denominator an operator compares ElapsedMs to.
	BudgetMs int      `json:"budget_ms"`
	Targets  []Target `json:"targets"`
}

// Registry holds this process's in-flight passes. The zero value is not
// usable; build one with NewRegistry.
type Registry struct {
	mu     sync.Mutex
	passes map[passKey]*passState
	now    func() time.Time
}

// passKey is the fan-out pass identity: the message being delivered to, in its
// session. A message id is unique per store, and the session is carried so a
// reader can scope a live view to the room it asked about.
type passKey struct{ sessionID, messageID string }

type passState struct {
	attempt   int
	startedAt time.Time
	budgetMs  int
	targets   []string
	ordinal   map[string]int
	classes   map[string]string
	attempts  map[string]Attempt
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{passes: map[passKey]*passState{}, now: func() time.Time { return time.Now().UTC() }}
}

// Begin registers a fan-out pass that is about to run. It is called AFTER the
// durable boundary append, so a live pass always has a durable intent behind
// it: a reader can never see in-flight work for a message the log does not
// have. Targets are the resolved planned set (deduplicated, sorted), and
// budget is the per-target delivery budget in milliseconds.
func (r *Registry) Begin(sessionID, messageID string, attempt int, targets []string, budgetMs int, startedAt time.Time) {
	r.BeginWithClasses(sessionID, messageID, attempt, targets, budgetMs, startedAt, nil)
}

// BeginWithClasses retains a snapshot of the caller's dispatch-path evidence.
// Missing classification stays unknown; it is never inferred from zero attempts.
func (r *Registry) BeginWithClasses(sessionID, messageID string, attempt int, targets []string, budgetMs int, startedAt time.Time, classes map[string]string) {
	if r == nil || sessionID == "" || messageID == "" {
		return
	}
	if startedAt.IsZero() {
		startedAt = r.now()
	}
	planned := dedupeSorted(targets)
	ordinal := make(map[string]int, len(planned))
	classSnapshot := make(map[string]string, len(planned))
	for i, t := range planned {
		ordinal[t] = i
		classSnapshot[t] = classes[t]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.passes[passKey{sessionID, messageID}] = &passState{
		attempt:   attempt,
		startedAt: startedAt,
		budgetMs:  budgetMs,
		targets:   planned,
		ordinal:   ordinal,
		classes:   classSnapshot,
		attempts:  map[string]Attempt{},
	}
}

// Observe records one attempt for a target of a live pass. An attempt for a
// pass this process is not running (or has already ended) is DISCARDED: the
// registry never invents in-flight state, and an attempt the durable journal
// cannot account for must not appear in a read surface.
func (r *Registry) Observe(a Attempt) {
	if r == nil || a.SessionID == "" || a.MessageID == "" || a.AgentID == "" {
		return
	}
	if a.At.IsZero() {
		a.At = r.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pass, ok := r.passes[passKey{a.SessionID, a.MessageID}]
	if !ok {
		return
	}
	if _, planned := pass.ordinal[a.AgentID]; !planned {
		return
	}
	pass.attempts[a.AgentID] = a
}

// End removes a pass. The caller defers it, so the pass disappears when the
// fan-out returns and its outcomes are written back — a reader that polls
// after the budget reads the terminal record instead of a stale indicator.
func (r *Registry) End(sessionID, messageID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.passes, passKey{sessionID, messageID})
}

// Live reports the in-flight pass for one message, or nil when this process is
// not delivering it. The returned view is a copy: a caller cannot mutate the
// registry through it. ElapsedMs is measured against the registry's clock at
// the moment of the read.
func (r *Registry) Live(sessionID, messageID string) *Pass {
	if r == nil || sessionID == "" || messageID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pass, ok := r.passes[passKey{sessionID, messageID}]
	if !ok {
		return nil
	}
	return r.viewLocked(pass)
}

// viewLocked renders one pass. It is called with the lock held so a reader
// never sees a half-updated target set, and it reports every planned target —
// including one nothing has been reported about yet — because the plan is the
// pass's own boundary, not an inference from an attempt.
func (r *Registry) viewLocked(pass *passState) *Pass {
	now := r.now()
	elapsed := now.Sub(pass.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	view := &Pass{
		Attempt:   pass.attempt,
		StartedAt: pass.startedAt,
		ElapsedMs: elapsed.Milliseconds(),
		BudgetMs:  pass.budgetMs,
		Targets:   make([]Target, 0, len(pass.targets)),
	}
	for _, name := range pass.targets {
		target := Target{Target: name, DeliveryClass: pass.classes[name]}
		if attempt, ok := pass.attempts[name]; ok {
			at := attempt.At
			target.Attempts = attempt.Attempt
			target.LastAttemptAt = &at
			target.Status = attempt.Status
			target.Class = attempt.Class
			target.Reason = attempt.Reason
		}
		view.Targets = append(view.Targets, target)
	}
	return view
}

// dedupeSorted returns the targets in a stable, duplicate-free order: the read
// surface must not depend on map iteration or on how a caller happened to
// resolve its audience.
func dedupeSorted(targets []string) []string {
	seen := make(map[string]bool, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
