package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDispatchRejectsInvalidTTL(t *testing.T) {
	t.Parallel()
	for _, ttl := range []int{-1, int(maxTTLSeconds + 1)} {
		t.Run(fmt.Sprint(ttl), func(t *testing.T) {
			store := NewMemoryStore()
			target := &Agent{ID: "receiver"}
			require.NoError(t, store.Register(target))
			h := NewHandler(store)
			result, err := h.Dispatch(context.Background(), target, DeliveryRequest{
				Entry: &InboxEntry{ID: "bad-ttl", Payload: []byte(`{"text":"hello"}`), TTLSeconds: &ttl},
			})
			var refused *DispatchError
			require.True(t, errors.As(err, &refused), "expected refusal, got %+v / %v", result, err)
			require.Equal(t, http.StatusBadRequest, refused.StatusCode)
			want := "ttl_seconds must not be negative"
			if ttl > 0 {
				want = fmt.Sprintf("ttl_seconds must be <= %d", maxTTLSeconds)
			}
			require.Equal(t, map[string]string{"error": want}, refused.Response)
			depth, leased, _, err := store.Stats(target.ID)
			require.NoError(t, err)
			require.Zero(t, depth+leased, "invalid TTL must never reach the store")
		})
	}
}

func TestDispatchNamespaceRefusalMatchesHTTP(t *testing.T) {
	t.Parallel()
	f := newNSHandler(t, `{"namespaces":[{"name":"interactive"},{"name":"batch"}]}`)
	target := &Agent{ID: "receiver", Namespace: "interactive"}
	require.NoError(t, f.store.Register(target))
	httpReply := f.do(t, http.MethodPost, "/agents/receiver/inbox", `{"payload":{},"namespace":"batch"}`, nil)
	require.Equal(t, http.StatusForbidden, httpReply.Code, "%s", httpReply.Body.String())
	var want map[string]string
	require.NoError(t, json.Unmarshal(httpReply.Body.Bytes(), &want))
	require.Equal(t, map[string]string{
		"error":  "NAMESPACE_MISMATCH",
		"detail": `target "receiver" is in namespace "interactive"; a message cannot cross namespaces ("batch" requested)`,
	}, want)
	result, err := f.handler.Dispatch(context.Background(), target, DeliveryRequest{
		Entry: &InboxEntry{ID: "cross-realm", Payload: []byte(`{}`)}, Namespace: "batch",
	})
	var refused *DispatchError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, httpReply.Code, result.StatusCode)
	require.Equal(t, want, refused.Response)
	depth, leased, _, err := f.store.Stats(target.ID)
	require.NoError(t, err)
	require.Zero(t, depth+leased)
}
