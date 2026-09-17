package registry

import (
	"fmt"
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
// the update, mirroring PostgresStore.Update (DF-CRIER-156: a PATCH is the
// only activity the registry observes, so the response's last_seen must be
// the value that is now persisted — a client can use it to confirm the
// write). The registry has no heartbeat, so an agent that never PATCHes
// keeps its registration-time last_seen. Returns an error if the agent is
// not found.
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
	s.agents[agent.ID] = agent
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
func (s *MemoryStore) Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	now := time.Now()
	queue := s.inboxes[agentID]

	// Pass 1: pick the batch. Expired leases on the way past are released
	// inline so the entry falls through to the claiming pass (DF-CRIER-33).
	// Without this, a default-backend message was redelivered only after
	// lease + a purge tick (60s for a documented 30s lease), because
	// PurgeExpired was the only release path. PurgeExpired remains the
	// backstop for messages nobody retrieves.
	capHint := maxMessages
	if capHint < 0 {
		capHint = 0
	}
	if capHint > len(queue) {
		capHint = len(queue)
	}
	batch := make([]*InboxEntry, 0, capHint)

	for _, entry := range queue {
		if len(batch) >= maxMessages {
			break
		}
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

		batch = append(batch, entry)
	}

	if len(batch) == 0 {
		// Nothing was leased: minting a lease here would hand the caller a
		// usable-looking credential over zero messages.
		return []*InboxEntry{}, "", nil
	}

	leaseID, err := newLeaseID()
	if err != nil {
		return nil, "", fmt.Errorf("generate lease id: %w", err)
	}

	// Pass 2: stamp the claimed batch with the lease.
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

// PurgeExpired removes all expired messages from all inboxes and returns
// messages whose lease has expired back to the unleased state. Returns the
// total number of messages removed.
func (s *MemoryStore) PurgeExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0

	for agentID, queue := range s.inboxes {
		filtered := make([]*InboxEntry, 0, len(queue))
		for _, entry := range queue {
			// Remove expired messages. A zero ExpiresAt means the message
			// never expires (ttl_seconds=0, DF-CRIER-37) and must survive
			// every purge pass.
			if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
				removed++
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

	return removed
}
