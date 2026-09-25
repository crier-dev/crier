package registry

// Capability-routed delivery (CR-FEAT-026): deliver to a CAPABILITY instead of
// a named agent id, so the registry's capability index becomes a worker pool a
// sender can actually dial.
//
// The gap this closes was measured live by the external hands-on review
// 'DISPATCH · CRI-001' (Carter, 2026-09-24, tested at ca28523d on a scratch
// port with the guard disabled per TESTERS.md §1) and re-verified at HEAD
// 0b60acc before the row was filed: the registry already ADVERTISES
// capabilities and filters by them on GET /agents?capability= (spec §7,
// CR-FEAT-007), but DELIVERY still required naming one agent id — so the
// capability index was a phone book nobody could dial, and a pool of
// interchangeable workers could not be addressed as a pool.
//
// POST /capabilities/{capability}/inbox resolves the capability to ONE holder
// and then runs the EXISTING deliver path unchanged (Handler.deliver): the same
// guard choke point, webhook driver, durable inbox write, lease/ack semantics,
// federation fallback and delivery observation. Nothing about deliver-by-id
// moves — it is the same function with an id that was never resolved, and its
// response body is byte-identical to what it was before this row.
//
// ## The selection rule (this is the whole contract)
//
//   - CANDIDATES are every registered agent whose advertised `capabilities`
//     include the name, matched EXACTLY — the same comparison the discovery
//     filter on GET /agents?capability= uses, so the two surfaces cannot
//     disagree about who holds a capability.
//   - LIVE holders rank first. Liveness is the registry's own derived status
//     (CR-FEAT-024, presence.go): `online` while the row's liveness evidence is
//     inside CR_PRESENCE_STALE_AFTER_S, and a row that STATES `offline` is never
//     live (the explicit statement wins in that derivation). When at least one
//     holder is live the pool is EXACTLY the live holders, so a crashed worker
//     absorbs no work.
//   - ROUND-ROBIN over the pool: one holder per delivery, one cursor per
//     capability, advanced once per resolved delivery. The pool is ordered by
//     agent id ascending — NOT in store iteration order, which for the
//     in-memory backend is Go map order and would make the rotation both
//     untestable and unfair.
//   - NO LIVE HOLDER is not an error and not a drop: the pool falls back to
//     ALL holders. The durable inbox is the reason — the message WAITS for a
//     holder that comes back, and if nobody ever takes it, the existing
//     ownership path (CR-FEAT-025) produces exactly one MESSAGE_EXPIRED
//     receipt in the sender's own inbox. A holder that dies MID-LEASE needs no
//     case of its own either: the lease expires and the message returns to the
//     queue it was delivered to (§4 lease/ack), and an unacknowledged message
//     that expires is reported as the same receipt.
//   - ZERO HOLDERS — nobody advertises the capability — is a NAMED error, never
//     a silent drop: 404 NO_CAPABLE_AGENT naming the capability.
//
// ## Stated limits (a selector that over-claims is the same lie in a new place)
//
//   - Selection is LOCAL to this relay. A capability held only on a LINKED
//     relay is not selected: federation forwards a delivery by AGENT ID
//     (CR-FEAT-006), and widening the selector across relays is a different
//     design (which relay would own the rotation, and over whose liveness
//     view?). The refusal names that limit, so a caller is never left guessing
//     whether "nobody" means "nobody here" or "nobody anywhere".
//   - The cursor lives in the HANDLER, in memory, per process. A restart, a
//     second process on the same store, or two relays behind a load balancer
//     each rotate on their own — this is fairness across holders, not
//     exactly-once dispatch.
//   - Capability routing changes nothing about how an agent registers, how a
//     lease is taken, or how an ack releases it. It chooses the inbox.
//   - The `capability` is a SELECTOR, not a queue: two different capabilities
//     are two pools, and a message is never fanned out to every holder (an
//     all-holders fan-out is a different delivery semantic with different
//     lease/ack consequences, and it is not what this row ships).
//   - Candidates come from the same store.List() the discovery read uses, so a
//     capability delivery costs ONE registry listing — the same O(N) read
//     GET /agents?capability= already performs. No index is added here, and none
//     is claimed.

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/middleware"
)

// ErrNoCapableAgent is the NAMED refusal for a capability-routed delivery when
// no registered agent advertises the capability (CR-FEAT-026). It is a stable
// machine-readable code — the same family as FEDERATION_FAILED and
// AGENT_QUARANTINED — so a caller can branch on it instead of parsing prose,
// and it is deliberately NOT the plain `agent not found` a by-id delivery to an
// unknown id gets: the two say different things, and the caller of a
// capability delivery needs to know which one it hit.
const ErrNoCapableAgent = "NO_CAPABLE_AGENT"

// capabilityRoutedTotal counts capability-routed deliveries that RESOLVED to a
// holder and were dispatched down the existing deliver path (the accept itself
// is counted by deliveries_total, exactly as a by-id delivery is). Refusals are
// counted separately, so an operator can see a pool that has gone empty without
// subtracting two numbers.
var capabilityRoutedTotal = metrics.Default.NewCounter("capability_routed_total",
	"Capability-routed deliveries that resolved to a holder (POST /capabilities/{capability}/inbox).")

// capabilityUnheldTotal counts capability-routed deliveries refused with
// NO_CAPABLE_AGENT: the dial failed because nobody on this relay advertises the
// capability.
var capabilityUnheldTotal = metrics.Default.NewCounter("capability_unheld_total",
	"Capability-routed deliveries refused with NO_CAPABLE_AGENT (no agent on this relay holds the capability).")

// routedTarget returns the holder id an accept should report, and "" for a
// by-id delivery — whose body must stay byte-identical to what it always was
// (CR-FEAT-026 acceptance). Both fields therefore travel together or not at all.
func routedTarget(capability, id string) string {
	if capability == "" {
		return ""
	}
	return id
}

// noCapableAgentResponse is the 404 body of a capability that nobody holds. It
// names the capability it was asked for and states the local scope of the
// selection, so the refusal is diagnosable without reading source.
type noCapableAgentResponse struct {
	Error      string `json:"error"` // always NO_CAPABLE_AGENT
	Capability string `json:"capability"`
	Detail     string `json:"detail"`
}

// noCapableAgentDetail is the explanation carried by the named refusal. It
// states BOTH facts a caller needs: nothing on this relay holds the capability,
// and selection is local (so "nobody here" is not "nobody anywhere").
func noCapableAgentDetail(capability string) string {
	return fmt.Sprintf("no agent registered on this relay advertises capability %q "+
		"(capability selection is local to the relay; federation forwards deliveries by agent id)", capability)
}

// capabilityHolders returns every registered agent that advertises capability,
// ordered by agent id ascending. A nil/empty capability, or a nil store, has no
// holders — a zero-value Handler answers the named refusal instead of panicking.
//
// The ordering is established HERE rather than trusted from the store: the
// in-memory store returns Go map order, which differs between calls, so a
// round-robin built on it would be neither reproducible in a test nor fair
// between holders.
func (h *Handler) capabilityHolders(capability string) []*Agent {
	if h == nil || h.store == nil || capability == "" {
		return nil
	}
	agents := h.store.List()
	holders := make([]*Agent, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		for _, c := range a.Capabilities {
			if c == capability {
				holders = append(holders, a)
				break
			}
		}
	}
	if len(holders) == 0 {
		return nil
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].ID < holders[j].ID })
	return holders
}

// selectCapabilityHolder picks the holder a capability-routed delivery goes to.
// It reports live=true when the pool was the live holders (the normal case) and
// live=false when it fell back to all holders because none was live; ok=false
// means nobody holds the capability at all, which the caller answers with the
// named NO_CAPABLE_AGENT refusal.
//
// One call per delivery, and the cursor advances only when a holder was
// actually chosen — a refusal does not consume a turn in the rotation.
func (h *Handler) selectCapabilityHolder(capability string, now time.Time) (holder *Agent, live bool, ok bool) {
	holders := h.capabilityHolders(capability)
	if len(holders) == 0 {
		return nil, false, false
	}
	liveHolders := make([]*Agent, 0, len(holders))
	for _, a := range holders {
		if h.presence.StatusOf(a, now) == StatusOnline {
			liveHolders = append(liveHolders, a)
		}
	}
	if len(liveHolders) > 0 {
		return liveHolders[h.nextCapabilityIndex(capability, len(liveHolders))], true, true
	}
	// No holder is live: the delivery is still accepted, because the inbox is
	// durable and a holder that comes back finds its work — and a message that
	// is never taken produces the sender's MESSAGE_EXPIRED receipt rather than
	// a silent drop (CR-FEAT-025).
	return holders[h.nextCapabilityIndex(capability, len(holders))], false, true
}

// nextCapabilityIndex returns the rotation slot for the next delivery to
// capability out of a pool of n, and advances the capability's cursor. Safe for
// concurrent callers; the cursor map is created on first use, so a Handler that
// was never built by NewHandler (a zero-value one in a test) still rotates
// instead of panicking.
func (h *Handler) nextCapabilityIndex(capability string, n int) int {
	if n <= 0 {
		return 0
	}
	h.capMu.Lock()
	defer h.capMu.Unlock()
	if h.capCursors == nil {
		h.capCursors = make(map[string]uint64)
	}
	index := int(h.capCursors[capability] % uint64(n))
	h.capCursors[capability]++
	return index
}

// routeCapability resolves a capability-routed delivery to a holder id.
//
// It is called from the deliver path AFTER the sender-side containment check and
// AFTER the idempotency key has been resolved — deliberately, in that order:
//
//   - a REPLAY must be answered from the recorded receipt even if the pool has
//     since emptied, and it must not consume a turn in the rotation, so the
//     registry is not consulted for it at all;
//   - a 409 (another delivery under the same key in flight) is likewise
//     answered without picking a holder;
//   - the resolution therefore happens only for a delivery that is actually
//     going to be dispatched, and the cursor advances exactly once per
//     dispatched capability delivery.
//
// ok=false means the named refusal was already written to w.
func (h *Handler) routeCapability(w http.ResponseWriter, r *http.Request, capability string) (id string, ok bool) {
	holder, live, found := h.selectCapabilityHolder(capability, time.Now())
	if !found {
		capabilityUnheldTotal.Inc()
		slog.Info("capability deliver refused",
			"capability", capability,
			"error", ErrNoCapableAgent,
			"request_id", middleware.RequestIDFromContext(r.Context()),
		)
		writeJSON(w, http.StatusNotFound, noCapableAgentResponse{
			Error:      ErrNoCapableAgent,
			Capability: capability,
			Detail:     noCapableAgentDetail(capability),
		})
		return "", false
	}
	capabilityRoutedTotal.Inc()
	slog.Info("capability deliver routed",
		"capability", capability,
		"target", holder.ID,
		"live", live,
		"request_id", middleware.RequestIDFromContext(r.Context()),
	)
	return holder.ID, true
}
