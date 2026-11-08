package registry

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestArchiveHookRetainsRefusalsAndDefaultSweepMatchesLegacy(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	old := time.Now().Add(-DefaultDeadLetterRetention - time.Hour).UTC()
	fresh := time.Now().UTC()
	for _, dl := range []*DeadLetter{
		{MessageID: "expired", AgentID: "agent", Payload: []byte("cold"), CreatedAt: old, ExpiredAt: old, DeadLetteredAt: old, Reason: "expired"},
		{MessageID: "fresh", AgentID: "agent", Payload: []byte("hot"), CreatedAt: fresh, ExpiredAt: fresh, DeadLetteredAt: fresh, Reason: "expired"},
	} {
		_, err := s.AppendDeadLetter(dl)
		require.NoError(t, err)
	}
	before, err := s.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Len(t, before, 2)
	removed := s.PurgeExpiredDeadLetters(func(*DeadLetter) bool { return false })
	require.Zero(t, removed)
	afterRefusal, err := s.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Equal(t, before, afterRefusal, "refusal must leave the hot-store table byte-equivalent at the record level")
	// Disabled/default mode passes no archive callback and must retain the old
	// bare-delete result: expired rows disappear; fresh rows remain unchanged.
	require.Equal(t, 1, s.PurgeExpiredDeadLetters(nil))
	afterDefault, err := s.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Len(t, afterDefault, 1)
	require.Equal(t, "fresh", afterDefault[0].MessageID)
}
