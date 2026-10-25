package session

import (
	"testing"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

func TestProgressDeliveryClasses(t *testing.T) {
	t.Parallel()
	store := registry.NewMemoryStore()
	for _, agent := range []*registry.Agent{
		{ID: "blocking", Webhook: &webhook.Config{URL: "http://127.0.0.1:39999", DeliveryMode: "blocking"}},
		{ID: "queue"},
	} {
		require.NoError(t, store.Register(agent))
	}
	h := &Handler{opts: HTTPOptions{Agents: store}}
	require.Equal(t, map[string]string{"blocking": "blocking_webhook", "queue": "inbox", "peer/bot": "federated"},
		h.deliveryClasses([]string{"blocking", "queue", "missing"}, []string{"peer/bot"}))
	h.opts.Agents = nil
	require.Equal(t, map[string]string{"peer/bot": "federated"}, h.deliveryClasses([]string{"blocking"}, []string{"peer/bot"}))
}
