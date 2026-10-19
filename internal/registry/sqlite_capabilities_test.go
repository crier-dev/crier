package registry

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "registry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAckTaskObservationSQLite(t *testing.T) { testAckTaskObservation(t, newTestSQLiteStore(t)) }

func TestSQLiteEvidenceAtomicAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.NoError(t, s.Register(&Agent{ID: "a"}))
	unknown := taskObservationFixture("unknown")
	require.NoError(t, s.RecordTaskObservation("a", unknown))
	require.ErrorIs(t, s.CloseObservedTask("a", taskObservationFixture("canceled")), ErrMessageNotFound)
	got, found, err := s.TaskObservation("a", "task")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "unknown", got.Outcome)
	require.NoError(t, s.Deliver("a", &InboxEntry{ID: "task", Payload: []byte(`{}`)}))
	// Fail evidence insert AFTER deletion. The same transaction must roll back
	// the deletion, not publish a cancellation or signal an acknowledgement.
	_, err = s.db.Exec(`CREATE TRIGGER reject_evidence BEFORE INSERT ON a2a_task_observations BEGIN SELECT RAISE(ABORT,'evidence disk failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, s.CloseObservedTask("a", taskObservationFixture("canceled")), "evidence disk failure")
	_, err = s.Peek("a", "task")
	require.NoError(t, err)
	_, err = s.db.Exec(`DROP TRIGGER reject_evidence`)
	require.NoError(t, err)
	cancellation := taskObservationFixture("canceled")
	require.NoError(t, s.CloseObservedTask("a", cancellation))
	output := taskOutputFixture()
	require.NoError(t, s.RecordTaskOutput("a", output))
	require.NoError(t, s.Unregister("a"))
	require.NoError(t, s.Close())
	s, err = NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	got, found, err = s.TaskObservation("a", "task")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, cancellation, got)
	out, found, err := s.TaskOutput("a", "task")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, output, out)
	require.NoError(t, s.RecordTaskObservation("a", unknown))
	got, _, err = s.TaskObservation("a", "task")
	require.NoError(t, err)
	require.Equal(t, cancellation, got)
	output.Artifact = []byte(`{"artifactId":"later","parts":[{"text":"ignored"}]}`)
	require.NoError(t, s.RecordTaskOutput("a", output))
	out, _, err = s.TaskOutput("a", "task")
	require.NoError(t, err)
	require.Contains(t, string(out.Artifact), "deployed")
	list, err := s.TaskObservations("a")
	require.NoError(t, err)
	require.Equal(t, []TaskObservation{cancellation}, list)
	require.ErrorIs(t, s.RecordTaskObservation("a", TaskObservation{}), ErrInvalidStoreInput)
	require.ErrorIs(t, s.RecordTaskOutput("a", TaskOutput{}), ErrInvalidStoreInput)
}

func TestSQLiteMetadataLeaseAndOwnership(t *testing.T) {
	s := newTestSQLiteStore(t)
	a := &Agent{ID: "a", PublicKey: make(HexKey, 32), Capabilities: []string{"worker"}, Namespace: "realm", Webhook: &webhook.Config{URL: "http://localhost/hook"}}
	require.NoError(t, s.Register(a))
	stored, err := s.Get("a")
	require.NoError(t, err)
	require.Equal(t, a, stored)
	require.NoError(t, s.Register(&Agent{ID: "b"}))
	prior := a.LastSeen
	require.NoError(t, s.Touch("a", prior.Add(time.Hour)))
	require.NoError(t, s.Touch("a", prior))
	stored, err = s.Get("a")
	require.NoError(t, err)
	require.True(t, stored.LastSeen.Equal(prior.Add(time.Hour)))
	updated := &Agent{ID: "a", Capabilities: []string{"new"}}
	require.NoError(t, s.Update(updated))
	require.Equal(t, a.RegisteredAt, updated.RegisteredAt)
	require.Equal(t, "realm", updated.Namespace)
	forever := 0
	entry := &InboxEntry{ID: "meta", Payload: []byte{0, 255, 17}, TTLSeconds: &forever, Sender: "b", IdempotencyKey: "key", PrincipalID: "human", Namespace: "realm", ThreadID: "thread", ReplyTo: "reply", Location: &Location{Channel: "room", SubThread: "sub"}, Model: NewHint(""), Provider: NewHint("specific"), DeliveryMs: 42, Guard: &guard.Meta{}}
	require.NoError(t, s.Deliver("a", entry))
	peek, err := s.Peek("a", "meta")
	require.NoError(t, err)
	entry.TTLSeconds = nil
	require.Equal(t, entry, peek)
	_, lease, err := s.Retrieve("a", time.Hour, 1)
	require.NoError(t, err)
	require.ErrorIs(t, s.Ack("a", lease, []string{"meta", "missing"}), ErrMessageNotFound)
	_, err = s.Transfer("a", "wrong", []string{"meta"}, "b", false)
	require.ErrorIs(t, err, ErrLeaseConflict)
	moved, err := s.Transfer("a", lease, []string{"meta"}, "b", false)
	require.NoError(t, err)
	require.Equal(t, 1, moved)
	peek, err = s.Peek("b", "meta")
	require.NoError(t, err)
	require.Empty(t, peek.LeaseID)
	require.True(t, peek.ExpiresAt.IsZero())
	require.Equal(t, entry.Payload, peek.Payload)
	_, _, err = s.Retrieve("b", time.Nanosecond, 1)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rows, newLease, err := s.Retrieve("b", time.Hour, 1)
		return err == nil && len(rows) == 1 && newLease != ""
	}, time.Second, time.Millisecond)
	count, err := s.RevokeLeases("b")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	depth, err := s.QueueDepth()
	require.NoError(t, err)
	require.Equal(t, 1, depth.Pending)
	require.Zero(t, depth.Leased)
	require.NoError(t, s.CloseEntry("b", "meta"))
	_, err = s.Peek("b", "meta")
	require.ErrorIs(t, err, ErrMessageNotFound)
	require.NoError(t, s.Deliver("a", &InboxEntry{ID: "old", CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute), Payload: []byte("expired")}))
	callbacks := 0
	require.Equal(t, 1, s.PurgeExpiredReport(func(id string, e *InboxEntry) {
		callbacks++
		_, err := s.Peek(id, e.ID)
		require.ErrorIs(t, err, ErrMessageNotFound)
		added, err := s.AppendDeadLetter(newDeadLetter(id, e, time.Now()))
		require.NoError(t, err)
		require.True(t, added)
	}))
	require.Equal(t, 1, callbacks)
	dl, found, err := s.LookupDeadLetter("a", "old")
	require.NoError(t, err)
	require.True(t, found)
	added, err := s.AppendDeadLetter(dl)
	require.NoError(t, err)
	require.False(t, added)
	require.NoError(t, s.Unregister("a"))
	list, err := s.ListDeadLetters("a", 20)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, found, err = s.LookupDeadLetter("b", "old")
	require.NoError(t, err)
	require.False(t, found)
}

func TestSQLiteServiceReceiptDurableAndAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.NoError(t, s.Register(&Agent{ID: "target"}))
	receipt := ServiceDeliveryReceipt{Status: 201, Response: []byte(`{"ok":true}`)}
	digest := strings.Repeat("a", 64)
	first, replay, err := s.DeliverServiceIdempotent("sender", "target", "key", digest, &InboxEntry{ID: "one"}, receipt)
	require.NoError(t, err)
	require.False(t, replay)
	require.NoError(t, s.Close())
	s, err = NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	again, replay, err := s.DeliverServiceIdempotent("sender", "target", "key", digest, &InboxEntry{ID: "two"}, receipt)
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, first, again)
	_, err = s.LookupServiceDelivery("sender", "target", "key", strings.Repeat("b", 64))
	require.ErrorIs(t, err, ErrServiceIdempotencyConflict)
	// Force receipt constraint failure and require its inbox write to roll back.
	_, _, err = s.DeliverServiceIdempotent("sender", "target", "bad", "not a digest", &InboxEntry{ID: "rolled-back"}, receipt)
	require.Error(t, err)
	_, err = s.Peek("target", "rolled-back")
	require.ErrorIs(t, err, ErrMessageNotFound)
	entries, err := s.PeekInbox("target")
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestSQLiteMigrationsAndBackendFailures(t *testing.T) {
	s := newTestSQLiteStore(t)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM registry_schema_migrations`).Scan(&count))
	require.Equal(t, 16, count)
	// A pre-015 file upgrades rather than silently missing the newest surfaces.
	require.NoError(t, s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		_, err := q.ExecContext(ctx, `DROP TABLE a2a_task_outputs; DROP TABLE a2a_task_observations; DELETE FROM registry_schema_migrations WHERE version>=15`)
		return err
	}))
	require.NoError(t, s.migrate())
	require.NoError(t, s.RecordTaskOutput("a", taskOutputFixture()))
	require.True(t, s.Healthy())
	require.NoError(t, s.Close())
	require.False(t, s.Healthy())
	require.Nil(t, s.List())
	require.Error(t, s.ListError())
	require.Error(t, s.Deliver("a", &InboxEntry{}))
	require.Zero(t, s.PurgeExpired())
	for _, path := range []string{"", ":memory:", "file:registry?mode=memory", filepath.Join(t.TempDir(), "absent", "db")} {
		_, err := NewSQLiteStore(path)
		require.Error(t, err, fmt.Sprint(path))
	}
}
