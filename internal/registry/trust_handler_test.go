package registry

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func TestTrustHandlerMalformedAndUnavailable(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	a, d := registryTrustFixture()
	require.NoError(t, s.AddTrustAnchor(a))
	h := NewTrustHandler(s, "operator-secret", "message-secret")
	cases := []string{
		`{}`,
		`{"signer":"` + a.PublicKey + `","scope":null,"chain":[]}`,
		`{"signer":"` + a.PublicKey + `","scope":[],"chain":null}`,
		`{"signer":"` + a.PublicKey + `","scope":[],"chain":[],"anchors":[]}`,
		`{"signer":"` + a.PublicKey + `","scope":[],"chain":[]} {}`,
	}
	for _, body := range cases {
		r := httptest.NewRequest("POST", "/trust/verify", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleVerify(w, r)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	r := httptest.NewRequest("POST", "/trust/verify", strings.NewReader(`{"signer":"`+a.PublicKey+`","scope":[],"chain":[]}`))
	w := httptest.NewRecorder()
	h.HandleVerify(w, r)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), trust.TrustChainReachable)
	require.Contains(t, w.Body.String(), `"depth":0`)
	unsupported := NewTrustHandler(nil, "operator-secret", "message-secret")
	w = httptest.NewRecorder()
	unsupported.HandleVerify(w, r)
	require.Equal(t, 501, w.Code)
	d.Scope = nil
	trust.SignChainDelegation(&d, ed25519.NewKeyFromSeed(make([]byte, 32)))
	require.ErrorIs(t, s.AddTrustDelegation(d), ErrInvalidStoreInput, "nil scope must not become [] on a memory-store copy, changing signed bytes")
	d.Scope = []string{}
	trust.SignChainDelegation(&d, ed25519.NewKeyFromSeed(make([]byte, 32)))
	require.NoError(t, s.AddTrustDelegation(d))
	held, err := s.ListTrustDelegations(d.SubjectKey)
	require.NoError(t, err)
	require.Equal(t, trust.TrustChainReachable, trust.VerifyChain([]trust.ChainAnchor{a}, d.SubjectKey, []string{}, held).Verdict)
	require.Equal(t, trust.TrustScopeInsufficient, trust.VerifyChain([]trust.ChainAnchor{a}, d.SubjectKey, []string{"message-signing"}, held).Verdict)
	var decoded trust.ChainDelegation
	raw, err := json.Marshal(held[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, d, decoded)
}
