package registry

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testA2AReplayContract(t *testing.T, stores []A2AReplayStore) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC)
	window := time.Hour
	primary := stores[0]
	token, raw, err := primary.AcquireA2AReplay("agent", "key", now, window)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.Nil(t, raw)
	_, _, err = stores[len(stores)-1].AcquireA2AReplay("agent", "key", now, window)
	require.ErrorIs(t, err, ErrA2AReplayPending)
	require.NoError(t, primary.FinishA2AReplay("agent", "key", token, []byte(`{"task":"original"}`)))
	for _, store := range stores {
		owner, receipt, err := store.AcquireA2AReplay("agent", "key", now.Add(window-time.Nanosecond), window)
		require.NoError(t, err)
		require.Empty(t, owner)
		require.Equal(t, `{"task":"original"}`, string(receipt))
		receipt[0] = 'x' // reads must not alias the stored memory receipt
	}
	_, raw, err = primary.AcquireA2AReplay("agent", "key", now, window)
	require.NoError(t, err)
	require.Equal(t, `{"task":"original"}`, string(raw))
	for _, pair := range [][2]string{{"agent", "different"}, {"other-agent", "key"}} {
		owner, _, err := primary.AcquireA2AReplay(pair[0], pair[1], now, window)
		require.NoError(t, err)
		require.NotEmpty(t, owner)
		require.NoError(t, primary.FinishA2AReplay(pair[0], pair[1], owner, nil))
		owner, _, err = primary.AcquireA2AReplay(pair[0], pair[1], now, window)
		require.NoError(t, err, "known rejection must permit another attempt")
		require.NotEmpty(t, owner)
	}
	replacement, raw, err := primary.AcquireA2AReplay("agent", "key", now.Add(window), window)
	require.NoError(t, err)
	require.NotEmpty(t, replacement, "expiry boundary is inclusive")
	require.Nil(t, raw)
	require.NotEqual(t, token, replacement)
	require.Error(t, primary.FinishA2AReplay("agent", "key", token, []byte("stale")))
	require.NoError(t, primary.FinishA2AReplay("agent", "key", replacement, []byte("new")))
	// A crash leaves a committed pending reservation, not permission to retry.
	pending, _, err := primary.AcquireA2AReplay("agent", "interrupted", now, window)
	require.NoError(t, err)
	require.NotEmpty(t, pending)
	_, _, err = stores[len(stores)-1].AcquireA2AReplay("agent", "interrupted", now, window)
	require.ErrorIs(t, err, ErrA2AReplayPending)
	next, _, err := primary.AcquireA2AReplay("agent", "interrupted", now.Add(window), window)
	require.NoError(t, err)
	require.NotEmpty(t, next, "pending reservations use the same bounded retention")

	// Independent handles race on a store reservation, not on a handler map.
	var wg sync.WaitGroup
	tokens := make(chan string, 12)
	errs := make(chan error, 12)
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			owner, _, err := stores[i%len(stores)].AcquireA2AReplay("race", "same-key", now, window)
			if err != nil {
				errs <- err
			} else {
				tokens <- owner
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(tokens)
	close(errs)
	require.Len(t, tokens, 1)
	require.Len(t, errs, 11)
	for err := range errs {
		require.ErrorIs(t, err, ErrA2AReplayPending)
	}
	require.NoError(t, primary.FinishA2AReplay("race", "same-key", <-tokens, []byte("one")))
}

func TestA2AReplayStores(t *testing.T) {
	t.Run("memory-authority", func(t *testing.T) {
		s := NewMemoryStore()
		testA2AReplayContract(t, []A2AReplayStore{s, NewAuthorityStore(s, nil, "")})
		require.LessOrEqual(t, len(s.a2aReplays), 3, "expired keys must be physically pruned")
		fresh := NewMemoryStore()
		owner, _, err := fresh.AcquireA2AReplay("race", "same-key", time.Now(), time.Hour)
		require.NoError(t, err)
		require.NotEmpty(t, owner, "memory is documented process-lifetime only")
	})
	t.Run("sqlite-independent-handles", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "registry.db")
		first, err := NewSQLiteStore(path)
		require.NoError(t, err)
		second, err := NewSQLiteStore(path)
		require.NoError(t, err)
		defer func() { _ = second.Close() }()
		testA2AReplayContract(t, []A2AReplayStore{first, NewAuthorityStore(second, nil, "")})
		require.NoError(t, first.Close())
		reopened, err := NewSQLiteStore(path)
		require.NoError(t, err)
		defer func() { _ = reopened.Close() }()
		owner, raw, err := reopened.AcquireA2AReplay("race", "same-key", time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC), time.Hour)
		require.NoError(t, err)
		require.Empty(t, owner)
		require.Equal(t, "one", string(raw))
	})
}

func TestSQLiteMigration018Upgrade(t *testing.T) {
	path, db := sqliteMigrationFixture(t, 17)
	_, err := db.Exec(`INSERT INTO agents(id,registered_at,last_seen) VALUES('pre-replay',1,1)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	assertSQLiteMigrationVersions(t, store)
	_, err = store.Get("pre-replay")
	require.NoError(t, err)
	owner, _, err := store.AcquireA2AReplay("pre-replay", "key", time.Now(), time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, owner)
}
