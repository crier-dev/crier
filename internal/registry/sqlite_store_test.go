package registry

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The same assertions exercise both engines, rather than a SQLite-only imitation.
func TestLocalStoreParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "registry.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				s = db
			}
			for _, id := range []string{"a", "b"} {
				require.NoError(t, s.Register(&Agent{ID: id}))
			}
			require.ErrorIs(t, s.Register(&Agent{ID: "a"}), ErrAgentExists)
			require.ErrorIs(t, s.Register(&Agent{ID: "a", PublicKey: make(HexKey, 32)}), ErrPublicKeyMismatch)
			for i := 0; i < 8; i++ {
				require.NoError(t, s.Deliver("a", &InboxEntry{ID: fmt.Sprint(i), Payload: []byte("hello")}))
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			seen := map[string]bool{}
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					entries, lease, err := s.Retrieve("a", time.Hour, 1)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					require.NotEmpty(t, lease)
					mu.Lock()
					require.False(t, seen[entries[0].ID])
					seen[entries[0].ID] = true
					mu.Unlock()
				}()
			}
			wg.Wait()
			empty, lease, err := s.Retrieve("a", time.Hour, 10)
			require.NoError(t, err)
			require.NotNil(t, empty)
			require.Empty(t, empty)
			require.Empty(t, lease)
			entries, err := s.(InboxLister).PeekInbox("a")
			require.NoError(t, err)
			require.ErrorIs(t, s.Ack("b", entries[0].LeaseID, []string{entries[0].ID}), ErrMessageNotFound)
			require.ErrorIs(t, s.Ack("a", "wrong", []string{entries[0].ID}), ErrLeaseConflict)
			require.NoError(t, s.Ack("a", entries[0].LeaseID, []string{entries[0].ID}))
			require.ErrorIs(t, s.Ack("a", entries[0].LeaseID, []string{entries[0].ID}), ErrMessageNotFound)
			zero := 0
			require.NoError(t, s.Deliver("b", &InboxEntry{ID: "expired", CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)}))
			require.NoError(t, s.Deliver("b", &InboxEntry{ID: "forever", TTLSeconds: &zero}))
			require.Equal(t, 1, s.PurgeExpired())
			entries, lease, err = s.Retrieve("b", time.Hour, 10)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "forever", entries[0].ID)
			require.NotEmpty(t, lease)
			obs := s.(TaskObservationStore)
			o := TaskObservation{TaskID: "task", ObservedAt: time.Now().UTC(), Outcome: "unknown", Evidence: []byte(`{"state":"unknown"}`)}
			require.NoError(t, obs.RecordTaskObservation("a", o))
			o.Outcome = "acknowledged"
			require.NoError(t, obs.RecordTaskObservation("a", o))
			o.Outcome = "unknown"
			require.NoError(t, obs.RecordTaskObservation("a", o))
			got, found, err := obs.TaskObservation("a", "task")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "acknowledged", got.Outcome)
			_, found, err = obs.TaskObservation("b", "task")
			require.NoError(t, err)
			require.False(t, found)
			outputs := s.(TaskOutputStore)
			output := TaskOutput{TaskID: "task", ObservedAt: time.Now().UTC(), Artifact: []byte(`{"artifactId":"answer","parts":[{"kind":"text","text":"first"}]}`)}
			require.NoError(t, outputs.RecordTaskOutput("a", output))
			output.Artifact = []byte(`{"artifactId":"answer","parts":[{"kind":"text","text":"second"}]}`)
			require.NoError(t, outputs.RecordTaskOutput("a", output))
			out, found, err := outputs.TaskOutput("a", "task")
			require.NoError(t, err)
			require.True(t, found)
			require.Contains(t, string(out.Artifact), "first")
		})
	}
}

func TestSQLiteStoreDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	for _, id := range []string{"a", "b"} {
		require.NoError(t, s.Register(&Agent{ID: id}))
	}
	entry := &InboxEntry{ID: "pending", Payload: []byte("durable payload"), ThreadID: "thread", PrincipalID: "human", Namespace: "realm", Location: &Location{Channel: "room"}}
	require.NoError(t, s.Deliver("a", entry))
	batch, lease, err := s.Retrieve("a", time.Hour, 1)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	before, err := s.Peek("a", "pending")
	require.NoError(t, err)
	require.Equal(t, time.Hour, before.LeaseDuration)
	require.Equal(t, lease, before.LeaseID)
	snapshot := func(label string) {
		t.Helper()
		q, l, _, err := s.Stats("a")
		require.NoError(t, err)
		t.Logf("%s: agents=%d inbox_entries=%d leased=%d message_id=pending lease_id=%s lease_duration=%s", label, len(s.List()), q, l, before.LeaseID, before.LeaseDuration)
	}
	snapshot("before close")
	require.NoError(t, s.Close())
	s, err = NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	snapshot("after reopen")
	require.Len(t, s.List(), 2)
	for _, id := range []string{"a", "b"} {
		_, err = s.Get(id)
		require.NoError(t, err)
	}
	after, err := s.Peek("a", "pending")
	require.NoError(t, err)
	require.Equal(t, before, after)
	batch, otherLease, err := s.Retrieve("a", time.Hour, 1)
	require.NoError(t, err)
	require.Empty(t, batch)
	require.Empty(t, otherLease)
	require.NoError(t, s.Ack("a", lease, []string{"pending"}))
	require.NoError(t, s.Close())
	s, err = NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	_, err = s.Peek("a", "pending")
	require.ErrorIs(t, err, ErrMessageNotFound)
}

func TestSQLiteStoreIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	a, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = a.Close() }()
	b, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()
	require.NoError(t, a.Register(&Agent{ID: "a"}))
	require.NoError(t, a.Deliver("a", &InboxEntry{ID: "one"}))
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for _, s := range []*SQLiteStore{a, b} {
		wg.Add(1)
		go func(s *SQLiteStore) {
			defer wg.Done()
			entries, _, err := s.Retrieve("a", time.Hour, 1)
			require.NoError(t, err)
			counts <- len(entries)
		}(s)
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	require.Equal(t, 1, total)
}
