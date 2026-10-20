package federation

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
)

type inboundDecisionKey struct{}

type inboundDecision struct {
	verdict Verdict
	peer    string
}

// Audit vocabulary for the peer-facing verdict header. DeliveryAuth writes the
// ingress classification before routing, so a different value afterwards was
// authored by the handler (DF-CRIER-432).
const (
	verdictSourceIngress = "ingress"
	verdictSourceHandler = "handler"
)

// VerifiedDeliveryPeer returns only the identity authenticated by DeliveryAuth's
// complete per-request checks, never an unverified header claim. An empty result
// means no attributable peer (including legacy bearer-only ingress).
func VerifiedDeliveryPeer(ctx context.Context) string {
	decision, ok := ctx.Value(inboundDecisionKey{}).(inboundDecision)
	if !ok || decision.verdict != FedAllowed {
		return ""
	}
	return decision.peer
}

// DeliveryAuth authenticates inbox ingress before the deployment gate. Claimed
// peer identities never downgrade to bearer auth when lifecycle records are
// enabled. Keyless calls remain deployment-authenticated in compatibility mode.
// With records configured, even an unmarked inbox POST is classified: a caller
// cannot evade the strict posture or audit by omitting the hop/peer headers.
func DeliveryAuth(existing func(http.Handler) http.Handler, getStore func() *PeerStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		legacy := existing(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !keyedInboxRoute(r) || r.Context().Value(inboundDecisionKey{}) != nil {
				legacy.ServeHTTP(w, r)
				return
			}
			store := getStore()
			posture := store.InboundPosture()
			peer := r.Header.Get(PeerHeader)
			v := FedTokenOnly
			if peer != "" && store != nil {
				v = store.VerifyDelivery(r)
			} else if posture.RequirePeerEffective {
				v = FedNoLink
			}
			// Inbox responses are finite receipts, never streaming routes. Capture
			// once so all handler exits (including auth refusals) carry verdict
			// headers while preserving the handler's exact body bytes in every posture.
			receipt := &inboundReceipt{ResponseWriter: w, ingress: v}
			// The peer-facing header is seeded here so the DF-CRIER-418 default still
			// holds — with no handler verdict, the peer reads the ingress
			// classification — while a handler that forwards onward can REPLACE it
			// with the onward verdict (DF-CRIER-371). The two ingress classification
			// headers are re-asserted from captured state in finish(), after the
			// handler returns, so no handler write can shadow the local decision
			// (DF-CRIER-432).
			receipt.Header().Set(VerdictHeader, string(v))
			receipt.Header().Set(InboundVerdictHeader, string(v))
			receipt.Header().Set(IngressVerdictHeader, string(v))
			completed := false
			defer func() {
				peerVerdict, peerSource := receipt.peerVerdict()
				attrs := []any{"peer", peer, "path", r.URL.Path, "verdict", v}
				if peerSource == verdictSourceHandler {
					// The wire and the audit line must not disagree silently: when the
					// handler replaced the peer-facing verdict, name both values and
					// which one the peer received.
					attrs = append(attrs, "peer_verdict", string(peerVerdict), "peer_verdict_source", peerSource)
				}
				if completed {
					receipt.finish()
					slog.Info("federation inbound delivery", append(attrs, "status", receipt.status, "receipt_flushed", true)...)
					return
				}
				// Preserve the outer recovery middleware's authority: never flush
				// a buffered success while unwinding a downstream panic. DF-CRIER-394:
				// the receipt was ABANDONED, so the audit line must not carry a status
				// or a verdict-shaped success an operator could read as a classified
				// delivery the client received. The verdict headers above still go
				// out with the recovery middleware's own response.
				slog.Info("federation inbound delivery", append(attrs, "flushed", false, "abandoned", true)...)
			}()
			if v != FedAllowed && v != FedTokenOnly {
				RefuseDelivery(receipt, v)
				completed = true
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), inboundDecisionKey{}, inboundDecision{verdict: v, peer: peer}))
			if v == FedAllowed {
				next.ServeHTTP(receipt, r)
			} else {
				legacy.ServeHTTP(receipt, r)
			}
			completed = true
		})
	}
}

type inboundReceipt struct {
	http.ResponseWriter
	body    bytes.Buffer
	status  int
	ingress Verdict
}

func (w *inboundReceipt) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *inboundReceipt) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// peerVerdict reports the value the peer reads in the peer-facing verdict
// header, and who authored it: the handler when it replaced the ingress
// classification this middleware wrote before routing (removing it counts — the
// header then reads back as the empty verdict), otherwise the ingress
// classification itself.
func (w *inboundReceipt) peerVerdict() (Verdict, string) {
	if v := w.Header().Get(VerdictHeader); v != string(w.ingress) {
		return Verdict(v), verdictSourceHandler
	}
	return w.ingress, verdictSourceIngress
}

// finish flushes the handler's bytes verbatim. The peer-facing verdict header is
// the handler's to author — an onward peer's receipt replaces it legitimately
// (DF-CRIER-371) — but the two ingress classification headers are re-asserted
// here from captured state, after the handler returned, so a handler write cannot
// shadow or contradict the local ingress decision (DF-CRIER-432). Handler-owned
// verdict fields (including an onward peer's receipt) must never be added,
// overwritten or re-encoded in the body.
func (w *inboundReceipt) finish() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.Header().Set(IngressVerdictHeader, string(w.ingress))
	w.Header().Set(InboundVerdictHeader, string(w.ingress))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(w.body.Bytes())
}

func keyedInboxRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return len(parts) == 3 && parts[1] != "" && parts[2] == "inbox" && (parts[0] == "agents" || parts[0] == "capabilities")
}
