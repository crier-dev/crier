package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

// Exercise run() and the real HTTP listener with signature enforcement disabled:
// deployment compatibility policy must not inflate a trust-verification claim.
func TestTrustPossessionHTTP(t *testing.T) {
	parallelBoot(t)
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			base := startTestServerWithEnv(t, map[string]string{
				"CR_REGISTRY_BACKEND": backend, "CR_REGISTRY_SQLITE_PATH": filepath.Join(t.TempDir(), "trust.db"),
				"CR_AUTH_TOKEN": df297MessageToken, "CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken,
				"CR_PERMISSIONS_ENABLED": "false", "CR_REQUIRE_AGENT_SIG": "false", "CR_DATABASE_URL": "",
			})
			client := &http.Client{Timeout: 5 * time.Second}
			root := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			seed := make([]byte, ed25519.SeedSize)
			seed[0] = 1
			leaf := ed25519.NewKeyFromSeed(seed)
			seed[0] = 2
			other := ed25519.NewKeyFromSeed(seed)
			a := trust.ChainAnchor{ID: "possession-anchor", PublicKey: hex.EncodeToString(root.Public().(ed25519.PublicKey)), Label: "Test operator"}
			d := trust.ChainDelegation{Voucher: a.ID, SubjectKey: hex.EncodeToString(leaf.Public().(ed25519.PublicKey)), Scope: []string{"message-signing"}, IssuedAt: time.Now().UTC()}
			trust.SignChainDelegation(&d, root)
			for _, setup := range []struct {
				path, token string
				body        any
			}{
				{"/trust/anchors", df297AdminToken, a},
				{"/trust/delegations", df297AdminToken, d},
				{"/agents", df297MessageToken, map[string]any{"id": "possession-leaf", "public_key": d.SubjectKey}},
			} {
				raw, err := json.Marshal(setup.body)
				require.NoError(t, err)
				status, response := df297Request(t, client, "POST", base+setup.path, setup.token, "", string(raw))
				require.Equal(t, http.StatusCreated, status, response)
			}
			for _, tc := range []struct {
				name string
				key  ed25519.PrivateKey
				want string
			}{
				{"no-proof", nil, `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"chain-only","anchor_id":"possession-anchor","depth":1}`},
				{"possession-proven", leaf, `{"verdict":"TRUST_VERIFIED","basis":"possession-verified","anchor_id":"possession-anchor","depth":1}`},
				{"different-key", other, `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"proof-invalid-signature","anchor_id":"possession-anchor","depth":1}`},
				{"registry-key-rotated", leaf, `{"verdict":"TRUST_CHAIN_REACHABLE","basis":"proof-registry-key-mismatch","anchor_id":"possession-anchor","depth":1}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": []trust.ChainDelegation{d}}
					if tc.key != nil {
						body["challenge"] = "caller-random-nonce-380"
					}
					raw, err := json.MarshalIndent(body, "", "  ") // Exact noncompact bytes must verify.
					require.NoError(t, err)
					r, err := http.NewRequest("POST", base+"/trust/verify", bytes.NewReader(raw))
					require.NoError(t, err)
					r.Header.Set("Authorization", "Bearer "+df297MessageToken)
					r.Header.Set("Content-Type", "application/json")
					if tc.key != nil {
						ts := strconv.FormatInt(time.Now().Unix(), 10)
						sum := sha256.Sum256(raw)
						digest := hex.EncodeToString(sum[:])
						r.Header.Set(registry.HeaderAgentID, "possession-leaf")
						r.Header.Set(registry.HeaderAgentTS, ts)
						r.Header.Set(registry.HeaderAgentBodySHA256, digest)
						r.Header.Set(registry.HeaderAgentSig, hex.EncodeToString(ed25519.Sign(tc.key, []byte("POST\n/trust/verify\n"+ts+"\nsha256:"+digest))))
					}
					if tc.name == "registry-key-rotated" {
						before := r.Clone(r.Context())
						before.Body = io.NopCloser(bytes.NewReader(raw))
						response, err := client.Do(before)
						require.NoError(t, err)
						result, err := io.ReadAll(response.Body)
						_ = response.Body.Close()
						require.NoError(t, err)
						require.Equal(t, http.StatusOK, response.StatusCode)
						require.JSONEq(t, `{"verdict":"TRUST_VERIFIED","basis":"possession-verified","anchor_id":"possession-anchor","depth":1}`, string(result))
						status, resultText := df297Request(t, client, "DELETE", base+"/agents/possession-leaf", df297MessageToken, "", "")
						require.Equal(t, http.StatusNoContent, status, resultText)
						registration, err := json.Marshal(map[string]string{"id": "possession-leaf", "public_key": hex.EncodeToString(other.Public().(ed25519.PublicKey))})
						require.NoError(t, err)
						status, resultText = df297Request(t, client, "POST", base+"/agents", df297MessageToken, "", string(registration))
						require.Equal(t, http.StatusCreated, status, resultText)
						// r retains the exact pre-rotation chain, body and signature.
					}
					response, err := client.Do(r)
					require.NoError(t, err)
					defer func() { _ = response.Body.Close() }()
					result, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					t.Logf("%s %s -> %d %s", backend, tc.name, response.StatusCode, result)
					require.Equal(t, http.StatusOK, response.StatusCode, string(result))
					require.JSONEq(t, tc.want, string(result))
				})
			}
		})
	}
}
