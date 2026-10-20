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

// jsonStrings renders a want-slice in the shape encoding/json decodes into, so a
// case can declare its expectation once as plain strings.
func jsonStrings(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// DF-CRIER-436: the audit must see EVERY verdict the peer can read. Header.Get and
// Header.Values resolve one map entry — the canonical spelling — so an appended
// second value, a raw map write under a differently cased key, and a duplicated
// spelling were all invisible to the audit while net/http put them on the wire:
// the peer could act on a handler-authored verdict the audit never named.
//
// Every case therefore runs real HTTP and asserts the wire the client parses and
// the audit line side by side. `wantWire` is exactly resp.Header.Values(...), i.e.
// what a flipping client reads (first value == Header.Get, last value == what a
// last-wins client honours); `wantFirst` pins the first element so the wire order
// claim is not tautological.
func TestPeerVerdictAuditNamesEveryWireValue(t *testing.T) {
	peers, _, _ := handshakeFixture(t)
	require.Positive(t, peers.Store.InboundPosture().Peers, "records present: token-only ingress is classified")
	const body = `{"id":"receipt"}`
	replaced := string(FedNoLink)
	ingress := string(FedTokenOnly)

	for _, tc := range []struct {
		name     string
		mutate   func(http.Header)
		wantWire []string // nil = the header is absent from the wire
		// audit expectations; nil/"" = the field must be ABSENT
		wantSource      string
		wantPeerVerdict *string
		wantAuditValues []string
		wantAuditKeys   []string
	}{
		{
			// Add() appends beside the value DeliveryAuth seeded, so the wire
			// carries two verdicts while Header.Get still reports only the first.
			name:            "handler_appends_a_second_verdict",
			mutate:          func(hd http.Header) { hd.Add(VerdictHeader, replaced) },
			wantWire:        []string{ingress, replaced},
			wantSource:      verdictSourceHandler,
			wantAuditValues: []string{ingress, replaced},
		},
		{
			// Two values, both the ingress classification: still two verdicts on
			// the wire, so the audit must not call it the ingress default.
			name:            "handler_appends_a_duplicate_of_the_ingress_verdict",
			mutate:          func(hd http.Header) { hd.Add(VerdictHeader, ingress) },
			wantWire:        []string{ingress, ingress},
			wantSource:      verdictSourceHandler,
			wantAuditValues: []string{ingress, ingress},
		},
		{
			// A blank rewrite is a handler decision (the field reads back empty),
			// distinct from removal: the wire still carries one (empty) value.
			name:            "handler_sets_the_empty_verdict",
			mutate:          func(hd http.Header) { hd.Set(VerdictHeader, "") },
			wantWire:        []string{""},
			wantSource:      verdictSourceHandler,
			wantPeerVerdict: strPtr(""),
		},
		{
			// Removal: no value at all reaches the peer, and the audit says so
			// rather than restoring a verdict the handler did not author.
			name:            "handler_removes_the_verdict_header",
			mutate:          func(hd http.Header) { hd.Del(VerdictHeader) },
			wantWire:        nil,
			wantSource:      verdictSourceHandler,
			wantPeerVerdict: strPtr(""),
		},
		{
			// The DF-CRIER-432 single-value cases are unchanged: one replaced
			// value keeps the `peer_verdict` field.
			name:            "handler_replaces_the_verdict_with_one_value",
			mutate:          func(hd http.Header) { hd.Set(VerdictHeader, replaced) },
			wantWire:        []string{replaced},
			wantSource:      verdictSourceHandler,
			wantPeerVerdict: strPtr(replaced),
		},
		{
			// Writing the ingress value back is one verdict with one author.
			name:     "handler_writes_the_ingress_value_back",
			mutate:   func(hd http.Header) { hd.Set(VerdictHeader, ingress) },
			wantWire: []string{ingress},
		},
		{
			// A raw map write under a differently cased key is a SECOND entry:
			// Header.Get and Header.Values both miss it, net/http writes it, and
			// any client that folds spellings (every conforming parser) reads two
			// verdicts — the first one the handler's, not the ingress verdict.
			name: "handler_writes_a_non_canonical_key",
			mutate: func(hd http.Header) {
				hd["X-CRIER-FED-VERDICT"] = []string{replaced}
			},
			wantWire:        []string{replaced, ingress},
			wantSource:      verdictSourceHandler,
			wantAuditValues: []string{replaced, ingress},
			wantAuditKeys:   []string{"X-CRIER-FED-VERDICT", VerdictHeader},
		},
		{
			// The anomaly is reported on its own: here the peer reads exactly the
			// ingress verdict, under a spelling that is not the canonical one.
			name: "handler_moves_the_ingress_verdict_to_a_non_canonical_key",
			mutate: func(hd http.Header) {
				hd.Del(VerdictHeader)
				hd["x-crier-fed-verdict"] = []string{ingress}
			},
			wantWire:      []string{ingress},
			wantAuditKeys: []string{"x-crier-fed-verdict"},
		},
		{
			// The default, unchanged: no handler write, no handler fields.
			name:     "handler_writes_nothing",
			mutate:   func(http.Header) {},
			wantWire: []string{ingress},
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
			gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return peers.Store })(next)
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

			wire := resp.Header.Values(VerdictHeader)
			t.Logf("wire %s Values=%q Get=%q", VerdictHeader, wire, resp.Header.Get(VerdictHeader))
			t.Logf("audit=%s", strings.TrimSpace(logs.String()))

			// The wire is the evidence: what the client reads is what the audit
			// must describe, in the same order.
			require.Equal(t, tc.wantWire, wire, "the peer-facing header carries exactly these values")
			wantFirst := ""
			if len(tc.wantWire) > 0 {
				wantFirst = tc.wantWire[0]
			}
			require.Equal(t, wantFirst, resp.Header.Get(VerdictHeader),
				"Header.Get reads the FIRST wire value — a client honouring the last one reads wantWire[1]")

			// Nothing about the receipt itself moved.
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			require.Equal(t, body, string(got), "the middleware must not parse, re-encode or annotate the handler body")
			require.Equal(t, "unchanged", resp.Header.Get("X-Handler-Receipt"))
			require.Equal(t, ingress, resp.Header.Get(IngressVerdictHeader),
				"the authoritative ingress verdict is unchanged by any handler write")
			require.Equal(t, ingress, resp.Header.Get(InboundVerdictHeader))

			var audit map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &audit), "exactly one audit line")
			require.Equal(t, "federation inbound delivery", audit["msg"])
			require.Equal(t, ingress, audit["verdict"], "`verdict` is always the ingress classification")
			require.Equal(t, true, audit["receipt_flushed"])

			if tc.wantSource == "" {
				require.NotContains(t, audit, "peer_verdict_source")
			} else {
				require.Equal(t, tc.wantSource, audit["peer_verdict_source"])
			}
			if tc.wantPeerVerdict == nil {
				require.NotContains(t, audit, "peer_verdict",
					"peer_verdict is the single-value field: absent means the two verdicts agreed")
			} else {
				require.Equal(t, *tc.wantPeerVerdict, audit["peer_verdict"])
			}
			if tc.wantAuditValues == nil {
				require.NotContains(t, audit, "peer_verdict_values")
			} else {
				require.Equal(t, jsonStrings(tc.wantAuditValues), audit["peer_verdict_values"],
					"the audit names EVERY value the peer reads, in the order it reads them")
			}
			if tc.wantAuditKeys == nil {
				require.NotContains(t, audit, "peer_verdict_keys")
			} else {
				require.Equal(t, jsonStrings(tc.wantAuditKeys), audit["peer_verdict_keys"],
					"duplicate and non-canonically spelled verdict keys are reported, not sanitized")
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// A kind=anchor claim in docs/claims.yaml (and this test) pins the prose that
// describes what the audit reports, so the documented field set cannot drift from
// the emitted one without a failing build.
func TestPeerVerdictAuditFieldsAreDocumented(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		return string(b)
	}
	spec := read("../../specs/CHAT-FEDERATION.md")
	for _, anchor := range []string{
		"DF-CRIER-436",
		"peer_verdict_values",
		"peer_verdict_keys",
		"reported in the audit, not sanitized",
	} {
		require.Contains(t, spec, anchor, "the federation spec states what the ingress audit now reports")
	}

	sessions := read("../../specs/CHAT-SESSIONS.md")
	require.Contains(t, sessions, "peer_verdict_values",
		"the inbox-ingress section documents the multi-value audit field")
	require.Contains(t, sessions, "peer_verdict_keys",
		"the inbox-ingress section documents the duplicate/non-canonical key report")

	openapi := read("../../docs/openapi.yaml")
	require.Contains(t, openapi, "peer_verdict_values")
	require.Contains(t, openapi, "peer_verdict_keys")
}
