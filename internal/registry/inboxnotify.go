package registry

// Long-poll + new-message ping (CR-FEAT-023).
//
// The durable inbox lane used to be strictly poll-only: GET /agents/{id}/inbox
// took `limit` and `lease_seconds` and answered with whatever was claimable at
// that instant. That made the one lane promising DURABILITY the one lane that
// could not wake a sleeping agent — a 60s poll loop learned about urgent work
// up to a minute late, and a rarely-polling agent may as well have been
// offline (external review 'DISPATCH · CRI-001' by Carter, re-measured at HEAD
// before this landed).
//
// Two ADDITIVE surfaces close that gap without touching poll-only semantics:
//
//   - `?wait_seconds=N` parks the read until a message is claimable and then
//     answers with that batch, or answers the SAME empty body a poll-only read
//     gets when the budget expires — the long-poll every agent framework
//     already understands (retrieveWithWait below).
//   - InboxPinger: a lightweight new-message ping fired after a delivery lands
//     in an inbox, for agents that cannot afford to hold an HTTP long-poll
//     open. The shipped implementation is a tick on the agent's EXISTING mesh
//     socket (internal/mesh.Mesh.PingInbox, opt-in per connection); a handler
//     with no pinger wired keeps the poll-only + long-poll behaviour alone.
//
// Lease, ack and TTL behaviour is untouched by all of this: a long-poll is a
// sequence of ordinary store retrieves, so every message it returns is leased
// by exactly the same rule a poll-only read applies.

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// maxWaitSeconds bounds the long-poll budget. It is the same ceiling the
// request-level blocking deliver budget uses (maxDeliverTimeoutMs = 120000ms),
// so the two request-level waits of this API cannot disagree about how long a
// caller may ask the server to hold a request open.
const maxWaitSeconds = 120

// waitFallbackInterval is how often a parked long-poll re-reads the inbox when
// no wake-up arrives. It exists because not every inbox write goes through this
// handler's HandleDeliver: the federation hold queue and the webhook-failure
// notices write into a store DIRECTLY, and a second relay process can share the
// same PostgreSQL store. The handler-mediated path (a normal delivery) wakes a
// parked read immediately, so this interval only bounds those out-of-band
// writers — one cheap read per second per parked request.
const waitFallbackInterval = time.Second

// InboxPinger is the optional new-message ping (CR-FEAT-023): a lightweight
// nudge telling an agent that a message has landed in its durable inbox, for
// agents that cannot hold an HTTP long-poll open. PingInbox returns true when
// the ping was handed to a live, subscribed transport, false when it was not
// (no connection, no opt-in, a failing transport) — a false return is never a
// delivery failure, because the message is already durable and the agent still
// finds it on its next read.
//
// Implementations must be safe for concurrent callers and must bound their own
// work: the caller treats the ping as fire-and-forget.
type InboxPinger interface {
	PingInbox(agentID, messageID, sender string) bool
}

// inboxWaiter is one parked long-poll read. ready is buffered (1) and only ever
// SIGNALED, never closed: a notify that races an unsubscribe therefore cannot
// panic on a closed channel, and a stray signal on a waiter nobody is reading
// is harmless (the channel is garbage once the waiter is dropped).
type inboxWaiter struct {
	ready chan struct{}
}

// inboxNotifier wakes parked long-poll reads when a delivery lands in an
// inbox. Waiters are keyed by agent id and signalled as a group: a woken read
// claims whatever batch the store hands it, so one wake per delivery is enough
// no matter how many reads are parked.
//
// Every method tolerates a nil receiver, so a Handler that was never given a
// notifier (a zero-value one, not built by NewHandler) degrades to
// fallback-polling instead of panicking on a parked read's wake-up path.
type inboxNotifier struct {
	mu      sync.Mutex
	waiters map[string]map[*inboxWaiter]struct{}
}

// newInboxNotifier returns an empty notifier registry.
func newInboxNotifier() *inboxNotifier {
	return &inboxNotifier{waiters: make(map[string]map[*inboxWaiter]struct{})}
}

// subscribe parks one read on agentID and returns the waiter that will be
// signalled when a delivery lands. Callers must unsubscribe exactly once.
func (n *inboxNotifier) subscribe(agentID string) *inboxWaiter {
	if n == nil {
		return nil
	}
	w := &inboxWaiter{ready: make(chan struct{}, 1)}
	n.mu.Lock()
	defer n.mu.Unlock()
	set := n.waiters[agentID]
	if set == nil {
		set = make(map[*inboxWaiter]struct{})
		n.waiters[agentID] = set
	}
	set[w] = struct{}{}
	return w
}

// unsubscribe releases a parked read, dropping the agent's entry entirely once
// its last waiter is gone so a long-lived server does not accumulate keys.
func (n *inboxNotifier) unsubscribe(agentID string, w *inboxWaiter) {
	if n == nil || w == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	set := n.waiters[agentID]
	if set == nil {
		return
	}
	delete(set, w)
	if len(set) == 0 {
		delete(n.waiters, agentID)
	}
}

// notify signals every read parked on agentID. It never blocks: the signal is
// a non-blocking send into a buffered channel, because a delivery must not be
// delayed by (or fail on) the wake-up path.
func (n *inboxNotifier) notify(agentID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for w := range n.waiters[agentID] {
		select {
		case w.ready <- struct{}{}:
		default:
			// Already signalled: one wake is enough, since the woken read
			// claims the whole claimable batch.
		}
	}
}

// wakeLongPolls releases the reads parked on agentID (CR-FEAT-023). It is
// called AFTER the delivery is in the store, never before: a woken read must
// find the message when it re-reads.
func (h *Handler) wakeLongPolls(agentID string) {
	h.notifier.notify(agentID)
}

// pingInbox fires the optional new-message ping (CR-FEAT-023).
//
// Fire-and-forget by construction: the ping is a notification ABOUT a message
// that is already durable, so it must never sit between the store write and the
// sender's accept. A delivery to an agent that never asked for pings (or whose
// socket is gone) simply reports false and is logged at debug — one goroutine
// per ping, each bounded by the transport's own write deadline.
func (h *Handler) pingInbox(agentID, messageID, sender string) {
	p := h.inboxPing
	if p == nil {
		return
	}
	go func() {
		if !p.PingInbox(agentID, messageID, sender) {
			slog.Debug("inbox ping not delivered", "agent_id", agentID, "message_id", messageID)
		}
	}()
}

// retrieveWithWait is the read behind GET /agents/{id}/inbox (CR-FEAT-023).
//
// wait == 0 is the poll-only read this endpoint has always made: one store
// retrieve, returned as-is. wait > 0 parks: the read returns the moment a
// message becomes claimable, or — when the budget expires with nothing
// claimable — an empty result together with the live counters, which is
// byte-identical to what the poll-only path answers for the same inbox.
//
// The parked read is released by either of two edges:
//
//   - a delivery through this handler's HandleDeliver (immediate: the notifier
//     is signalled right after the store write);
//   - waitFallbackInterval, which covers inbox writes that bypass this handler
//     (the federation hold queue, webhook-failure notices, a second relay
//     process sharing the store).
//
// A store error, including agent-not-found, is returned immediately — the wait
// never turns a 404 or a 500 into a late empty 200. A cancelled request
// context ends the park with ctx.Err(), so a client that hung up does not keep
// a read alive for the rest of the budget.
func (h *Handler) retrieveWithWait(ctx context.Context, agentID string, lease time.Duration, maxMessages int, wait time.Duration) ([]*InboxEntry, string, error) {
	if wait <= 0 {
		return h.store.Retrieve(agentID, lease, maxMessages)
	}

	// Subscribe BEFORE the first read: a delivery landing between that read
	// and the park would otherwise be a lost wake-up, leaving a claimable
	// message sitting in the inbox until the budget expired.
	waiter := h.notifier.subscribe(agentID)
	defer h.notifier.unsubscribe(agentID, waiter)

	// A nil waiter (a Handler with no notifier) leaves `ready` nil below, and
	// a nil channel is never selected — the fallback arm and the budget still
	// bound the wait, so the read is correct, only slower to wake.
	var ready <-chan struct{}
	if waiter != nil {
		ready = waiter.ready
	}

	budget := time.NewTimer(wait)
	defer budget.Stop()
	fallback := time.NewTicker(waitFallbackInterval)
	defer fallback.Stop()

	for {
		messages, leaseID, err := h.store.Retrieve(agentID, lease, maxMessages)
		if err != nil || len(messages) > 0 {
			return messages, leaseID, err
		}
		select {
		case <-ready:
		case <-fallback.C:
		case <-budget.C:
			// Budget expired with nothing claimable: the same empty read a
			// poll-only caller gets. The caller adds the live queue_depth /
			// leased_count, so a message that arrived in the last instant is
			// still visible as depth rather than silently missing.
			return nil, "", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}
