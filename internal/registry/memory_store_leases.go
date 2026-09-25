package registry

// RevokeLeases releases every outstanding lease held by an agent, so the
// messages it had claimed become claimable again (CR-FEAT-030, the
// kill-switch's revoke_leases step). It returns the number of leases released.
//
// Semantics, and why each part matters to containment:
//
//   - The entries are NOT deleted and NOT acked: revoking a lease returns a
//     message to the queue, it does not destroy it. A contained agent's
//     backlog stays deliverable to whoever is allowed to read that inbox
//     after the operator's investigation.
//   - Only leases are cleared. An already-ACKed message is final and is left
//     alone; a never-claimed message needs nothing.
//   - An unknown agent id is not an error: 0 released is the honest answer for
//     an agent with no row (the kill-switch unregisters the row AFTER this
//     step, so a re-run against a contained agent still answers 0, not a 404).
func (s *MemoryStore) RevokeLeases(agentID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	released := 0
	for _, entry := range s.inboxes[agentID] {
		if entry == nil || entry.ACKed {
			continue
		}
		if entry.LeaseID == "" && entry.LeasedAt == nil {
			continue
		}
		entry.LeasedAt = nil
		entry.LeaseID = ""
		entry.LeaseDuration = 0
		released++
	}
	return released, nil
}
