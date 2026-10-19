package registry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProducerDelivererEntryIdentityAndRefusals(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store)
	require.ErrorContains(t, h.Deliver("target", nil), "nil delivery entry")
	require.ErrorIs(t, h.Deliver("missing", &InboxEntry{Payload: []byte(`{"text":"hello"}`)}), ErrAgentNotFound)
	require.NoError(t, store.Register(&Agent{ID: "target"}))
	for _, id := range []string{"", "existing-id"} {
		entry := &InboxEntry{ID: id, Payload: []byte(`{"text":"hello"}`)}
		require.NoError(t, h.Deliver("target", entry))
		require.Equal(t, "target", entry.AgentID)
		require.NotEmpty(t, entry.ID)
		if id != "" {
			require.Equal(t, id, entry.ID)
		}
	}
	depth, leased, _, err := store.Stats("target")
	require.NoError(t, err)
	require.Equal(t, 2, depth+leased)
}
