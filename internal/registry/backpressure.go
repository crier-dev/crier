package registry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/ratelimit"
)

// CR-FEAT-035: priority lanes and real backpressure.
//
// Three gaps the external review (DISPATCH · CRI-001, Carter, via Bane
// 2026-09-25) named in passing and which this file closes:
//
//  1. NO PRIORITY LANES. Every message waited its turn behind every other one,
//     so a steady stream of low-value traffic starved an urgent message. The
//     `priority` field (docs/openapi.yaml, POST /agents/{id}/inbox) now orders
//     an agent's inbox; absent, it is exactly the FIFO queue it always was
//     (see InboxEntry.Priority and MemoryStore.Retrieve / PostgresStore.
//     Retrieve).
//  2. NO GLOBAL BACKPRESSURE. The per-agent publish cap on the relay lane
//     (CR_RATE_LIMIT_PER_MINUTE) was the ONLY thing that ever refused a
//     request, and the inbox ingest — the path that actually grows a queue —
//     had none at all: a runaway producer either got 429ed per agent id or
//     pushed every inbox deeper with nothing to stop it. The budget below is a
//     GLOBAL shed on that ingest.
//  3. INVISIBLE DEPTH. Nothing reported how much was queued, so an operator
//     could not tell a healthy relay from one 40 000 messages behind. QueueDepth
//     is that number, surfaced at GET /status and GET /metrics.
//
// It is deliberately opt-in: with CR_RATE_LIMIT_GLOBAL_PER_MINUTE unset the
// budget does not exist, no delivery is ever refused by it, and the delivery
// path behaves exactly as it did before this file existed. The report's own
// wording is the contract — "MUST NOT change existing behaviour when unused".

const (
	// MinMessagePriority and MaxMessagePriority bound the documented
	// `priority` field (docs/openapi.yaml: integer, minimum 0, maximum 9,
	// default 0). A value outside the range is a 400 at the HTTP boundary,
	// never a silent clamp: a delivery that asked for a priority the store
	// cannot honor is a caller bug, and clamping it would hide the bug while
	// still changing when the message is read.
	MinMessagePriority = 0
	MaxMessagePriority = 9

	// globalRateLimitKey is the single key the global ingest budget is counted
	// under. It is a named constant rather than an inline literal because
	// CR-FEAT-029 replaces it with one key per namespace; the counting and
	// shedding logic around it does not change when that happens.
	globalRateLimitKey = "global"

	// globalRateLimitScope is what the shed names itself as, on the wire and on
	// the metric.
	globalRateLimitScope = "global"

	// globalRateLimitWindow is the period the budget is stated over — the same
	// one minute the per-agent publish cap uses, so both caps can be read
	// against one clock.
	globalRateLimitWindow = time.Minute

	// globalRateLimitCleanup is the sweep interval of the budget's window
	// (conservative: the window is one minute).
	globalRateLimitCleanup = 5 * time.Minute
)

// globalRateLimitShedError is the NAMED error a shed delivery is refused with.
// The review's complaint about the existing 429 was that it carried no
// `Retry-After` and no machine-readable reason; this constant is the reason, so
// a client can branch on the code instead of pattern-matching prose (the same
// shape GUARD_BLOCKED, AGENT_QUARANTINED and FEDERATION_FAILED already use).
const globalRateLimitShedError = "RATE_LIMITED_GLOBAL"

// inboxShedTotal counts deliveries the ingest budget refused, by scope. The
// label exists so CR-FEAT-029 can add per-namespace series without a rename.
var inboxShedTotal = metrics.Default.NewCounterVec("inbox_shed_total",
	"Deliveries refused by the inbox ingest budget before any transport or store work, by scope.", "scope")

// shedResponse is the wire body of a shed delivery: 429 with a named error, the
// scope that refused it, the budget in force and the same wait the Retry-After
// header carries (so a client that logs only the body still learns the backoff).
type shedResponse struct {
	Error          string `json:"error"`
	Scope          string `json:"scope"`
	LimitPerMinute int    `json:"limit_per_minute"`
	RetryAfterS    int    `json:"retry_after_s"`
}

// SetGlobalRateLimit installs the global inbox-ingest budget, in deliveries per
// minute (CR_RATE_LIMIT_GLOBAL_PER_MINUTE). 0 — the default — disables it
// entirely: no limiter is created, no delivery is shed, and the ingest path is
// byte-identical to a build without this feature.
//
// It is a boot-time setter (the same shape as SetPresence / SetGuardFilter) and
// is meant to be called ONCE per process: calling it with a positive limit
// again replaces the limiter and its cleanup goroutine.
func (h *Handler) SetGlobalRateLimit(limitPerMinute int) {
	h.globalRateLimit = limitPerMinute
	if limitPerMinute > 0 {
		h.globalLimiter = ratelimit.NewWindow(globalRateLimitCleanup)
		return
	}
	h.globalLimiter = nil
}

// GlobalRateLimit reports the budget in force (0 = disabled). GET /status
// reports it next to the per-agent publish cap, so the two are never confused.
func (h *Handler) GlobalRateLimit() int {
	return h.globalRateLimit
}

// shedGlobal enforces the global ingest budget, writing the 429 and reporting
// true when this delivery is refused. It is called AFTER the request has been
// decoded and validated and after containment — so a malformed request still
// learns it is malformed, and a contained agent is still answered
// 403 AGENT_QUARANTINED rather than a 429 that would mask a security decision —
// and BEFORE idempotency, federation forwarding, the guard LLM call and the
// store write, which are the work a shed exists to avoid doing.
//
// Nil limiter (the default) means the budget does not exist and nothing is ever
// shed.
func (h *Handler) shedGlobal(w http.ResponseWriter) bool {
	if h.globalLimiter == nil {
		return false
	}
	if h.globalLimiter.Allow(globalRateLimitKey, h.globalRateLimit, globalRateLimitWindow) {
		return false
	}

	retryAfterS := ratelimit.HeaderSeconds(
		h.globalLimiter.RetryAfter(globalRateLimitKey, h.globalRateLimit, globalRateLimitWindow))
	inboxShedTotal.With(globalRateLimitScope).Inc()
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterS))
	writeJSON(w, http.StatusTooManyRequests, shedResponse{
		Error:          globalRateLimitShedError,
		Scope:          globalRateLimitScope,
		LimitPerMinute: h.globalRateLimit,
		RetryAfterS:    retryAfterS,
	})
	return true
}

// priorityOrDefault resolves the delivery's requested priority: the documented
// default (MinMessagePriority) when the request named none, else the caller's
// value — already range-checked by validateDeliverParameters, so nothing here
// clamps.
func priorityOrDefault(requested *int) int {
	if requested == nil {
		return MinMessagePriority
	}
	return *requested
}

// QueueDepth is the live inbox queue accounting across the whole store
// (CR-FEAT-035): how much is waiting, how much of it is currently held under a
// lease, and how old the oldest waiting message is. It is a MEASUREMENT, never a
// configuration value — the counterpart of the per-agent counters GET
// /agents/{id}/inbox/stats has always reported, summed over every inbox.
type QueueDepth struct {
	// Pending is the number of un-acknowledged, un-expired messages across all
	// inboxes (leased messages included: a leased message is still queued).
	Pending int
	// Leased is how many of Pending are currently held under a live lease.
	Leased int
	// OldestAge is how long the oldest Pending message has been waiting (0
	// when nothing is queued).
	OldestAge time.Duration
}

// DepthReporter is an optional Store capability: the store-wide queue depth
// above. It is optional for the same reason PurgeReporter and DeadLetterStore
// are — a store that cannot answer (a remote proxy with no such route) must
// say so rather than report a fabricated zero, so the surfaces that read it
// (/status, /metrics) degrade to "unknown" instead of to a reassuring number.
type DepthReporter interface {
	QueueDepth() (QueueDepth, error)
}
