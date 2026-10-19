package registry

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Exercise the production verifier through AuthorityStore, not a second proof
// parser. String expectations deliberately also compile against the old handler.
func TestTrustProofFailureBases(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "trust.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				s = db
			}
			s = NewAuthorityStore(s, nil, "")
			a, d := registryTrustFixture()
			require.NoError(t, s.(TrustStore).AddTrustAnchor(a))
			seed := make([]byte, ed25519.SeedSize)
			seed[0] = 1
			leaf := ed25519.NewKeyFromSeed(seed)
			seed[0] = 2
			other := ed25519.NewKeyFromSeed(seed)
			require.Equal(t, d.SubjectKey, hex.EncodeToString(leaf.Public().(ed25519.PublicKey)))
			require.NoError(t, s.Register(&Agent{ID: "failure-leaf", PublicKey: HexKey(leaf.Public().(ed25519.PublicKey))}))
			require.NoError(t, s.Register(&Agent{ID: "failure-keyless"}))
			for _, tc := range []struct {
				name      string
				challenge string
				basis     string
				mutate    func(*http.Request)
				skew      int
				verified  bool
			}{
				{name: "stale-five-minutes", challenge: "nonce", basis: "proof-stale-timestamp", skew: 300},
				{name: "future-five-minutes", challenge: "nonce", basis: "proof-stale-timestamp", skew: -300},
				{name: "keyless", challenge: "nonce", basis: "proof-no-registry-key", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentID, "failure-keyless") }},
				{name: "keyless-never-offered-signature", challenge: "nonce", basis: "proof-no-registry-key", mutate: func(r *http.Request) {
					r.Header.Set(HeaderAgentID, "failure-keyless")
					r.Header.Del(HeaderAgentSig)
					r.Header.Del(HeaderAgentTS)
				}},
				{name: "unregistered", challenge: "nonce", basis: "proof-no-registry-key", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentID, "unknown") }},
				{name: "malformed-hex", challenge: "nonce", basis: "proof-malformed", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentSig, "not-hex") }},
				{name: "malformed-signature-length", challenge: "nonce", basis: "proof-malformed", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentSig, "abcd") }},
				{name: "malformed-timestamp", challenge: "nonce", basis: "proof-malformed", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentTS, "not-seconds") }},
				{name: "wrong-key-signature", challenge: "nonce", basis: "proof-invalid-signature", mutate: func(r *http.Request) {
					r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(other, agentSigPayload(r.Method, r.URL.Path, r.Header.Get(HeaderAgentTS), r.Header.Get(HeaderAgentBodySHA256)))))
				}},
				{name: "missing-proof", challenge: "nonce", basis: "proof-missing", mutate: func(r *http.Request) { r.Header = make(http.Header) }},
				{name: "empty-signature", challenge: "nonce", basis: "proof-missing", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentSig, " ") }},
				{name: "missing-body-binding", challenge: "nonce", basis: "proof-body-unbound", mutate: func(r *http.Request) { r.Header.Del(HeaderAgentBodySHA256) }},
				{name: "mismatched-body-binding", challenge: "nonce", basis: "proof-body-mismatch", mutate: func(r *http.Request) { r.Header.Set(HeaderAgentBodySHA256, strings.Repeat("0", 64)) }},
				// The reported ~300-byte challenge exceeds the existing MAXIMUM,
				// not a minimum. Preserve that rule, but no longer call it absent.
				{name: "challenge-300-bytes", challenge: strings.Repeat("n", 300), basis: "proof-challenge-oversized"},
				{name: "challenge-257-bytes", challenge: strings.Repeat("n", 257), basis: "proof-challenge-oversized"},
				{name: "challenge-utf8-bytes", challenge: strings.Repeat("\u00e9", 129), basis: "proof-challenge-oversized"},
				{name: "absent-challenge", basis: "chain-only"},
				{name: "blank-challenge", challenge: " ", basis: "chain-only"},
				{name: "short-usable-challenge", challenge: "n", basis: "possession-verified", verified: true},
				{name: "challenge-256-bytes", challenge: strings.Repeat("n", 256), basis: "possession-verified", verified: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}}
					if tc.challenge != "" {
						body["challenge"] = tc.challenge
					}
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					r := httptest.NewRequest(http.MethodPost, "/trust/verify", bytes.NewReader(raw))
					ts := strconv.FormatInt(time.Now().Add(-time.Duration(tc.skew)*time.Second).Unix(), 10)
					r.Header.Set(HeaderAgentID, "failure-leaf")
					r.Header.Set(HeaderAgentTS, ts)
					r.Header.Set(HeaderAgentBodySHA256, sha256Hex(raw))
					r.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(leaf, agentSigPayload(r.Method, r.URL.Path, ts, sha256Hex(raw)))))
					if tc.mutate != nil {
						tc.mutate(r)
					}
					w := httptest.NewRecorder()
					NewTrustHandler(s, "operator-secret", "reach-secret").HandleVerify(w, r)
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					var got struct {
						trust.ChainVerdict
						Skew *float64 `json:"timestamp_skew_seconds"`
					}
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
					t.Logf("%s -> basis=%s response=%s", tc.name, got.Basis, strings.TrimSpace(w.Body.String()))
					wantVerdict := trust.TrustChainReachable
					if tc.verified {
						wantVerdict = trust.TrustVerified
					}
					require.Equal(t, wantVerdict, got.Verdict)
					require.Equal(t, a.ID, got.AnchorID)
					require.Equal(t, 1, got.Depth)
					require.Equal(t, tc.basis, got.Basis, "actual failure must determine the basis")
					if tc.skew != 0 {
						require.NotNil(t, got.Skew, "stale proof must carry signed clock skew")
						require.InDelta(t, float64(tc.skew), *got.Skew, 2)
					} else {
						require.Nil(t, got.Skew)
					}
					for _, secret := range []string{d.SubjectKey, r.Header.Get(HeaderAgentSig), "not-hex", "operator-secret", "reach-secret"} {
						if strings.TrimSpace(secret) != "" {
							require.NotContains(t, w.Body.String(), secret)
						}
					}
				})
			}
		})
	}
}

type trustProofUnavailableStore struct{ *MemoryStore }

func (s trustProofUnavailableStore) Get(string) (*Agent, error) {
	return nil, errors.New("private registry failure")
}

func TestTrustProofRegistryUnavailableBasis(t *testing.T) {
	s := trustProofUnavailableStore{NewMemoryStore()}
	a, d := registryTrustFixture()
	require.NoError(t, s.AddTrustAnchor(a))
	raw, err := json.Marshal(map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}, "challenge": "nonce"})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/trust/verify", bytes.NewReader(raw))
	r.Header.Set(HeaderAgentID, "unavailable")
	r.Header.Set(HeaderAgentTS, strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set(HeaderAgentSig, strings.Repeat("0", 128))
	w := httptest.NewRecorder()
	NewTrustHandler(s, "operator-secret", "reach-secret").HandleVerify(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"proof-registry-unavailable","anchor_id":"test-anchor","depth":1}`, w.Body.String())
	require.NotContains(t, w.Body.String(), "private registry failure")
}
