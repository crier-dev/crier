package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInboxAcceptanceDetectorVocabulary(t *testing.T) {
	verdict, transport := deliveryVerdict(http.StatusCreated, "")
	require.Equal(t, "inbox_accepted", verdict, "storing a message is not observing an answer")
	require.Equal(t, "inbox", transport)
	require.Equal(t, transport, transportForVerdict(verdict))
	// Explicit overrides must retain the same transport mapping too.
	overrideVerdict, overrideTransport := deliveryVerdict(http.StatusOK, verdict)
	require.Equal(t, verdict, overrideVerdict)
	require.Equal(t, transport, overrideTransport)
}

func TestForbiddenResponseEvidence(t *testing.T) {
	verdict, transport := deliveryVerdict(http.StatusForbidden, "")
	require.Equal(t, "forbidden_unclassified", verdict, "status alone establishes no cause")
	require.Empty(t, transport)
	for _, body := range []string{
		`{"error":"new refusal, not GUARD_BLOCKED"}`,
		`{"error":{"code":"GUARD_BLOCKED"}}`,
		`{"detail":"GUARD_BLOCKED"}`,
		`GUARD_BLOCKED`,
		`{"error":"GUARD_BLOCKED"`,
		`{"error":"GUARD_BLOCKED","other":broken}`,
		`{"error":""}`,
		``,
	} {
		t.Run(body, func(t *testing.T) {
			got, transport, detail := deliveryResponseVerdict(http.StatusForbidden, "", []byte(body))
			require.Equal(t, "forbidden_unclassified", got)
			require.Empty(t, transport)
			if body == `{"error":"new refusal, not GUARD_BLOCKED"}` {
				require.Equal(t, "new refusal, not GUARD_BLOCKED", detail)
			} else {
				require.Equal(t, body, detail, "unreadable or absent error preserves the raw body")
			}
		})
	}
	for _, explicit := range []string{VerdictQuarantined, VerdictFedPeerUntrusted, VerdictFedNamespaceRefused, VerdictFedAgentRefused} {
		got, transport, detail := deliveryResponseVerdict(http.StatusForbidden, explicit, []byte(`{"error":"POLICY_REFUSAL"}`))
		require.Equal(t, explicit, got)
		require.Equal(t, transportForVerdict(explicit), transport)
		require.Equal(t, "POLICY_REFUSAL", detail)
	}
	got, transport, detail := deliveryResponseVerdict(http.StatusCreated, "", []byte(`{"error":"GUARD_BLOCKED"}`))
	require.Equal(t, VerdictInboxAccepted, got)
	require.Equal(t, "inbox", transport)
	require.Empty(t, detail, "successful payloads cannot become refusal evidence")
}

func TestStatusRecorderCapturesOnlyForbiddenBody(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusCreated, http.StatusOK} {
		w := httptest.NewRecorder()
		rec := newStatusRecorder(w)
		rec.WriteHeader(code)
		for _, chunk := range []string{`{"error":`, `"DELIVERY_FORBIDDEN"}`} {
			n, err := rec.Write([]byte(chunk))
			require.NoError(t, err)
			require.Equal(t, len(chunk), n)
		}
		require.Equal(t, code, rec.status())
		require.Equal(t, `{"error":"DELIVERY_FORBIDDEN"}`, w.Body.String())
		if code == http.StatusForbidden {
			require.Equal(t, w.Body.Bytes(), rec.forbiddenBody)
		} else {
			require.Empty(t, rec.forbiddenBody)
		}
	}
	rec := newStatusRecorder(httptest.NewRecorder())
	_, err := rec.Write([]byte(`{"error":"GUARD_BLOCKED"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.status())
	require.Empty(t, rec.forbiddenBody)
}
