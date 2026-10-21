package federation

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/textproto"
	"sort"
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
				delta := receipt.peerVerdict()
				attrs := []any{"peer", peer, "path", r.URL.Path, "verdict", v}
				if delta.source == verdictSourceHandler {
					// The wire and the audit line must not disagree silently: when the
					// handler replaced the peer-facing verdict, name what the peer will
					// actually read. Clients differ on which of several values they
					// honour, so two or more values are reported as the full set rather
					// than one arbitrarily chosen value (DF-CRIER-436). A single value
					// keeps the DF-CRIER-432 `peer_verdict` field, and a removed header
					// keeps naming it as the empty verdict so an absent `peer_verdict`
					// goes on meaning "the two agreed".
					attrs = append(attrs, "peer_verdict_source", delta.source)
					switch {
					case len(delta.values) > 1:
						attrs = append(attrs, "peer_verdict_values", delta.values)
					case len(delta.values) == 1:
						attrs = append(attrs, "peer_verdict", delta.values[0])
					default:
						attrs = append(attrs, "peer_verdict", "")
					}
				}
				if delta.anomalous() {
					// Duplicate and non-canonically spelled verdict keys are REPORTED
					// here, not sanitized in finish(): the peer-facing header stays the
					// handler's to author (DF-CRIER-371) and finish() does not rewrite
					// it, so the audit is what makes the shape visible (DF-CRIER-436).
					attrs = append(attrs, "peer_verdict_keys", delta.keys)
				}
				if completed {
					receipt.finish()
					attrs = append(attrs, ingressVerdictAuditAttrs(receipt.Header())...)
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

// peerVerdictDelta is one reading of the peer-facing verdict header: every value
// the peer will read, in the order it will read them, the exact key spellings
// carrying them, and who authored the header.
type peerVerdictDelta struct {
	values []string // every value on the wire, in the order net/http writes them
	keys   []string // every key spelling present, in the same order
	source string   // verdictSourceIngress or verdictSourceHandler
}

// anomalous reports whether the verdict header reached the wire under more than
// one key spelling, or under a single spelling that is not the canonical one.
// http.Header is a plain map keyed by exact string, so a handler writing
// hd["X-CRIER-FED-VERDICT"] rather than hd.Set(VerdictHeader, …) adds a SECOND
// entry beside the canonical one — net/http writes both, and a client that folds
// spellings (any conforming parser does) reads two verdicts (DF-CRIER-436).
func (d peerVerdictDelta) anomalous() bool {
	if len(d.keys) > 1 {
		return true
	}
	return len(d.keys) == 1 && d.keys[0] != VerdictHeader
}

// peerVerdict reports every value the peer reads in the peer-facing verdict
// header, and who authored it: the handler when the header does not carry exactly
// one value equal to the ingress classification this middleware wrote before
// routing — a replaced single value, an appended second value, a differently
// cased key and removal (no value at all) are all handler decisions — otherwise
// the ingress classification itself.
//
// Header.Get and Header.Values resolve ONE map entry, the canonical spelling, so
// neither can see the two shapes that carry more than one verdict: Add appends
// beside the seeded value, and a raw map write under a differently cased key is a
// separate entry both accessors skip while net/http still puts it on the wire. A
// client that reads the last value then acts on a handler-authored verdict the
// audit named as ingress — the exact wire/audit disagreement DF-CRIER-432's
// precedence forbids (DF-CRIER-436). So this reads EVERY entry whose canonical
// spelling is VerdictHeader and rebuilds the wire order: key spellings sorted
// bytewise, each key's values in the order that key holds them — the order
// net/http writes response headers (Header.sortedKeyValues), which is therefore
// the order a client reads them in.
func (w *inboundReceipt) peerVerdict() peerVerdictDelta {
	keys, values := verdictWireValues(w.Header(), VerdictHeader)
	source := verdictSourceHandler
	if len(values) == 1 && values[0] == string(w.ingress) {
		source = verdictSourceIngress
	}
	return peerVerdictDelta{values: values, keys: keys, source: source}
}

// verdictWireValues reads the server map in net/http's serialization order.
// Use only before serialization: the client parser has already folded keys and
// must use Header.Values to retain the actual received order instead.
func verdictWireValues(h http.Header, header string) (keys, values []string) {
	canonical := textproto.CanonicalMIMEHeaderKey(header)
	for k := range h {
		if textproto.CanonicalMIMEHeaderKey(k) == canonical {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		values = append(values, h[k]...)
	}
	return keys, values
}

// ingressVerdictAuditAttrs records the FINAL wire shape, after finish restores
// the canonical ingress value. Never delete a handler's differently cased key:
// report it. Identical duplicates are a shape anomaly, not a disagreement.
func ingressVerdictAuditAttrs(h http.Header) []any {
	keys, values := verdictWireValues(h, IngressVerdictHeader)
	var attrs []any
	if len(values) > 1 {
		attrs = append(attrs, "ingress_verdict_anomaly", true, "ingress_verdict_values", values)
	}
	if len(keys) > 1 || (len(keys) == 1 && keys[0] != IngressVerdictHeader) {
		attrs = append(attrs, "ingress_verdict_keys", keys)
	}
	return attrs
}

// finish flushes the handler's bytes verbatim. The peer-facing verdict header is
// the handler's to author — an onward peer's receipt replaces it legitimately
// (DF-CRIER-371) — but the two ingress classification headers are re-asserted
// here from captured state, after the handler returned. Re-assertion protects
// the canonical map entries (DF-CRIER-432); differently cased handler entries
// remain on the wire and are reported in the ingress audit (DF-CRIER-457).
// Handler-owned
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
