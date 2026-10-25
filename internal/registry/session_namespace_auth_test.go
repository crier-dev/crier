package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/namespace"
)

func TestSessionDispatchTargetNamespaceAuth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		targetNS  string
		requestNS string
		token     string
		status    int
		code      string
	}{
		{name: "missing_token", targetNS: "private", status: http.StatusUnauthorized, code: "NAMESPACE_UNAUTHORIZED"},
		{name: "wrong_token", targetNS: "private", token: "wrong", status: http.StatusUnauthorized, code: "NAMESPACE_UNAUTHORIZED"},
		{name: "correct_token", targetNS: "private", token: "realm-secret", status: http.StatusCreated},
		{name: "explicit_namespace", targetNS: "private", requestNS: "private", token: "realm-secret", status: http.StatusCreated},
		{name: "mismatch_precedes_auth", targetNS: "private", requestNS: "default", status: http.StatusForbidden, code: "NAMESPACE_MISMATCH"},
		{name: "default_no_token", status: http.StatusCreated},
		{name: "canonical_default_no_token", targetNS: "default", requestNS: "default", status: http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies, err := namespace.Parse(`{"namespaces":[{"name":"private","auth":"token","token_ref":"env:TARGET_REALM_TOKEN"}]}`, func(key string) (string, bool) {
				return "realm-secret", key == "TARGET_REALM_TOKEN"
			})
			require.NoError(t, err)
			store := NewMemoryStore()
			require.NoError(t, store.Register(&Agent{ID: "target", Namespace: tc.targetNS}))
			h := NewHandler(store)
			h.SetNamespacePolicies(policies)
			entry := &InboxEntry{ID: "message.target", Payload: json.RawMessage(`{"text":"hello"}`)}
			result, err := NewSessionDeliverer(h).DispatchDelivery(context.Background(), "target", DeliveryRequest{
				Entry: entry, SessionID: "session", Namespace: tc.requestNS, NamespaceToken: tc.token,
			})
			require.Equal(t, tc.status, result.StatusCode)
			if tc.code != "" {
				var refusal *DispatchError
				require.True(t, errors.As(err, &refusal), "expected DispatchError, got %v", err)
				require.Equal(t, tc.status, refusal.StatusCode)
				body, ok := refusal.Response.(map[string]string)
				require.True(t, ok)
				require.Equal(t, tc.code, body["error"])
				if tc.code == "NAMESPACE_UNAUTHORIZED" {
					require.Contains(t, body["detail"], `namespace "private"`)
					// Compare against the actual HTTP ingress contract, not an
					// independently reimplemented expectation of that contract.
					fixture := newNSHandler(t, "")
					fixture.handler.SetNamespacePolicies(policies)
					require.NoError(t, fixture.store.Register(&Agent{ID: "target", Namespace: tc.targetNS}))
					rec := fixture.deliver(t, "target", tc.requestNS, `{"text":"hello"}`, map[string]string{namespace.HeaderNamespaceToken: tc.token})
					require.Equal(t, result.StatusCode, rec.Code)
					var httpBody map[string]string
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpBody))
					require.Equal(t, httpBody, body)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, namespace.Canonical(tc.targetNS), entry.Namespace)
				require.Equal(t, namespace.Canonical(tc.targetNS), entry.Location.Namespace)
			}
			depth, leased, _, err := store.Stats("target")
			require.NoError(t, err)
			wantEntries := 0
			if tc.code == "" {
				wantEntries = 1
			}
			require.Equal(t, wantEntries, depth+leased)
		})
	}
}

func TestDeliveryRequestDoesNotSerializeNamespaceToken(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(DeliveryRequest{NamespaceToken: "caller-credential"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "caller-credential")
	require.NotContains(t, string(raw), "NamespaceToken")
}
