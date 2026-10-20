package federation

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

// DF-CRIER-418: ingress classification belongs in headers, not in a rewritten
// handler response. Use real HTTP so the peer observes committed wire headers
// and bytes, including whitespace, duplicate keys, and non-object bodies.
func TestDeliveryAuthPreservesHandlerBodyBytes(t *testing.T) {
	h, _, _ := handshakeFixture(t)
	require.Positive(t, h.Store.InboundPosture().Peers, "exercise the former body-annotation branch")
	for _, path := range []string{"/agents/target/inbox", "/capabilities/tool/inbox"} {
		for _, tc := range []struct {
			name, body, outbound string
			status               int
		}{
			{name: "plain", body: `{"id":"receipt"}`, status: http.StatusCreated},
			{name: "formatting", body: " { \"z\": 1, \"id\": \"receipt\" }\n\n", status: http.StatusAccepted},
			{name: "same_verdict", body: `{"verdict":"FED_TOKEN_ONLY","id":"receipt"}`, status: http.StatusCreated},
			{name: "outbound_refusal", body: `{"verdict":"FED_NO_LINK","inbound_verdict":"handler-owned"}`, outbound: string(FedNoLink), status: http.StatusForbidden},
			{name: "duplicate_keys", body: `{"id":"first","id":"second"}`, status: http.StatusCreated},
			{name: "array", body: "[1, 2]\n", status: http.StatusCreated},
			{name: "non_json", body: "not json\x00\xff\n", status: http.StatusCreated},
			{name: "empty", status: http.StatusOK},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(old) })
				next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
					w.Header().Set("X-Handler-Receipt", "unchanged")
					if tc.outbound != "" {
						w.Header().Set(VerdictHeader, tc.outbound)
					}
					if tc.status != http.StatusOK {
						w.WriteHeader(tc.status)
					}
					// Multiple writes must not alter chunk boundaries in the entity bytes.
					mid := len(tc.body) / 2
					_, _ = io.WriteString(w, tc.body[:mid])
					_, _ = io.WriteString(w, tc.body[mid:])
				})
				gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(next)
				finished := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(finished)
					gate.ServeHTTP(w, r)
				}))
				defer server.Close()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, strings.NewReader(`{"payload":{}}`))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer deployment")
				resp, err := server.Client().Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				<-finished // order audit-buffer reads after the server's final log write
				t.Logf("handler_bytes=%q peer_bytes=%q", tc.body, body)
				t.Logf("headers=%s:%s %s:%s", VerdictHeader, resp.Header.Get(VerdictHeader), "X-Crier-Fed-Inbound-Verdict", resp.Header.Get("X-Crier-Fed-Inbound-Verdict"))
				t.Logf("audit=%s", logs.String())
				require.Equal(t, tc.body, string(body), "the middleware must not parse, re-encode or annotate the handler body")
				require.Equal(t, tc.status, resp.StatusCode)
				require.Equal(t, "unchanged", resp.Header.Get("X-Handler-Receipt"))
				require.Equal(t, strconv.Itoa(len(tc.body)), resp.Header.Get("Content-Length"))
				wantVerdict := string(FedTokenOnly)
				if tc.outbound != "" {
					wantVerdict = tc.outbound
				}
				require.Equal(t, wantVerdict, resp.Header.Get(VerdictHeader))
				require.Equal(t, string(FedTokenOnly), resp.Header.Get(InboundVerdictHeader))
				// DF-CRIER-432: the ingress verdict always has an unshadowable home,
				// so a handler verdict on the peer-facing header loses nothing.
				require.Equal(t, string(FedTokenOnly), resp.Header.Get(IngressVerdictHeader))
				var audit map[string]any
				require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit), "exactly one audit line")
				require.Equal(t, "federation inbound delivery", audit["msg"])
				require.Equal(t, string(FedTokenOnly), audit["verdict"])
				require.Equal(t, true, audit["receipt_flushed"])
				require.EqualValues(t, resp.StatusCode, audit["status"])
				if tc.outbound == "" {
					require.NotContains(t, audit, "peer_verdict",
						"no handler verdict: the ingress value IS what the peer received, named once")
				} else {
					require.Equal(t, tc.outbound, audit["peer_verdict"],
						"the line names the value the peer received on the peer-facing header")
					require.Equal(t, "handler", audit["peer_verdict_source"])
				}
			})
		}
	}
}
