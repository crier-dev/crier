package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

type inboundDecisionKey struct{}

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
			// Inbox responses are finite JSON receipts, never streaming routes. Capture
			// once so all handler exits (including auth refusals) carry the same verdict.
			// Empty/no records preserve the legacy response bytes; its header and audit
			// still expose the degraded crossing without breaking legacy consumers.
			receipt := &inboundReceipt{ResponseWriter: w}
			receipt.Header().Set(VerdictHeader, string(v))
			receipt.Header().Set("X-Crier-Fed-Inbound-Verdict", string(v))
			completed := false
			defer func() {
				if completed {
					receipt.finish(v, posture.Peers > 0)
				} else {
					// Preserve the outer recovery middleware's authority: never flush
					// a buffered success while unwinding a downstream panic.
					receipt.status = http.StatusInternalServerError
				}
				slog.Info("federation inbound delivery", "peer", peer, "path", r.URL.Path, "verdict", v, "status", receipt.status)
			}()
			if v != FedAllowed && v != FedTokenOnly {
				RefuseDelivery(receipt, v)
				completed = true
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), inboundDecisionKey{}, v))
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
	body   bytes.Buffer
	status int
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
func (w *inboundReceipt) finish(v Verdict, annotate bool) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	body := w.body.Bytes()
	if annotate {
		var object map[string]json.RawMessage
		if json.Unmarshal(body, &object) == nil && object != nil {
			encodedVerdict, _ := json.Marshal(v)
			if outgoing, exists := object["verdict"]; exists && !bytes.Equal(outgoing, encodedVerdict) {
				// A local caller can enter here then forward to another instance. Keep
				// the existing outbound verdict and expose ingress separately, never
				// relabel an outbound key refusal as successful token authentication.
				object["inbound_verdict"] = encodedVerdict
			} else {
				object["verdict"] = encodedVerdict
			}
			if encoded, err := json.Marshal(object); err == nil {
				body = append(encoded, '\n')
			}
		}
	}
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

func keyedInboxRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return len(parts) == 3 && parts[1] != "" && parts[2] == "inbox" && (parts[0] == "agents" || parts[0] == "capabilities")
}
