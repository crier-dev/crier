package registry

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// This file is the cheap half of the PostgreSQL archive-then-delete contract:
// it runs in the default regime (no build tag, no database), so a regression
// that hoists the retention DELETE above the archive callback reds at commit
// time instead of waiting for the integration job. The durable half — a real
// PostgreSQL, a really committed archive row, a really injected archive
// failure — is archive_hook_postgres_integration_test.go (build tag
// `integration`).
//
// What pgxmock can and cannot express, stated rather than implied: it can
// express the ORDER of statements on the pool (expectations match in order by
// default) and whether a statement runs at all, which is exactly the
// invariant. It cannot express durability, cross-connection visibility or a
// real constraint failure — that is what the integration test is for. It also
// cannot prove the archive write landed anywhere: the callback here writes to a
// fictitious sink table so the ordering has a statement to order against.

// deadLetterMockRows is the dead-letter read scan's column list, in scan order
// (deadLetterColumns in postgres_ownership.go, expanded).
func deadLetterMockRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"message_id", "agent_id", "sender", "payload", "created_at",
		"expires_at", "dead_lettered_at", "reason", "idempotency_key",
		"namespace", "principal_id",
	})
}

// archiveSinkInsert is the archive write the callbacks below perform. It is a
// stand-in for the DuckBrain POST: the point of the ordering is that SOME
// durable write the callback owns happens before the DELETE, so the mock needs
// a statement of the callback's own to order against.
const archiveSinkInsert = `INSERT INTO test_dead_letter_archive (message_id) VALUES ($1)`

// TestPostgresArchiveHookOrdersArchiveBeforeDelete proves the positive
// direction on the Postgres purge path: the callback's write happens first, and
// only then does the retention DELETE run. The order is enforced by the mock,
// not by the callback: pgxmock matches expectations in sequence, so an
// implementation that deleted first would hand the DELETE statement to the
// INSERT expectation and be refused.
func TestPostgresArchiveHookOrdersArchiveBeforeDelete(t *testing.T) {
	store, mock := newMockStore(t)
	old := time.Now().Add(-DefaultDeadLetterRetention - time.Hour).UTC()

	mock.ExpectQuery(`SELECT `).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(deadLetterMockRows().AddRow(
			"expired", "agent", "", []byte(`{"k":"v"}`), old, old, old,
			DeadLetterReasonExpired, "", "", ""))
	mock.ExpectExec(regexp.QuoteMeta(archiveSinkInsert)).WithArgs("expired").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM dead_letters`)).WithArgs("expired", "agent").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	removed := store.PurgeExpiredDeadLetters(func(dl *DeadLetter) bool {
		_, err := store.pool.Exec(context.Background(), archiveSinkInsert, dl.MessageID)
		return err == nil
	})

	require.Equal(t, 1, removed, "an archived dead letter is removed exactly once")
	require.NoError(t, mock.ExpectationsWereMet(),
		"the archive write must run BEFORE the retention DELETE and the DELETE must still run")
}

// TestPostgresArchiveHookRetainsRowWhenArchiveFails proves the negative
// direction: a failed archive write deletes nothing. The DELETE expectation is
// queued and left UNMET on purpose — that unmet expectation IS the assertion
// that no retention delete was issued, so a regression cannot pass by deleting
// and merely logging the archive failure.
func TestPostgresArchiveHookRetainsRowWhenArchiveFails(t *testing.T) {
	store, mock := newMockStore(t)
	old := time.Now().Add(-DefaultDeadLetterRetention - time.Hour).UTC()

	mock.ExpectQuery(`SELECT `).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(deadLetterMockRows().AddRow(
			"expired", "agent", "", []byte(`{"k":"v"}`), old, old, old,
			DeadLetterReasonExpired, "", "", ""))
	mock.ExpectExec(regexp.QuoteMeta(archiveSinkInsert)).WithArgs("expired").
		WillReturnError(errors.New("archive sink unavailable"))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM dead_letters`)).WithArgs("expired", "agent").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	removed := store.PurgeExpiredDeadLetters(func(dl *DeadLetter) bool {
		_, err := store.pool.Exec(context.Background(), archiveSinkInsert, dl.MessageID)
		return err == nil
	})

	require.Zero(t, removed, "a failed archive write removes nothing")
	unmet := mock.ExpectationsWereMet()
	require.Error(t, unmet, "the retention DELETE must never run when the archive write fails")
	require.Contains(t, unmet.Error(), "DELETE FROM dead_letters",
		"the unmet expectation must be the retention DELETE itself")
}

// TestAuthorityStoreKeepsArchiveHookVisible pins the composition seam the
// production server uses: `composeRegistryDelivery` wraps the registry store in
// an AuthorityStore whenever a service-delivery allowlist is configured, and
// Handler.PurgeExpired reaches the dead-letter retention sweep through a
// capability assertion on that wrapper. A wrapper that hides ArchiveHook
// silently disables archive-then-delete for every allowlist deployment, so the
// capability is asserted here and the sweep is driven end-to-end through it.
func TestAuthorityStoreKeepsArchiveHookVisible(t *testing.T) {
	base, err := NewSQLiteStore(filepath.Join(t.TempDir(), "wrapped-archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })

	wrapped := NewAuthorityStore(base, nil, "")
	require.Implements(t, (*ArchiveHook)(nil), wrapped,
		"the policy wrapper must not hide the retention sweep from the handler")

	old := time.Now().Add(-DefaultDeadLetterRetention - time.Hour).UTC()
	added, err := wrapped.AppendDeadLetter(&DeadLetter{
		MessageID: "through-wrapper", AgentID: "agent", Payload: []byte(`{"k":"v"}`),
		CreatedAt: old, ExpiredAt: old, DeadLetteredAt: old, Reason: DeadLetterReasonExpired,
	})
	require.NoError(t, err)
	require.True(t, added)

	handler := NewHandler(wrapped)
	var seen []string

	// Refusal direction: the sweep must call the hook, and must keep the record.
	handler.SetArchiveHook(func(dl *DeadLetter) bool {
		seen = append(seen, dl.MessageID)
		return false
	})
	handler.PurgeExpired()
	require.Equal(t, []string{"through-wrapper"}, seen,
		"the sweep must consult the archive hook through the wrapper, exactly once")
	retained, err := wrapped.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Len(t, retained, 1, "a refused archive retains the record")

	// Acceptance direction: same seam, archive succeeds, the record goes.
	seen = nil
	handler.SetArchiveHook(func(dl *DeadLetter) bool {
		seen = append(seen, dl.MessageID)
		return true
	})
	handler.PurgeExpired()
	require.Equal(t, []string{"through-wrapper"}, seen)
	purged, err := wrapped.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Empty(t, purged, "an accepted archive purges the expired record")
}
