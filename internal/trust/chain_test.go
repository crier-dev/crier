package trust

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func chainFixture() (ChainAnchor, []ChainDelegation, string) {
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	middle := ed25519.NewKeyFromSeed(seed)
	seed[0] = 2
	leaf := ed25519.NewKeyFromSeed(seed)
	pub := func(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Public().(ed25519.PublicKey)) }
	at := time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)
	a := ChainAnchor{ID: "anchor-test", PublicKey: pub(root), Label: "Test operator", CreatedAt: at}
	chain := []ChainDelegation{
		{Voucher: a.ID, SubjectKey: pub(middle), Scope: []string{"message-signing", TransitiveCapability}, MaxDepth: 1, IssuedAt: at},
		{Voucher: ChainKeyID(pub(middle)), SubjectKey: pub(leaf), Scope: []string{"message-signing"}, MaxDepth: 0, IssuedAt: at},
	}
	SignChainDelegation(&chain[0], root)
	SignChainDelegation(&chain[1], middle)
	return a, chain, pub(leaf)
}

func TestVerifyChainNamedVerdicts(t *testing.T) {
	a, chain, signer := chainFixture()
	tests := []struct {
		name    string
		anchors []ChainAnchor
		mutate  func([]ChainDelegation)
		scope   []string
		code    string
		link    int
		missing string
	}{
		{name: "verified", anchors: []ChainAnchor{a}, scope: []string{"message-signing"}, code: TrustVerified},
		{name: "unknown", scope: []string{"message-signing"}, code: TrustUnknownAnchor},
		{name: "depth", anchors: []ChainAnchor{a}, mutate: func(c []ChainDelegation) {
			c[0].MaxDepth = 0
			SignChainDelegation(&c[0], ed25519.NewKeyFromSeed(make([]byte, 32)))
		}, scope: []string{"message-signing"}, code: TrustDepthExceeded, link: 1},
		{name: "scope", anchors: []ChainAnchor{a}, scope: []string{"admin"}, code: TrustScopeInsufficient, link: 1, missing: "admin"},
		{name: "signature", anchors: []ChainAnchor{a}, mutate: func(c []ChainDelegation) { c[1].Signature = chain[0].Signature }, scope: []string{"message-signing"}, code: TrustBadSignature, link: 2},
		{name: "transitivity-explicit", anchors: []ChainAnchor{a}, mutate: func(c []ChainDelegation) {
			c[0].Scope = []string{"message-signing"}
			SignChainDelegation(&c[0], ed25519.NewKeyFromSeed(make([]byte, 32)))
		}, scope: []string{"message-signing"}, code: TrustScopeInsufficient, link: 1, missing: TransitiveCapability},
		{name: "broken-path", anchors: []ChainAnchor{a}, mutate: func(c []ChainDelegation) { c[1].Voucher = "someone-else" }, scope: []string{"message-signing"}, code: TrustBadSignature, link: 2},
		{name: "leaf-mismatch", anchors: []ChainAnchor{a}, mutate: func(c []ChainDelegation) { c[1].SubjectKey = a.PublicKey }, scope: []string{"message-signing"}, code: TrustBadSignature, link: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(chain)
			require.NoError(t, err)
			var c []ChainDelegation
			require.NoError(t, json.Unmarshal(raw, &c))
			if tt.mutate != nil {
				tt.mutate(c)
			}
			got := VerifyChain(tt.anchors, signer, tt.scope, c)
			require.Equal(t, tt.code, got.Verdict)
			require.Equal(t, tt.link, got.Link)
			require.Equal(t, tt.missing, got.MissingCapability)
			if tt.code == TrustVerified {
				require.Equal(t, a.ID, got.AnchorID)
				require.Equal(t, 2, got.Depth)
			}
			t.Logf("measured verdict: %s", mustChainJSON(t, got))
		})
	}
	require.Equal(t, TrustVerified, VerifyChain([]ChainAnchor{a}, a.PublicKey, nil, nil).Verdict)
	// A max_depth of zero permits the direct leaf, not another voucher.
	require.Equal(t, TrustVerified, VerifyChain([]ChainAnchor{a}, chain[0].SubjectKey, []string{"message-signing"}, chain[:1]).Verdict)
}

func mustChainJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestChainCanonicalContract(t *testing.T) {
	_, chain, _ := chainFixture()
	d := chain[0]
	require.Equal(t, `{"voucher":"anchor-test","subject_key":"cecc1507dc1ddd7295951c290888f095adb9044d1b73d696e6df065d683bd4fc","scope":["message-signing","trust-delegation"],"max_depth":1,"issued_at":"2026-10-19T00:00:00.123456789Z","signature":""}`, string(d.CanonicalBytes()))
	d.Signature = "changed"
	require.Equal(t, chain[0].CanonicalBytes(), d.CanonicalBytes())
	d.MaxDepth++
	require.NotEqual(t, chain[0].CanonicalBytes(), d.CanonicalBytes())
}
