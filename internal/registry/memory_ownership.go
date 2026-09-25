package registry

import (
	"fmt"
)

// DefaultDeadLetterCapacity is how many dead letters the in-memory destination
// keeps. The memory backend is process-lifetime and needs a hard bound: past it
// the OLDEST record is evicted, so a relay under a permanently failing consumer
// cannot grow without limit. (A persisting backend is bounded by time instead —
// DefaultDeadLetterRetention.)
const DefaultDeadLetterCapacity = 1024

// DefaultDeadLetterListLimit is the page size of a dead-letter read when the
// caller names none.
const DefaultDeadLetterListLimit = 20

// MaxDeadLetterListLimit is the largest page a caller may request. A caller
// asking for more is rejected rather than silently clamped (the house rule for
// a documented parameter: honor it or reject it, never ignore it).
const MaxDeadLetterListLimit = 100

// memoryDeadLetters is the in-memory dead-letter destination. It is guarded by
// the owning MemoryStore's mutex — every method here is called with that lock
// held — so it needs none of its own.
type memoryDeadLetters struct {
	capacity int
	// order holds message ids oldest-first; byID resolves them. Order is what
	// makes eviction and newest-first listing exact.
	order []string
	byID  map[string]*DeadLetter
}

func newMemoryDeadLetters(capacity int) *memoryDeadLetters {
	if capacity <= 0 {
		capacity = DefaultDeadLetterCapacity
	}
	return &memoryDeadLetters{
		capacity: capacity,
		byID:     make(map[string]*DeadLetter),
	}
}

// append records dl unless its message id is already there, evicting the oldest
// record when the bound is exceeded.
func (d *memoryDeadLetters) append(dl *DeadLetter) bool {
	if _, exists := d.byID[dl.MessageID]; exists {
		return false
	}
	// Copy the record and its payload so a later mutation of the caller's
	// entry (or of the caller's byte slice) cannot rewrite history.
	rec := *dl
	rec.Payload = append([]byte(nil), dl.Payload...)
	d.byID[rec.MessageID] = &rec
	d.order = append(d.order, rec.MessageID)

	for len(d.order) > d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.byID, oldest)
	}
	return true
}

// list returns up to limit records for agentID, newest first.
func (d *memoryDeadLetters) list(agentID string, limit int) []*DeadLetter {
	if limit <= 0 {
		limit = DefaultDeadLetterListLimit
	}
	out := make([]*DeadLetter, 0, limit)
	for i := len(d.order) - 1; i >= 0 && len(out) < limit; i-- {
		rec, ok := d.byID[d.order[i]]
		if !ok || rec.AgentID != agentID {
			continue
		}
		cp := *rec
		cp.Payload = append([]byte(nil), rec.Payload...)
		out = append(out, &cp)
	}
	return out
}

// AppendDeadLetter records a dead letter, reporting whether it was added
// (CR-FEAT-025). A message id already recorded is not added twice.
func (s *MemoryStore) AppendDeadLetter(dl *DeadLetter) (bool, error) {
	if dl == nil || dl.MessageID == "" {
		return false, fmt.Errorf("%w: dead letter needs a message id", ErrInvalidStoreInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadLetters.append(dl), nil
}

// ListDeadLetters returns up to limit dead letters for the given agent, newest
// first. The agent need not be registered: a dead letter outlives the row it
// was addressed to, which is exactly when an operator needs to read it.
func (s *MemoryStore) ListDeadLetters(agentID string, limit int) ([]*DeadLetter, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if limit > MaxDeadLetterListLimit {
		return nil, fmt.Errorf("%w: limit must be <= %d", ErrInvalidStoreInput, MaxDeadLetterListLimit)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deadLetters.list(agentID, limit), nil
}

// Transfer moves messages out of an agent's inbox into another agent's inbox
// (CR-FEAT-025, Transferrer). The moved messages keep their id, payload,
// creation and expiry and land in the destination UNLEASED, which is what makes
// a stuck lease recoverable: the new holder can claim them immediately instead
// of waiting out the old lease.
//
// Error contract (shared with the PostgreSQL backend): ErrInvalidStoreInput for
// a blank/identical agent, an empty id list or a duplicate id;
// ErrAgentNotFound when either agent is unknown; ErrMessageNotFound when a
// requested id is not in the source inbox; ErrLeaseConflict when a message is
// held under a DIFFERENT lease and force is not set. Nothing moves unless every
// requested message moves.
func (s *MemoryStore) Transfer(agentID, leaseID string, messageIDs []string, targetAgentID string, force bool) (int, error) {
	if agentID == "" || targetAgentID == "" {
		return 0, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if agentID == targetAgentID {
		return 0, fmt.Errorf("%w: transfer target %q is the source inbox", ErrInvalidStoreInput, targetAgentID)
	}
	if len(messageIDs) == 0 {
		return 0, fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}
	seen := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		if seen[id] {
			return 0, fmt.Errorf("%w: duplicate message ID %q", ErrInvalidStoreInput, id)
		}
		seen[id] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}
	if _, ok := s.agents[targetAgentID]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrAgentNotFound, targetAgentID)
	}

	queue := s.inboxes[agentID]
	index := make(map[string]*InboxEntry, len(queue))
	for _, entry := range queue {
		index[entry.ID] = entry
	}

	// Classify every requested id before moving anything: a not-found id and a
	// live foreign lease are both terminal, and a partial move would silently
	// split a batch the caller asked to move as a unit. Without force an entry
	// is movable when it is unleased (nobody owns it) or leased under the
	// caller's lease.
	var missing, mismatched []string
	for _, id := range messageIDs {
		entry, ok := index[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if !force && entry.LeaseID != "" && entry.LeaseID != leaseID {
			mismatched = append(mismatched, id)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: message id(s) %s do not exist in agent %q's inbox (never delivered, already acknowledged, or expired)",
			ErrMessageNotFound, quoteMessageIDs(missing), agentID)
	}
	if len(mismatched) > 0 {
		return 0, fmt.Errorf("%w: message %q is not leased under lease %q (current lease: %q)",
			ErrLeaseConflict, mismatched[0], leaseID, index[mismatched[0]].LeaseID)
	}

	wanted := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		wanted[id] = true
	}

	moved := make([]*InboxEntry, 0, len(messageIDs))
	kept := make([]*InboxEntry, 0, len(queue))
	for _, entry := range queue {
		if !wanted[entry.ID] {
			kept = append(kept, entry)
			continue
		}
		entry.AgentID = targetAgentID
		entry.LeasedAt = nil
		entry.LeaseID = ""
		entry.LeaseDuration = 0
		entry.ACKed = false
		moved = append(moved, entry)
	}
	s.inboxes[agentID] = kept
	s.inboxes[targetAgentID] = append(s.inboxes[targetAgentID], moved...)

	return len(moved), nil
}

// compile-time assertions: the memory backend implements the ownership
// capabilities (CR-FEAT-025). A rename here must fail the build rather than
// silently drop the dead-letter destination or the transfer surface for the
// default backend.
var (
	_ PurgeReporter   = (*MemoryStore)(nil)
	_ DeadLetterStore = (*MemoryStore)(nil)
	_ Transferrer     = (*MemoryStore)(nil)
)
