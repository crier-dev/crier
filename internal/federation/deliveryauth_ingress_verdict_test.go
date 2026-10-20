package federation

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

// DF-CRIER-432: the ingress classification must survive a handler that writes the
// same header. Real HTTP, because the claim is about the headers the PEER reads —
// a handler write that lands after the response is committed is not a shadow.
func TestIngressVerdictSurvivesHandlerHeader(t *testing.T) {
	h, _, _ := handshakeFixture(t)
	require.Positive(t, h.Store.InboundPosture().Peers, "records present: token-only ingress is classified")
	const body = `{"id":"receipt"}`
	for _, tc := range []struct {
		name, wantPeer, wantSource string
		mutate                     func(http.Header)
	}{
		{
			// The reviewer fixture's case: an onward peer receipt replaces the
			// peer-facing header. That stays legitimate; the ingress verdict moves
			// to a header the handler cannot reach.
			name:       "handler_sets_onward_verdict",
			mutate:     func(hd http.Header) { hd.Set(VerdictHeader, string(FedNoLink)) },
			wantPeer:   string(FedNoLink),
			wantSource: verdictSourceHandler,
		},
		{
			// A handler aiming at the classification itself cannot reach it.
			name: "handler_shadows_both_ingress_headers",
			mutate: func(hd http.Header) {
				hd.Set(IngressVerdictHeader, string(FedNoLink))
				hd.Set(InboundVerdictHeader, string(FedNoLink))
			},
			wantPeer: string(FedTokenOnly),
		},
		{
			// Removing the peer-facing header is a handler decision, visible on the
			// wire and in the audit line, not silently repaired into a verdict the
			// handler did not author.
			name:       "handler_removes_the_peer_header",
			mutate:     func(hd http.Header) { hd.Del(VerdictHeader) },
			wantPeer:   "",
			wantSource: verdictSourceHandler,
		},
		{
			// Writing the ingress value back is indistinguishable from writing
			// nothing, which is the honest reading: one verdict, one author.
			name:     "handler_sets_the_same_value",
			mutate:   func(hd http.Header) { hd.Set(VerdictHeader, string(FedTokenOnly)) },
			wantPeer: string(FedTokenOnly),
		},
		{
			name:     "handler_writes_nothing",
			mutate:   func(http.Header) {},
			wantPeer: string(FedTokenOnly),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })

			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Handler-Receipt", "unchanged")
				tc.mutate(w.Header())
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			})
			gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(next)
			finished := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				gate.ServeHTTP(w, r)
			}))
			defer server.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/agents/target/inbox", strings.NewReader(`{"payload":{}}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer deployment")
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			<-finished // order audit-buffer reads after the server's final log write

			t.Logf("peer_headers=%s:%s %s:%s %s:%s", VerdictHeader, resp.Header.Get(VerdictHeader),
				IngressVerdictHeader, resp.Header.Get(IngressVerdictHeader),
				InboundVerdictHeader, resp.Header.Get(InboundVerdictHeader))
			t.Logf("audit=%s", strings.TrimSpace(logs.String()))

			// DF-CRIER-418's guarantee, unchanged: the handler's body is flushed
			// byte-for-byte in every posture.
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			require.Equal(t, body, string(got), "the middleware must not parse, re-encode or annotate the handler body")
			require.Equal(t, "unchanged", resp.Header.Get("X-Handler-Receipt"))

			// The documented precedence, as the peer observes it.
			require.Equal(t, tc.wantPeer, resp.Header.Get(VerdictHeader),
				"the peer-facing header is the handler's when it sets one, else the ingress verdict")
			require.Equal(t, string(FedTokenOnly), resp.Header.Get(IngressVerdictHeader),
				"the authoritative ingress verdict cannot be shadowed by a handler write")
			require.Equal(t, string(FedTokenOnly), resp.Header.Get(InboundVerdictHeader),
				"the alias carries the same captured state, never a handler write")

			var audit map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit), "exactly one audit line")
			require.Equal(t, "federation inbound delivery", audit["msg"])
			require.Equal(t, string(FedTokenOnly), audit["verdict"], "`verdict` is always the ingress classification")
			require.Equal(t, true, audit["receipt_flushed"])
			if tc.wantSource == "" {
				require.NotContains(t, audit, "peer_verdict",
					"no handler verdict: the ingress value IS what the peer received, so the line names one verdict")
				require.NotContains(t, audit, "peer_verdict_source")
			} else {
				require.Equal(t, tc.wantPeer, audit["peer_verdict"],
					"a handler verdict is named with the value the peer received")
				require.Equal(t, tc.wantSource, audit["peer_verdict_source"])
			}
		})
	}
}

// The refusal path keeps its OWN verdict — no handler ran, RefuseDelivery authors
// the receipt — and the ingress headers carry that same refusal verdict, so a
// peer never has to guess which of them decided.
func TestIngressVerdictOnRefusalKeepsRefusalVerdict(t *testing.T) {
	h, _, _ := handshakeFixture(t)
	h.Store.SetRequirePeer(true)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a keyless request bypassed strict peer policy")
	}))
	r := httptest.NewRequest(http.MethodPost, "/agents/target/inbox", strings.NewReader(`{"payload":{}}`))
	r.Header.Set("Authorization", "Bearer deployment")
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), string(FedNoLink))
	require.Equal(t, string(FedNoLink), w.Header().Get(VerdictHeader))
	require.Equal(t, string(FedNoLink), w.Header().Get(IngressVerdictHeader))
	require.Equal(t, string(FedNoLink), w.Header().Get(InboundVerdictHeader))

	var audit map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit), "exactly one audit line")
	require.Equal(t, string(FedNoLink), audit["verdict"])
	require.Equal(t, float64(http.StatusForbidden), audit["status"])
	require.Equal(t, true, audit["receipt_flushed"])
	require.NotContains(t, audit, "peer_verdict", "refusal verdict == ingress verdict: one verdict, not two")
	t.Logf("refusal audit=%s", strings.TrimSpace(logs.String()))
}

// The precedence is a documented contract, and the documents are pinned to the
// wire claim so the prose cannot drift away from what a peer receives.
func TestIngressVerdictPrecedenceIsDocumented(t *testing.T) {
	spec, err := os.ReadFile("../../specs/CHAT-SESSIONS.md")
	require.NoError(t, err)
	require.Contains(t, string(spec), "`X-Crier-Fed-Ingress-Verdict` carries the local ingress verdict")
	require.Contains(t, string(spec), "`X-Crier-Fed-Verdict` is the hop's peer-facing verdict")
	require.Contains(t, string(spec), "otherwise the ingress verdict",
		"the precedence must be stated, not implied by the seeding order")
	require.Contains(t, string(spec), `peer_verdict_source: "handler"`)

	federation, err := os.ReadFile("../../specs/CHAT-FEDERATION.md")
	require.NoError(t, err)
	require.Contains(t, string(federation), "X-Crier-Fed-Ingress-Verdict")
	// DF-CRIER-440: the spec must name the field that reads the authoritative
	// header, so the prose cannot drift from what the code reads back.
	require.Contains(t, string(federation), "ForwardReceipt.IngressVerdict")

	openapi, err := os.ReadFile("../../docs/openapi.yaml")
	require.NoError(t, err)
	require.Contains(t, string(openapi), "X-Crier-Fed-Ingress-Verdict carries the local ingress")
}
