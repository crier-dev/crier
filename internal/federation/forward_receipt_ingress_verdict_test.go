package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

// DF-CRIER-440: the peer receipt must name the header a peer ACTUALLY reads.
// The defect was a naming inversion in the comments — the header called
// "AUTHORITATIVE" was written and never read by production code, while the one
// called a "retained alias" was the only one any consumer read. The fix gives
// the authoritative header a reader of its own (ForwardReceipt.IngressVerdict)
// and keeps the alias read, because a peer predating DF-CRIER-432 sends only the
// alias.
//
// Every posture below is a real HTTP exchange: a real Client forwarding to a real
// DeliveryAuth gate in front of a real handler. The peerness is not simulated —
// the signatures are produced and verified by the shipped code paths.
type receiptIdentity int

const (
	noIdentity receiptIdentity = iota
	pinnedIdentity
	wrongKeyIdentity
)

type receiptPosture struct {
	name string
	// destState is the destination's view of the announcer: "" means the
	// destination holds no record for it at all.
	destState   string
	requirePeer bool
	identity    receiptIdentity
	bearer      string
	// onward, when set, is what a handler that forwards onward authors in the
	// peer-facing header (DF-CRIER-371), leaving the ingress classification to
	// the unshadowable header (DF-CRIER-432).
	onward     Verdict
	want       Verdict
	wantStatus int
}

func TestForwardReceiptCarriesEveryIngressPosture(t *testing.T) {
	for _, tc := range []receiptPosture{
		{name: "signed_linked_identity", destState: "linked", identity: pinnedIdentity, want: FedAllowed, wantStatus: http.StatusCreated},
		{name: "keyless_deployment_credential", destState: "linked", bearer: "deployment", want: FedTokenOnly, wantStatus: http.StatusCreated},
		{name: "handler_replaces_the_peer_facing_header", destState: "linked", bearer: "deployment", onward: FedNoLink, want: FedTokenOnly, wantStatus: http.StatusCreated},
		{name: "no_credential_at_all", destState: "linked", want: FedTokenOnly, wantStatus: http.StatusUnauthorized},
		{name: "unlinked_identity", identity: pinnedIdentity, want: FedNoLink, wantStatus: http.StatusForbidden},
		{name: "revoked_identity", destState: "revoked", identity: pinnedIdentity, want: FedPeerRevoked, wantStatus: http.StatusForbidden},
		{name: "wrong_key_for_the_announced_identity", destState: "linked", identity: wrongKeyIdentity, want: FedBadSignature, wantStatus: http.StatusUnauthorized},
		{name: "strict_posture_refuses_a_keyless_call", destState: "linked", requirePeer: true, bearer: "deployment", want: FedNoLink, wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReceiptRig(t, tc)
			receipt, err := rig.client.ForwardToURLReceiptWithNamespaceToken(t.Context(), rig.srv.URL, "target", []byte(`{"payload":{}}`), "")
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, receipt.StatusCode, string(receipt.Body))

			wantPeerFacing := string(tc.want)
			if tc.onward != "" {
				wantPeerFacing = string(tc.onward)
			}
			// The hop's peer-facing header is the handler's when the handler
			// authored one, else the ingress classification (DF-CRIER-418/-432).
			require.Equal(t, wantPeerFacing, receipt.Verdict)
			// The local ingress classification is readable by name, from the
			// header no handler can shadow.
			require.Equal(t, string(tc.want), receipt.IngressVerdict,
				"the receipt must name the peer's local ingress decision from the unshadowable header")
			require.Equal(t, string(tc.want), receipt.InboundVerdict,
				"the retained alias carries the same captured decision")
			require.Equal(t, receipt.IngressVerdict, receipt.InboundVerdict,
				"both ingress headers are written from one captured decision, so they cannot disagree")
			if tc.onward != "" {
				require.NotEqual(t, receipt.Verdict, receipt.IngressVerdict,
					"DF-CRIER-432: a handler's onward verdict must never be read as the local ingress decision")
			}
			t.Logf("posture=%s status=%d peer_facing=%q ingress=%q alias=%q",
				tc.name, receipt.StatusCode, receipt.Verdict, receipt.IngressVerdict, receipt.InboundVerdict)
		})
	}
}

// A peer built before DF-CRIER-432 writes only the alias. The receipt must keep
// reporting that classification rather than going empty — which is the whole
// reason InboundVerdict is still read.
func TestForwardReceiptKeepsTheAliasWhenThePeerOmitsTheAuthoritativeHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(InboundVerdictHeader, string(FedTokenOnly))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"legacy_peer"}`)
	}))
	defer srv.Close()

	c := NewClient(nil, 0, "")
	receipt, err := c.ForwardToURLReceiptWithNamespaceToken(t.Context(), srv.URL, "target", []byte(`{"payload":{}}`), "")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, receipt.StatusCode, string(receipt.Body))
	require.Empty(t, receipt.IngressVerdict, "the older peer never sent the authoritative header")
	require.Equal(t, string(FedTokenOnly), receipt.InboundVerdict,
		"the alias still carries the ingress decision, so a reader that falls back to it keeps the classification")
}

type receiptRig struct {
	dest   *PeerStore
	client *Client
	srv    *httptest.Server
}

func newReceiptRig(t *testing.T, p receiptPosture) *receiptRig {
	t.Helper()
	selfPub, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	destPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// The destination's registry: the identity it pinned for the announcer.
	dest, err := OpenPeerStore(filepath.Join(t.TempDir(), "dest", "peers.json"))
	require.NoError(t, err)
	if p.destState != "" {
		_, err = dest.Create(PeerRecord{ID: "a", PublicKey: hex.EncodeToString(selfPub), URL: "https://a.example", Direction: "both"})
		require.NoError(t, err)
		_, err = dest.link("a", KeyID(selfPub))
		require.NoError(t, err)
		if p.destState == "revoked" {
			state := "revoked"
			_, err = dest.Patch("a", PeerPatch{State: &state})
			require.NoError(t, err)
		}
	}
	dest.SetRequirePeer(p.requirePeer)

	var gate http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gate.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	gate = DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return dest })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.onward != "" {
			w.Header().Set(VerdictHeader, string(p.onward))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"receipt"}`)
	}))

	// The forwarding relay's own registry holds the destination, keyed by its
	// URL: that record is what makes the client sign and announce an identity.
	link, err := OpenPeerStore(filepath.Join(t.TempDir(), "link", "peers.json"))
	require.NoError(t, err)
	_, err = link.Create(PeerRecord{ID: "dest", PublicKey: hex.EncodeToString(destPub), URL: srv.URL, Direction: "both"})
	require.NoError(t, err)
	_, err = link.link("dest", KeyID(destPub))
	require.NoError(t, err)

	c := NewClient(nil, 0, p.bearer)
	switch p.identity {
	case pinnedIdentity:
		c.SetSelfKey(selfPriv)
		c.SetPeerRecords(link, "a")
	case wrongKeyIdentity:
		// The announced identity is the pinned one, but the proof is not.
		c.SetSelfKey(wrongPriv)
		c.SetPeerRecords(link, "a")
	}
	return &receiptRig{dest: dest, client: c, srv: srv}
}
