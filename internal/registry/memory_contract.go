package registry

import (
	"time"
)

// MemoryStore's arm of the INBOX consumer contract (INBOX-CRIER-001): the
// detector, the contract expiry sweep and the stats aging read. They mirror the
// Postgres and SQLite implementations; the capability interface set at the
// bottom of memory_ownership.go keeps all three in the same shape.

// contractSweepBatch bounds one sweep pass so a large backlog is drained over
// successive ticks instead of holding the lexicographic/write lock for one
// unbounded pass. The same bound exists in every backend.
const contractSweepBatch = 500

// contractOfLocked resolves the stored consumer contract for an agent. The
// caller holds s.mu (read or write).
func (s *MemoryStore) contractOfLocked(agentID string) *ConsumerContract {
	if a, ok := s.agents[agentID]; ok && a != nil {
		return a.Consumer
	}
	return nil
}

// ExpireContractOverdue removes unacknowledged messages that outlived their
// agent's consumer contract and reports each one AFTER the inbox no longer
// holds it — the same unlock-before-report discipline PurgeExpiredReport uses,
// because the report path writes back into this store (dead letter + receipt).
//
// The reported entry carries the CONTRACT DEADLINE in ExpiresAt: for a
// never-expiring message that is the instant that actually elapsed, and for a
// finite message it is the earlier of the two, which is what the dead letter
// and the MESSAGE_EXPIRED receipt must quote.
func (s *MemoryStore) ExpireContractOverdue(now time.Time, defaultTTL time.Duration, report func(agentID string, entry *InboxEntry)) int {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	removed := s.expireContractLocked(now, defaultTTL)
	for _, entry := range removed {
		if report != nil {
			report(entry.AgentID, entry)
		}
	}
	return len(removed)
}

func (s *MemoryStore) expireContractLocked(now time.Time, defaultTTL time.Duration) []*InboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*InboxEntry
	for agentID, queue := range s.inboxes {
		contract := s.contractOfLocked(agentID)
		ttl := contract.EffectiveTTL(defaultTTL)
		if ttl <= 0 {
			// No contract bound: the message's own expiry is the whole rule.
			continue
		}
		filtered := make([]*InboxEntry, 0, len(queue))
		for _, entry := range queue {
			if entry.ACKed {
				filtered = append(filtered, entry)
				continue
			}
			deadline := entry.CreatedAt.Add(ttl)
			// A message that is ALREADY past its own expiry is the message-TTL
			// purge's business (it may have run in the same tick, before us).
			// Only unacked, still-present entries are contract candidates.
			pastMessageExpiry := !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now)
			if !pastMessageExpiry && !deadline.After(now) && len(expired) < contractSweepBatch {
				entry.AgentID = agentID
				entry.ExpiresAt = deadline
				expired = append(expired, entry)
				continue
			}
			filtered = append(filtered, entry)
		}
		s.inboxes[agentID] = filtered
	}
	return expired
}

// StaleConsumers reports every agent whose unacknowledged queue is older than
// its own contract right now. A contract (or resolved default) of 0 means the
// agent opted out of a contract bound, so it can never be stale by contract.
func (s *MemoryStore) StaleConsumers(now time.Time, defaultTTL time.Duration) ([]StaleConsumer, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []StaleConsumer
	for agentID, queue := range s.inboxes {
		contract := s.contractOfLocked(agentID)
		ttl := contract.EffectiveTTL(defaultTTL)
		if ttl <= 0 {
			continue
		}
		count, oldest := unackedInQueue(queue, now)
		if count == 0 || oldest.IsZero() {
			continue
		}
		if oldest.Add(ttl).After(now) {
			continue
		}
		reader := ""
		if contract != nil {
			reader = contract.Reader
		}
		out = append(out, StaleConsumer{
			AgentID:     agentID,
			Reader:      reader,
			ContractTTL: ttl,
			Unacked:     count,
			Oldest:      oldest,
		})
	}
	return out, nil
}

// OldestUnacked answers the stats fields `unacked_count` and `oldest_unacked`:
// unacknowledged messages still legally in the inbox (never-expiring ones
// included) and the creation instant of the oldest.
func (s *MemoryStore) OldestUnacked(agentID string) (int, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.agents[agentID]; !ok {
		return 0, time.Time{}, ErrAgentNotFound
	}
	count, oldest := unackedInQueue(s.inboxes[agentID], time.Now())
	return count, oldest, nil
}

// unackedInQueue is the ONE definition of "unacked" for the contract surfaces:
// not acked and not past the message's own expiry. A never-expiring message is
// included: the consumer contract (declared or deployment default) is the bound
// that applies to it — that is precisely the eleven-day case this row closes.
func unackedInQueue(queue []*InboxEntry, now time.Time) (count int, oldest time.Time) {
	for _, entry := range queue {
		if entry.ACKed {
			continue
		}
		if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now) {
			continue
		}
		count++
		if oldest.IsZero() || entry.CreatedAt.Before(oldest) {
			oldest = entry.CreatedAt
		}
	}
	return count, oldest
}

// Keep the memory backend asserted against the contract capabilities the
// handler type-asserts for, so a rename cannot silently disable the sweep.
var (
	_ ContractExpirer       = (*MemoryStore)(nil)
	_ StaleConsumerReporter = (*MemoryStore)(nil)
	_ InboxAgingReporter    = (*MemoryStore)(nil)
)
