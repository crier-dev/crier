package registry

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func TestTrustPossessionBackendParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "trust.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				s = db
			}
			exerciseTrustPossession(t, NewAuthorityStore(s, nil, ""))
		})
	}
}

// PostgreSQL runs this exact HTTP contract too; no backend-specific constraint
// or permissive signature-policy setting can promote a chain-only result.
func exerciseTrustPossession(t *testing.T, s Store) {
	t.Helper()
	a, d := registryTrustFixture()
	require.NoError(t, s.(TrustStore).AddTrustAnchor(a))
	require.NoError(t, s.(TrustStore).AddTrustDelegation(d))
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 1
	leaf := ed25519.NewKeyFromSeed(seed)
	seed[0] = 2
	other := ed25519.NewKeyFromSeed(seed)
	require.Equal(t, d.SubjectKey, hex.EncodeToString(leaf.Public().(ed25519.PublicKey)))
	for _, agent := range []*Agent{
		{ID: "leaf", PublicKey: HexKey(leaf.Public().(ed25519.PublicKey))},
		{ID: "other", PublicKey: HexKey(other.Public().(ed25519.PublicKey))},
		{ID: "keyless"},
	} {
		require.NoError(t, s.Register(agent))
	}
	h := NewTrustHandler(s, "operator-secret", "reach-secret")
	weak := `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"chain-only","anchor_id":"test-anchor","depth":1}`
	strong := `{"verdict":"TRUST_VERIFIED","basis":"possession-verified","anchor_id":"test-anchor","depth":1}`
	invalid := `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"proof-invalid-signature","anchor_id":"test-anchor","depth":1}`
	basis := func(value string) string { return strings.Replace(weak, "chain-only", value, 1) }
	for _, tc := range []struct {
		name      string
		challenge string
		key       ed25519.PrivateKey
		agent     string
		mutate    func(*http.Request, map[string]any)
		want      string
	}{
		{name: "no-proof", want: weak},
		{name: "possession-proven", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", want: strong},
		{name: "different-key", challenge: "caller-random-nonce-380", key: other, agent: "leaf", want: invalid},
		{name: "different-registered-agent", challenge: "caller-random-nonce-380", key: other, agent: "other", want: invalid},
		{name: "challenge-without-signature", challenge: "caller-random-nonce-380", want: basis("proof-missing")},
		{name: "signed-without-challenge", key: leaf, agent: "leaf", want: weak},
		{name: "keyless", challenge: "caller-random-nonce-380", key: leaf, agent: "keyless", want: basis("proof-no-registry-key")},
		{name: "unregistered", challenge: "caller-random-nonce-380", key: leaf, agent: "missing", want: basis("proof-no-registry-key")},
		{name: "unbound-body", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(r *http.Request, _ map[string]any) {
			r.Header.Del(HeaderAgentBodySHA256)
			r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(leaf, agentSigPayload(r.Method, r.URL.Path, r.Header.Get(HeaderAgentTS), ""))))
		}, want: basis("proof-body-unbound")},
		{name: "swapped-challenge", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(_ *http.Request, body map[string]any) { body["challenge"] = "attacker-replaced-nonce" }, want: basis("proof-body-mismatch")},
		{name: "swapped-scope", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(_ *http.Request, body map[string]any) { body["scope"] = []string{} }, want: basis("proof-body-mismatch")},
		{name: "stale-proof", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(r *http.Request, _ map[string]any) {
			ts := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
			r.Header.Set(HeaderAgentTS, ts)
			r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(leaf, agentSigPayload(r.Method, r.URL.Path, ts, r.Header.Get(HeaderAgentBodySHA256)))))
		}, want: basis("proof-stale-timestamp")},
		{name: "malformed-proof", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(r *http.Request, _ map[string]any) { r.Header.Set(HeaderAgentSig, "not-hex") }, want: basis("proof-malformed")},
		{name: "empty-proof", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(r *http.Request, _ map[string]any) { r.Header.Set(HeaderAgentSig, "") }, want: basis("proof-missing")},
		{name: "blank-challenge", challenge: " ", key: leaf, agent: "leaf", want: weak},
		{name: "oversized-challenge", challenge: strings.Repeat("n", 257), key: leaf, agent: "leaf", want: basis("proof-challenge-oversized")},
		{name: "valid-proof-cannot-repair-chain", challenge: "caller-random-nonce-380", key: leaf, agent: "leaf", mutate: func(_ *http.Request, body map[string]any) {
			bad := d
			bad.Voucher = "unknown"
			body["chain"] = []trust.ChainDelegation{bad}
		}, want: `{"verdict":"TRUST_UNKNOWN_ANCHOR","depth":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}}
			if tc.challenge != "" {
				body["challenge"] = tc.challenge
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			r := httptest.NewRequest("POST", "/trust/verify", bytes.NewReader(raw))
			if tc.key != nil {
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				r.Header.Set(HeaderAgentID, tc.agent)
				r.Header.Set(HeaderAgentTS, ts)
				r.Header.Set(HeaderAgentBodySHA256, sha256Hex(raw))
				r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(tc.key, agentSigPayload(r.Method, r.URL.Path, ts, sha256Hex(raw)))))
			}
			if tc.mutate != nil {
				tc.mutate(r, body)
				raw, err = json.Marshal(body)
				require.NoError(t, err)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				if tc.name == "valid-proof-cannot-repair-chain" {
					r.Header.Set(HeaderAgentBodySHA256, sha256Hex(raw))
					r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(tc.key, agentSigPayload(r.Method, r.URL.Path, r.Header.Get(HeaderAgentTS), sha256Hex(raw)))))
				}
			}
			w := httptest.NewRecorder()
			h.HandleVerify(w, r)
			t.Logf("%s -> %d %s", tc.name, w.Code, w.Body.String())
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			actual := w.Body.String()
			if tc.name == "stale-proof" {
				var got map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
				require.Contains(t, got, "timestamp_skew_seconds")
				require.InDelta(t, 60, got["timestamp_skew_seconds"], 2)
				delete(got, "timestamp_skew_seconds")
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				actual = string(raw)
			}
			require.JSONEq(t, tc.want, actual)
		})
	}
}
