package registry

// RevokeLeases releases every outstanding lease held by an agent
// (CR-FEAT-030, the kill-switch's revoke_leases step) and returns how many
// were released.
//
// The UPDATE clears all three lease columns together, because the table's
// inbox_entries_lease_fields_consistent CHECK constraint (migration 002)
// requires them to move as a unit: leased_at IS NULL iff lease_id IS NULL iff
// lease_expires_at IS NULL. Clearing one and leaving another would be rejected
// by the database — the same shape PurgeExpired already uses when it releases
// elapsed leases.
//
// A message is RETURNED TO THE QUEUE, not destroyed: no row is deleted, acked
// or expired. Rows already ACKed are left untouched (lease_id is NULL on them
// anyway), and an agent with no leases answers 0 rather than an error — the
// kill-switch unregisters the row after this step, so a re-run against a
// contained agent must still answer, not fail.
func (s *PostgresStore) RevokeLeases(agentID string) (int, error) {
	if agentID == "" {
		return 0, ErrInvalidStoreInput
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	tag, err := s.pool.Exec(ctx, `
UPDATE inbox_entries
SET leased_at = NULL,
    lease_id = NULL,
    lease_expires_at = NULL
WHERE agent_id = $1
  AND lease_id IS NOT NULL;`, agentID)
	if err != nil {
		s.setListError(err)
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
