package federation

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

// Use real HTTP: the parser folds differently spelled keys into one ordered
// slice. The audit must retain the spellings before that information is lost.
func TestForwardReceiptIngressWireValues(t *testing.T) {
	const canonical = IngressVerdictHeader
	const variant = "X-CRIER-FED-INGRESS-VERDICT"
	for _, tc := range []struct {
		name    string
		mutate  func(http.Header)
		want    []string
		primary string
		keys    []string
	}{
		{"CanonicalOnly", func(h http.Header) { h.Set(canonical, string(FedTokenOnly)) }, []string{string(FedTokenOnly)}, string(FedTokenOnly), nil},
		{"CanonicalAndDifferentCaseConflict", func(h http.Header) {
			h.Set(canonical, string(FedTokenOnly))
			h[variant] = []string{string(FedNoLink)}
		}, []string{string(FedNoLink), string(FedTokenOnly)}, "", []string{variant, canonical}},
		{"CanonicalAndDifferentCaseDuplicate", func(h http.Header) {
			h.Set(canonical, string(FedTokenOnly))
			h[variant] = []string{string(FedTokenOnly)}
		}, []string{string(FedTokenOnly), string(FedTokenOnly)}, string(FedTokenOnly), []string{variant, canonical}},
		{"SetThenDel", func(h http.Header) { h.Set(canonical, string(FedNoLink)); h.Del(canonical) }, nil, "", nil},
		{"CasingVariantOnly", func(h http.Header) { h[variant] = []string{string(FedTokenOnly)} }, []string{string(FedTokenOnly)}, string(FedTokenOnly), []string{variant}},
		{"ThreeValuesPreserveOrder", func(h http.Header) {
			h.Set(canonical, string(FedTokenOnly))
			h[variant] = []string{string(FedNoLink), string(FedBadSignature)}
		}, []string{string(FedNoLink), string(FedBadSignature), string(FedTokenOnly)}, "", []string{variant, canonical}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			finished := make(chan struct{})
			// A bare peer handler deliberately permits absent/variant-only headers;
			// DeliveryAuth restores the canonical ingress header by design (432).
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer close(finished)
				tc.mutate(w.Header())
				attrs := ingressVerdictAuditAttrs(w.Header())
				slog.Info("peer wire", attrs...)
				w.Header().Set(InboundVerdictHeader, string(FedAllowed))
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, " {\"id\":\"untouched\"} \n")
			}))
			defer srv.Close()
			c := NewClient(nil, 0, "")
			receipt, err := c.ForwardToURLReceiptWithNamespaceToken(t.Context(), srv.URL, "target", []byte(`{"payload":{}}`), "")
			require.NoError(t, err)
			<-finished
			require.Equal(t, http.StatusCreated, receipt.StatusCode)
			require.Equal(t, " {\"id\":\"untouched\"} \n", string(receipt.Body))
			require.Equal(t, tc.primary, receipt.IngressVerdict)
			require.Equal(t, tc.want, receipt.IngressVerdictValues)
			require.Equal(t, len(tc.want) > 1, receipt.IngressVerdictAnomaly)
			require.Equal(t, string(FedAllowed), receipt.InboundVerdict, "do not replace or fall back to the alias")
			var audit map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit))
			if len(tc.want) > 1 {
				require.Equal(t, jsonStrings(receipt.IngressVerdictValues), audit["ingress_verdict_values"])
				require.Equal(t, true, audit["ingress_verdict_anomaly"])
			} else {
				require.NotContains(t, audit, "ingress_verdict_values")
				require.NotContains(t, audit, "ingress_verdict_anomaly")
			}
			if tc.keys == nil {
				require.NotContains(t, audit, "ingress_verdict_keys")
			} else {
				require.Equal(t, jsonStrings(tc.keys), audit["ingress_verdict_keys"])
			}
		})
	}
}

func TestForwardReceiptIngressAgreesWithCompletedAudit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		same       bool
		mirrorPeer bool
	}{
		{"ConflictingValues", false, true},
		{"IdenticalDuplicates", true, true},
		{"OriginalIngressOnlyFinding", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			value := string(FedNoLink)
			if tc.same {
				value = string(FedTokenOnly)
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Mirror the same handler answer into both channels so the receipt
				// can be compared with the production peer_verdict_values audit.
				w.Header().Set(IngressVerdictHeader, string(FedNoLink))
				w.Header()["X-CRIER-FED-INGRESS-VERDICT"] = []string{value}
				if tc.mirrorPeer {
					w.Header()["X-CRIER-FED-VERDICT"] = []string{value}
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"id":"receipt"}`)
			})
			gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return nil })(next)
			finished := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				gate.ServeHTTP(w, r)
			}))
			defer srv.Close()
			c := NewClient(nil, 0, "deployment")
			receipt, err := c.ForwardToURLReceiptWithNamespaceToken(t.Context(), srv.URL, "target", []byte(`{"payload":{}}`), "")
			require.NoError(t, err)
			<-finished
			require.Equal(t, http.StatusCreated, receipt.StatusCode)
			require.Equal(t, []string{value, string(FedTokenOnly)}, receipt.IngressVerdictValues)
			require.True(t, receipt.IngressVerdictAnomaly)
			if tc.same {
				require.Equal(t, string(FedTokenOnly), receipt.IngressVerdict)
			} else {
				require.Empty(t, receipt.IngressVerdict)
			}
			var audit map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit))
			if tc.mirrorPeer {
				require.Equal(t, jsonStrings(receipt.IngressVerdictValues), audit["peer_verdict_values"])
			} else {
				require.NotContains(t, audit, "peer_verdict")
				require.NotContains(t, audit, "peer_verdict_source")
				require.NotContains(t, audit, "peer_verdict_values")
				require.Equal(t, string(FedTokenOnly), receipt.Verdict)
			}
			require.Equal(t, jsonStrings(receipt.IngressVerdictValues), audit["ingress_verdict_values"])
			require.Equal(t, true, audit["ingress_verdict_anomaly"])
			require.Equal(t, jsonStrings([]string{"X-CRIER-FED-INGRESS-VERDICT", IngressVerdictHeader}), audit["ingress_verdict_keys"])
			require.Equal(t, string(FedTokenOnly), audit["verdict"])
			require.Equal(t, true, audit["receipt_flushed"])
		})
	}
}

func TestForwardReceiptIngressValuesContractDocumented(t *testing.T) {
	for _, path := range []string{"../../specs/CHAT-FEDERATION.md", "../../docs/openapi.yaml"} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, field := range []string{"DF-CRIER-457", "IngressVerdictValues", "IngressVerdictAnomaly", "ingress_verdict_values", "ingress_verdict_keys"} {
			require.Contains(t, string(data), field, path)
		}
	}
}
