package registry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
)

func TestInbound371RegistryReceiptsAndSingleAudit(t *testing.T) {
	a, b := newKeyedRelay(t, "a"), newKeyedRelay(t, "b")
	declarePeer(t, a, b)
	declarePeer(t, b, a)
	linkRelay(t, a, b)
	linkRelay(t, b, a)
	require.NoError(t, b.store.Register(&Agent{ID: "target"}))
	c := forwardClient(a, b)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	t.Run("unmarked_direct_handler", func(t *testing.T) {
		logs.Reset()
		rec := deliverTo(t, b.router, "target", []byte(`{"payload":{"text":"compatibility"}}`), nil)
		assertFedVerdict(t, rec, 201, federation.FedTokenOnly)
		require.NotContains(t, rec.Body.String(), `"verdict"`)
		require.Equal(t, "FED_TOKEN_ONLY", rec.Header().Get("X-Crier-Fed-Inbound-Verdict"))
		require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte(`"msg":"federation inbound delivery"`)), logs.String())
	})
	t.Run("forward_preserves_both_decisions", func(t *testing.T) {
		logs.Reset()
		rec := deliverTo(t, a.router, "target", []byte(`{"payload":{"text":"forwarded"}}`), nil)
		assertFedVerdict(t, rec, 201, federation.FedAllowed)
		require.Equal(t, "FED_TOKEN_ONLY", rec.Header().Get("X-Crier-Fed-Inbound-Verdict"))
		var receipt map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &receipt))
		require.NotContains(t, receipt, "verdict")
		require.NotContains(t, receipt, "inbound_verdict")
		require.Equal(t, 2, bytes.Count(logs.Bytes(), []byte(`"msg":"federation inbound delivery"`)), logs.String())
	})
	for _, tc := range []struct {
		name   string
		revoke bool
		want   federation.Verdict
		status int
	}{
		{"signed_over_http", false, federation.FedAllowed, 201},
		{"revoked_over_http", true, federation.FedPeerRevoked, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.revoke {
				state := "revoked"
				_, err := b.peers.Patch(a.id, federation.PeerPatch{State: &state})
				require.NoError(t, err)
			}
			before := fedQueueDepth(t, b.store, "target")
			logs.Reset()
			receipt, err := c.ForwardToURLReceiptWithNamespaceToken(t.Context(), b.server.URL, "target", []byte(`{"payload":{"text":"signed"}}`), "")
			require.NoError(t, err)
			require.Equal(t, tc.status, receipt.StatusCode, string(receipt.Body))
			require.Equal(t, string(tc.want), receipt.Verdict)
			require.Equal(t, string(tc.want), receipt.InboundVerdict)
			var body map[string]any
			require.NoError(t, json.Unmarshal(receipt.Body, &body))
			if tc.revoke {
				require.Equal(t, string(tc.want), body["verdict"], "RefuseDelivery authors the refusal body")
			} else {
				require.NotContains(t, body, "verdict", "successful handler receipt is not annotated")
			}
			require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte(`"msg":"federation inbound delivery"`)), logs.String())
			require.Contains(t, logs.String(), `"verdict":"`+string(tc.want)+`"`)
			if tc.revoke {
				require.Equal(t, before, fedQueueDepth(t, b.store, "target"))
			} else {
				require.Equal(t, before+1, fedQueueDepth(t, b.store, "target"))
			}
		})
	}
}
