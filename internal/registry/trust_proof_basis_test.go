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
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func TestTrustProofBasisRegistryRotation(t *testing.T) {
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
			seed := make([]byte, ed25519.SeedSize)
			seed[0] = 1
			leaf := ed25519.NewKeyFromSeed(seed)
			seed[0] = 2
			rotated := ed25519.NewKeyFromSeed(seed)
			require.Equal(t, d.SubjectKey, hex.EncodeToString(leaf.Public().(ed25519.PublicKey)))
			require.NoError(t, s.Register(&Agent{ID: "basis-leaf", PublicKey: HexKey(leaf.Public().(ed25519.PublicKey))}))
			// Issue the chain while the registry still holds the leaf key.
			require.NoError(t, s.(TrustStore).AddTrustAnchor(a))
			require.NoError(t, s.(TrustStore).AddTrustDelegation(d))
			h := NewTrustHandler(s, "operator-secret", "reach-secret")
			body, err := json.Marshal(map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}, "challenge": "basis-nonce-395"})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/trust/verify", bytes.NewReader(body))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			r.Header.Set(HeaderAgentID, "basis-leaf")
			r.Header.Set(HeaderAgentTS, ts)
			r.Header.Set(HeaderAgentBodySHA256, sha256Hex(body))
			payload := agentSigPayload(r.Method, r.URL.Path, ts, sha256Hex(body))
			signature := ed25519.Sign(leaf, payload)
			require.True(t, ed25519.Verify(ed25519.PublicKey(mustDecodeTrustLeaf(t, d.SubjectKey)), payload, signature))
			r.Header.Set(HeaderAgentSig, hex.EncodeToString(signature))
			check := func(t *testing.T, request *http.Request, raw []byte, wantVerdict, wantBasis string) {
				t.Helper()
				request = request.Clone(request.Context())
				request.Body = io.NopCloser(bytes.NewReader(raw))
				w := httptest.NewRecorder()
				h.HandleVerify(w, request)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var got trust.ChainVerdict
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
				t.Logf("verdict=%s basis=%s", got.Verdict, got.Basis)
				require.Equal(t, trust.ChainVerdict{Verdict: wantVerdict, Basis: wantBasis, AnchorID: a.ID, Depth: 1}, got)
				// No raw verifier refusal, key, proof, or challenge can escape the sink.
				for _, secret := range []string{d.SubjectKey, hex.EncodeToString(rotated.Public().(ed25519.PublicKey)), request.Header.Get(HeaderAgentSig), "basis-nonce-395", "registered key does not match trust leaf"} {
					if secret != "" {
						require.NotContains(t, w.Body.String(), secret)
					}
				}
			}
			check(t, r, body, trust.TrustVerified, "possession-verified")
			// Registry keys are immutable during registration: rotation here is an
			// explicit unregister/re-register, not an accidental overwrite or PATCH.
			require.NoError(t, s.Unregister("basis-leaf"))
			require.NoError(t, s.Register(&Agent{ID: "basis-leaf", PublicKey: HexKey(rotated.Public().(ed25519.PublicKey))}))
			registered, err := s.Get("basis-leaf")
			require.NoError(t, err)
			require.Equal(t, HexKey(rotated.Public().(ed25519.PublicKey)), registered.PublicKey)
			t.Run("valid-leaf-proof-registry-mismatch", func(t *testing.T) {
				// Replay identical body, timestamp and signature after rotation.
				check(t, r, body, trust.TrustChainReachable, "proof-registry-key-mismatch")
			})
			t.Run("wrong-key-signature-not-mismatch", func(t *testing.T) {
				wrong := r.Clone(r.Context())
				wrong.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(rotated, payload)))
				check(t, wrong, body, trust.TrustChainReachable, "proof-invalid-signature")
			})
			t.Run("no-challenge-chain-only", func(t *testing.T) {
				raw, err := json.Marshal(map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}})
				require.NoError(t, err)
				unsigned := httptest.NewRequest(http.MethodPost, "/trust/verify", bytes.NewReader(raw))
				check(t, unsigned, raw, trust.TrustChainReachable, "chain-only")
			})
		})
	}
}

func mustDecodeTrustLeaf(t *testing.T, key string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(key)
	require.NoError(t, err)
	return decoded
}
