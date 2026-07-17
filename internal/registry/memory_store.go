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
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = entry.CreatedAt.Add(24 * time.Hour)
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
// agent's inbox. It assigns a new leaseID, sets LeasedAt to now, and marks
// each message with the leaseID. Returns the leased messages and the leaseID.
// Under write lock, so concurrent retrievers get disjoint message sets.
func (s *MemoryStore) Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	leaseID, err := newLeaseID()
	if err != nil {
		return nil, "", fmt.Errorf("generate lease id: %w", err)
	}

	now := time.Now()
	queue := s.inboxes[agentID]
	var leased []*InboxEntry

	for _, entry := range queue {
		if len(leased) >= maxMessages {
			break
		}
		// Skip already ACKed messages.
		if entry.ACKed {
			continue
		}
		// Skip expired messages.
		if entry.ExpiresAt.Before(now) {
			continue
		}
		// Skip already leased messages (lease still active).
		if entry.LeasedAt != nil && entry.LeaseID != "" {
			continue
		}

		leasedAt := now
		entry.LeasedAt = &leasedAt
		entry.LeaseID = leaseID
		entry.LeaseDuration = leaseDuration
		leased = append(leased, entry)
	}

	return leased, leaseID, nil
}

// Ack permanently removes messages by ID that match the given leaseID.
// Returns an error if any message is not found under that lease.
func (s *MemoryStore) Ack(agentID, leaseID string, messageIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}

	queue := s.inboxes[agentID]
	idSet := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		idSet[id] = true
	}

	// Verify all messages exist and are leased with the correct leaseID.
	found := 0
	for _, entry := range queue {
		if idSet[entry.ID] {
			found++
			if entry.LeaseID != leaseID {
				return fmt.Errorf("%w: message %q is not leased under lease %q (current lease: %q)", ErrLeaseConflict, entry.ID, leaseID, entry.LeaseID)
			}
		}
	}
	if found < len(messageIDs) {
		return fmt.Errorf("%w: %d of %d message(s) not found", ErrLeaseConflict, len(messageIDs)-found, len(messageIDs))
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
		if entry.ExpiresAt.Before(now) {
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
			// Remove expired messages.
			if entry.ExpiresAt.Before(now) {
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
