package dispatchlive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProgressDeliveryClassSnapshot(t *testing.T) {
	r := NewRegistry()
	classes := map[string]string{"peer/bot": "federated", "slow": "blocking_webhook", "queue": "inbox"}
	r.BeginWithClasses("room", "msg", 1, []string{"peer/bot", "slow", "queue", "unknown"}, 120000, time.Now(), classes)
	classes["peer/bot"] = "blocking_webhook"
	pass := r.Live("room", "msg")
	require.Equal(t, 120000, pass.BudgetMs)
	got := map[string]string{}
	for _, target := range pass.Targets {
		got[target.Target] = target.DeliveryClass
		require.Zero(t, target.Attempts)
		require.Empty(t, target.Class, "path classification is not attempt failure evidence")
	}
	require.Equal(t, map[string]string{"peer/bot": "federated", "slow": "blocking_webhook", "queue": "inbox", "unknown": ""}, got)
	raw, err := json.Marshal(pass)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"delivery_class":"federated"`)
	require.Contains(t, string(raw), `"delivery_class":"blocking_webhook"`)
	pass.Targets[0].DeliveryClass = "mutated"
	require.Equal(t, "federated", r.Live("room", "msg").Targets[0].DeliveryClass)
}
