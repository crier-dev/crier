package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
)

type decisionKey struct{}

// DeliveryRefusal is definitive: never queue or retry an auth rejection.
type DeliveryRefusal struct{ Verdict Verdict }

func (e *DeliveryRefusal) Error() string { return string(e.Verdict) }
func (e *DeliveryRefusal) Status() int {
	if e.Verdict == FedBadSignature {
		return http.StatusUnauthorized
	}
	return http.StatusForbidden
}

func noteDecision(ctx context.Context, v Verdict) {
	if p, ok := ctx.Value(decisionKey{}).(*Verdict); ok {
		*p = v
	}
}

// SetPeerRecords enables the lifecycle gate on outbound links. An empty store
// retains the shipped token-only transport; once records exist unmatched links
// cannot silently fall back to the shared credential.
func (c *Client) SetPeerRecords(s *PeerStore, selfID string) { c.records = s; c.selfID = selfID }

func (c *Client) preparePeerForward(req *http.Request, link Link, body []byte) error {
	if c.records == nil || len(c.records.List()) == 0 {
		if c.token != "" {
			noteDecision(req.Context(), FedTokenOnly)
		}
		return nil
	}
	p, ok := c.records.ForURL(link.URL)
	v := peerVerdict(p, ok)
	if v == FedAllowed && (c.selfID == "" || len(c.selfKey) != ed25519.PrivateKeySize) {
		v = FedBadSignature
	}
	if v != FedAllowed {
		noteDecision(req.Context(), v)
		return &DeliveryRefusal{Verdict: v}
	}
	req.Header.Set(PeerHeader, c.selfID)
	req.Header.Set("X-Fed-Key-Id", KeyID(c.selfKey.Public().(ed25519.PublicKey)))
	// FedAuthPayload remains present and verifiable by the shipped gate.
	req.Header.Set("X-Fed-Body-Sig", hex.EncodeToString(ed25519.Sign(c.selfKey, deliveryPayload(req, body))))
	noteDecision(req.Context(), FedAllowed)
	return nil
}

// ForwardOrHoldVerdict adds a per-call identity decision, without shared mutable
// client state or changes to the legacy forwarding API/body bytes.
func (c *Client) ForwardOrHoldVerdict(ctx context.Context, agentID string, body []byte, meta HoldMeta) (int, []byte, *HoldItem, Verdict, error) {
	v := FedNoLink
	status, response, held, err := c.ForwardOrHold(context.WithValue(ctx, decisionKey{}, &v), agentID, body, meta)
	return status, response, held, v, err
}

// RefuseDelivery keeps the named peer verdict separate from transport failures.
func RefuseDelivery(w http.ResponseWriter, v Verdict) {
	e := &DeliveryRefusal{Verdict: v}
	w.Header().Set(VerdictHeader, string(v))
	fedJSON(w, e.Status(), map[string]string{"error": fmt.Sprint(v), "verdict": string(v)})
}

func currentDecision(ctx context.Context) Verdict {
	if p, ok := ctx.Value(decisionKey{}).(*Verdict); ok {
		return *p
	}
	return ""
}
