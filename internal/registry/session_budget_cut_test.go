package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

func TestSessionDispatchRetainsBlockingEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		status         int
		cut, permanent bool
	}{
		{"refusal", http.StatusBadRequest, false, true},
		{"503", http.StatusServiceUnavailable, false, false},
		{"upstream timeout", http.StatusGatewayTimeout, true, false},
		{"partial 200", http.StatusOK, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"reply":"incomplete exchange"}`))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			t.Cleanup(receiver.Close)
			store := NewMemoryStore()
			require.NoError(t, store.Register(&Agent{ID: "receiver", Webhook: &webhook.Config{URL: receiver.URL}}))
			h := NewHandler(store)
			h.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(time.Second, nil), webhook.NewMemoryQueue(), webhook.DefaultDriverConfig()))
			result, err := NewSessionDeliverer(h).DispatchDelivery(context.Background(), "receiver", DeliveryRequest{
				Entry: &InboxEntry{ID: "message.receiver", Payload: []byte(`{"text":"hello"}`)}, TimeoutMs: 100,
			})
			require.Error(t, err)
			var dispatch *DispatchError
			var blocking *webhook.BlockingDeliveryError
			require.ErrorAs(t, err, &dispatch)
			require.ErrorAs(t, err, &blocking, "do not flatten timeout vs refusal evidence")
			require.Equal(t, 1, blocking.Attempts)
			require.Equal(t, 100*time.Millisecond, blocking.Budget)
			require.Equal(t, tc.cut, blocking.Cut)
			require.Equal(t, tc.permanent, errors.Is(err, webhook.ErrPermanent))
			require.Equal(t, !tc.permanent, blocking.BudgetExhausted)
			require.Empty(t, result.Reply, "partial answers cannot enter the successful receipt cache")
			if tc.status == http.StatusOK {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
		})
	}
}
