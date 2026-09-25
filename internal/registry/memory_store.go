package registry

import (
	"fmt"
	"sort"
	"time"
)

// Register adds an agent to the registry. Returns an error if the agent ID
// already exists (409-style conflict).
func (s *MemoryStore) Register(agent *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.agents[agent.ID]; exists {
		return fmt.Errorf("%w: %q", ErrAgentExists, agent.ID)
	}

	now := time.Now()
	agent.RegisteredAt = now
	agent.LastSeen = now
	if agent.Status == "" {
		agent.Status = StatusOnline
	}
	if agent.Capabilities == nil {
		agent.Capabilities = []string{}
	}

	s.agents[agent.ID] = agent
	s.inboxes[agent.ID] = make([]*InboxEntry, 0)
	return nil
}

// Get returns an agent by ID. Returns an error if not found (404-style).
func (s *MemoryStore) Get(id string) (*Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agent, ok := s.agents[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return agent, nil
}

// List returns all registered agents.
func (s *MemoryStore) List() []*Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, a)
	}
	return out
}

// Unregister removes an agent and its inbox. Returns an error if not found.
func (s *MemoryStore) Unregister(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[id]; !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	delete(s.agents, id)
	delete(s.inboxes, id)
	return nil
}

// Update replaces the mutable registration fields of an existing agent
// (capabilities, webhook) with the caller's copy — the PATCH /agents/{id}
// path (CR-FEAT-007). Registration identity and registration time are
// preserved from the stored record; last_seen is ADVANCED to the moment of
// mirroring PostgresStore.Update (DF-CRIER-156: a PATCH is the only
// agent-owned write the registry observes, so the response's last_seen must be
// the value that is now persisted — a client can use it to confirm the
// write). Since CR-FEAT-024 that PATCH is one of the three liveness signals
// this registry records in last_seen — along with a mesh connect and a mesh
// KEEPALIVE heartbeat, both via Touch — and the status reported for a row is
// DERIVED from last_seen (presence.go). Returns an error if the agent is not
// found.
func (s *MemoryStore) Update(agent *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.agents[agent.ID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agent.ID)
	}
	if agent.Capabilities == nil {
		agent.Capabilities = []string{}
	}
	agent.Status = existing.Status
	agent.RegisteredAt = existing.RegisteredAt
	agent.LastSeen = time.Now()
	// The realm is registration-time identity, not a PATCH field (CR-FEAT-029):
	// it is preserved from the stored row exactly like RegisteredAt, so a
	// caller that builds an Agent without one can never move a live agent into
	// another realm through the update path. Moving realms is unregister +
	// register; the HTTP handler refuses a `namespace` member on a PATCH for
	// the same reason.
	agent.Namespace = existing.Namespace
	s.agents[agent.ID] = agent
	return nil
}

// Touch advances an agent's last_seen from a liveness signal — the mesh
// heartbeat path (CR-FEAT-024, registry.Toucher). Nothing else on the row
// moves: the stored status stays whatever it was, because the status REPORTED
// for the row is derived from last_seen at read time (presence.go), not stored.
//
// The advance is MONOTONIC: an `at` older than the recorded last_seen (a clock
// adjustment, a frame replayed out of order) leaves the row where it was, so a
// heartbeat can never make a live agent look older than its real evidence.
//
// Returns ErrAgentNotFound for an unknown id — the ordinary case for a peer
// that connected without a registry row.
func (s *MemoryStore) Touch(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.agents[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	if at.IsZero() {
		at = time.Now()
	}
	if at.After(agent.LastSeen) {
		agent.LastSeen = at
	}
	return nil
}

// Deliver appends a message to an agent's FIFO inbox. Returns an error if the
// agent is not found.
func (s *MemoryStore) Deliver(agentID string, entry *InboxEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	entry.AgentID = agentID
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	// Apply the delivery's requested lifetime: ttl_seconds > 0 → that many
	// seconds, ttl_seconds == 0 → never expires (ExpiresAt stays zero),
	// absent → the 24h default (DF-CRIER-37).
	if err := resolveMessageExpiry(entry); err != nil {
		return err
	}
	if entry.ID == "" {
		id, err := newLeaseID()
		if err != nil {
			return fmt.Errorf("generate message id: %w", err)
		}
		entry.ID = id
	}
	// A delivery never arrives pre-leased: the lease is minted by Retrieve and
	// by nothing else (CR-FEAT-025 keeps the re-delivery below honest — an
	// entry handed back by a dead-letter/transfer path is unleased).
	entry.LeasedAt = nil
	entry.LeaseID = ""
	entry.LeaseDuration = 0
	entry.ACKed = false

	s.inboxes[agentID] = append(s.inboxes[agentID], entry)
	return nil
}

// Retrieve fetches up to maxMessages un-ACKed, un-expired messages from an
// agent's inbox. When at least one message is claimable it mints a new lease
// ID, sets LeasedAt to now, and marks each message with that lease ID.
// When nothing is claimable (empty inbox, or every queued message already
// leased and unexpired) it returns a non-nil empty slice and an EMPTY lease
// ID — no lease is minted for a batch that does not exist (DF-CRIER-32).
// Under write lock, so concurrent retrievers get disjoint message sets.
//
// ORDER (CR-FEAT-035): highest Priority first; messages of equal priority keep
// their arrival order. Every message delivered without a `priority` carries the
// zero value, so a queue of them comes back exactly FIFO — the ordering this
// method had before priorities existed. The ordering is applied to the whole
// claimable set BEFORE the batch is cut to maxMessages, which is what makes a
// high-priority message behind a long low-priority backlog reachable at all.
func (s *MemoryStore) Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	// A non-positive batch size claims nothing (the pre-CR-FEAT-035 contract)
	// and, for the same reason as before, touches no entry's lease state.
	if maxMessages <= 0 {
		return []*InboxEntry{}, "", nil
	}

	now := time.Now()
	queue := s.inboxes[agentID]

	// Pass 1: collect every claimable message. Expired leases on the way past
	// are released inline so the entry falls through to the claiming pass
	// (DF-CRIER-33). Without this, a default-backend message was redelivered
	// only after lease + a purge tick (60s for a documented 30s lease), because
	// PurgeExpired was the only release path. PurgeExpired remains the
	// backstop for messages nobody retrieves.
	available := make([]*InboxEntry, 0, len(queue))

	for _, entry := range queue {
		// Skip already ACKed messages.
		if entry.ACKed {
			continue
		}
		// Skip expired messages. A zero ExpiresAt means "never expires"
		// (ttl_seconds=0, DF-CRIER-37) — the zero time is Before(now), so
		// the check must be skipped explicitly or a never-expiring message
		// would be treated as long expired.
		if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
			continue
		}
		// Skip already leased messages, unless the lease has expired — in
		// which case release it inline and treat it as available.
		if entry.LeasedAt != nil && entry.LeaseID != "" {
			leaseDuration := entry.LeaseDuration
			if leaseDuration == 0 {
				leaseDuration = 30 * time.Second
			}
			if entry.LeasedAt.Add(leaseDuration).Before(now) {
				// Lease expired: release inline and treat as available.
				entry.LeasedAt = nil
				entry.LeaseID = ""
				entry.LeaseDuration = 0
			} else {
				continue
			}
		}

		available = append(available, entry)
	}

	if len(available) == 0 {
		// Nothing was leased: minting a lease here would hand the caller a
		// usable-looking credential over zero messages.
		return []*InboxEntry{}, "", nil
	}

	// Pass 2: order the claimable set (CR-FEAT-035) and cut the batch. Stable,
	// so equal priorities keep the arrival order `available` was built in.
	if len(available) > 1 {
		sort.SliceStable(available, func(i, j int) bool {
			return available[i].Priority > available[j].Priority
		})
	}
	batch := available
	if len(batch) > maxMessages {
		batch = batch[:maxMessages]
	}

	leaseID, err := newLeaseID()
	if err != nil {
		return nil, "", fmt.Errorf("generate lease id: %w", err)
	}

	// Pass 3: stamp the claimed batch with the lease.
	for _, entry := range batch {
		leasedAt := now
		entry.LeasedAt = &leasedAt
		entry.LeaseID = leaseID
		entry.LeaseDuration = leaseDuration
	}

	return batch, leaseID, nil
}

// Ack permanently removes messages by ID that match the given leaseID.
// Message IDs absent from the inbox are reported as ErrMessageNotFound;
// IDs that exist under a different (or no) lease are reported as
// ErrLeaseConflict. Missing IDs are reported first so an all-unknown request
// is never mistaken for a stale-lease problem (DF-CRIER-32).
func (s *MemoryStore) Ack(agentID, leaseID string, messageIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	// A lease-only ack is a silent no-op that leaves messages queued for
	// redelivery after lease expiry (CR-GAP-014) — reject it.
	if len(messageIDs) == 0 {
		return fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}

	queue := s.inboxes[agentID]
	idSet := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		idSet[id] = true
	}

	// Classify every requested ID before deleting anything: absent from the
	// inbox (never delivered, acked, or expired+purged) is a not-found;
	// present under another lease is a lease conflict.
	index := make(map[string]*InboxEntry, len(queue))
	for _, entry := range queue {
		index[entry.ID] = entry
	}

	var missing, mismatched []string
	for _, id := range messageIDs {
		entry, ok := index[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if entry.LeaseID != leaseID {
			mismatched = append(mismatched, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: message id(s) %s do not exist in agent %q's inbox (never delivered, already acknowledged, or expired)",
			ErrMessageNotFound, quoteMessageIDs(missing), agentID)
	}
	if len(mismatched) > 0 {
		return fmt.Errorf("%w: message %q is not leased under lease %q (current lease: %q)",
			ErrLeaseConflict, mismatched[0], leaseID, index[mismatched[0]].LeaseID)
	}

	// Filter out ACKed messages.
	filtered := make([]*InboxEntry, 0, len(queue))
	for _, entry := range queue {
		if idSet[entry.ID] {
			continue
		}
		filtered = append(filtered, entry)
	}
	s.inboxes[agentID] = filtered
	return nil
}

// Stats returns queue depth, leased count, and oldest message age for an
// agent's inbox.
func (s *MemoryStore) Stats(agentID string) (queueDepth, leasedCount int, oldestAge time.Duration, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.agents[agentID]; !ok {
		return 0, 0, 0, fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	now := time.Now()
	queue := s.inboxes[agentID]
	var oldest time.Time

	for _, entry := range queue {
		if entry.ACKed {
			continue
		}
		// A zero ExpiresAt is "never expires" (DF-CRIER-37) — counted, not
		// silently treated as expired.
		if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
			continue
		}
		queueDepth++
		if entry.LeasedAt != nil && entry.LeaseID != "" {
			leasedCount++
		}
		if oldest.IsZero() || entry.CreatedAt.Before(oldest) {
			oldest = entry.CreatedAt
		}
	}

	if !oldest.IsZero() {
		oldestAge = now.Sub(oldest)
	}

	return queueDepth, leasedCount, oldestAge, nil
}

// QueueDepth reports the store-wide inbox queue accounting (CR-FEAT-035,
// registry.DepthReporter): every un-acknowledged, un-expired message in every
// inbox, how many of those are held under a live lease, and how long the oldest
// one has been waiting.
//
// The per-message rules are the SAME predicates this store's Stats already
// applies per agent (a zero ExpiresAt means "never expires"; a lease counts as
// held while LeasedAt/LeaseID are set), so the store-wide number is the sum of
// the per-agent ones rather than a second definition of "queued". The in-memory
// store cannot fail here, but the signature returns an error because
// DepthReporter is shared with the PostgreSQL backend, where a depth query can
// fail.
func (s *MemoryStore) QueueDepth() (QueueDepth, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var (
		depth  QueueDepth
		oldest time.Time
	)

	for _, queue := range s.inboxes {
		for _, entry := range queue {
			if entry.ACKed {
				continue
			}
			// A zero ExpiresAt is "never expires" (DF-CRIER-37) — counted,
			// not silently treated as expired.
			if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
				continue
			}
			depth.Pending++
			if entry.LeasedAt != nil && entry.LeaseID != "" {
				depth.Leased++
			}
			if oldest.IsZero() || entry.CreatedAt.Before(oldest) {
				oldest = entry.CreatedAt
			}
		}
	}

	if !oldest.IsZero() {
		depth.OldestAge = now.Sub(oldest)
	}

	return depth, nil
}

// PurgeExpired removes all expired messages from all inboxes and returns
// messages whose lease has expired back to the unleased state. Returns the
// total number of messages removed.
//
// It is the count-only arm of PurgeExpiredReport (CR-FEAT-025): a caller that
// only needs the number gets it without a report callback, exactly as before.
func (s *MemoryStore) PurgeExpired() int {
	return s.PurgeExpiredReport(nil)
}

// PurgeExpiredReport removes every TTL-expired message and reports each one to
// `report` AFTER it is out of the inbox, so a caller can dead-letter it and
// notify its sender (CR-FEAT-025). Expired leases are still returned to the
// unleased state, as PurgeExpired has always done.
//
// The report callback is invoked with the store's write lock RELEASED: the
// caller's report path writes back to this store (a dead-letter record and a
// receipt into the sender's inbox), and reporting under the lock would
// self-deadlock on the non-reentrant mutex.
func (s *MemoryStore) PurgeExpiredReport(report func(agentID string, entry *InboxEntry)) int {
	expired := s.purgeExpiredLocked(time.Now())

	for _, entry := range expired {
		if report != nil {
			report(entry.AgentID, entry)
		}
	}
	return len(expired)
}

// purgeExpiredLocked performs the sweep and returns the removed entries (their
// AgentID filled in). It takes the write lock itself and releases it before
// returning, so no caller can accidentally invoke a report callback while
// holding it.
func (s *MemoryStore) purgeExpiredLocked(now time.Time) []*InboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*InboxEntry
	for agentID, queue := range s.inboxes {
		filtered := make([]*InboxEntry, 0, len(queue))
		for _, entry := range queue {
			// Remove expired messages. A zero ExpiresAt means the message
			// never expires (ttl_seconds=0, DF-CRIER-37) and must survive
			// every purge pass.
			if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
				entry.AgentID = agentID
				expired = append(expired, entry)
				continue
			}
			// Return expired leases to unleased state.
			if entry.LeasedAt != nil && entry.LeaseID != "" {
				leaseDuration := entry.LeaseDuration
				if leaseDuration == 0 {
					leaseDuration = 30 * time.Second
				}
				if entry.LeasedAt.Add(leaseDuration).Before(now) {
					entry.LeasedAt = nil
					entry.LeaseID = ""
					entry.LeaseDuration = 0
				}
			}
			filtered = append(filtered, entry)
		}
		s.inboxes[agentID] = filtered
	}
	return expired
}
