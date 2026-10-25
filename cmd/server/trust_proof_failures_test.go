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
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func TestTrustProofFailureBasesHTTP(t *testing.T) {
	parallelBoot(t)
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			base := startTestServerWithEnv(t, map[string]string{
				"CR_REGISTRY_BACKEND": backend, "CR_REGISTRY_SQLITE_PATH": filepath.Join(t.TempDir(), "trust.db"),
				"CR_AUTH_TOKEN": df297MessageToken, "CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken,
				"CR_PERMISSIONS_ENABLED": "false", "CR_REQUIRE_AGENT_SIG": "false", "CR_DATABASE_URL": "",
			})
			client := &http.Client{Timeout: 5 * time.Second}
			leaf := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			publicKey := hex.EncodeToString(leaf.Public().(ed25519.PublicKey))
			for _, setup := range []struct {
				path, token string
				body        any
			}{
				{"/trust/anchors", df297AdminToken, trust.ChainAnchor{ID: "failure-anchor", PublicKey: publicKey, Label: "Test operator"}},
				{"/agents", df297MessageToken, map[string]string{"id": "failure-leaf", "public_key": publicKey}},
				{"/agents", df297MessageToken, map[string]string{"id": "failure-keyless"}},
			} {
				raw, err := json.Marshal(setup.body)
				require.NoError(t, err)
				status, result := df297Request(t, client, http.MethodPost, base+setup.path, setup.token, "", string(raw))
				require.Equal(t, http.StatusCreated, status, result)
			}
			for _, tc := range []struct {
				name, challenge, basis string
				mutate                 func(*http.Request)
			}{
				{"stale", "nonce", "proof-stale-timestamp", func(r *http.Request) {
					r.Header.Set(registry.HeaderAgentTS, strconv.FormatInt(time.Now().Add(-5*time.Minute).Unix(), 10))
				}},
				{"keyless-unsigned", "nonce", "proof-no-registry-key", func(r *http.Request) {
					r.Header.Set(registry.HeaderAgentID, "failure-keyless")
					r.Header.Del(registry.HeaderAgentSig)
				}},
				{"malformed-hex", "nonce", "proof-malformed", func(r *http.Request) { r.Header.Set(registry.HeaderAgentSig, "not-hex") }},
				{"300-byte-challenge", strings.Repeat("n", 300), "proof-challenge-oversized", nil},
				{"absent-challenge", "", "chain-only", nil},
				{"usable-short-challenge", "n", "possession-verified", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					raw, err := json.Marshal(map[string]any{"signer": publicKey, "scope": []string{}, "chain": []trust.ChainDelegation{}, "challenge": tc.challenge})
					require.NoError(t, err)
					r, err := http.NewRequest(http.MethodPost, base+"/trust/verify", bytes.NewReader(raw))
					require.NoError(t, err)
					r.Header.Set("Authorization", "Bearer "+df297MessageToken)
					ts := strconv.FormatInt(time.Now().Unix(), 10)
					sum := sha256.Sum256(raw)
					digest := hex.EncodeToString(sum[:])
					r.Header.Set(registry.HeaderAgentID, "failure-leaf")
					r.Header.Set(registry.HeaderAgentTS, ts)
					r.Header.Set(registry.HeaderAgentBodySHA256, digest)
					r.Header.Set(registry.HeaderAgentSig, hex.EncodeToString(ed25519.Sign(leaf, []byte("POST\n/trust/verify\n"+ts+"\nsha256:"+digest))))
					if tc.mutate != nil {
						tc.mutate(r)
					}
					response, err := client.Do(r)
					require.NoError(t, err)
					result, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, response.StatusCode, string(result))
					var got trust.ChainVerdict
					require.NoError(t, json.Unmarshal(result, &got))
					t.Logf("%s %s -> %s", backend, tc.name, result)
					wantVerdict := trust.TrustChainReachable
					if tc.basis == "possession-verified" {
						wantVerdict = trust.TrustVerified
					}
					require.Equal(t, wantVerdict, got.Verdict)
					require.Equal(t, tc.basis, got.Basis)
					require.Equal(t, "failure-anchor", got.AnchorID)
					require.Zero(t, got.Depth)
					if tc.name == "stale" {
						require.NotNil(t, got.TimestampSkewSeconds)
						require.InDelta(t, 300, *got.TimestampSkewSeconds, 2)
					} else {
						require.Nil(t, got.TimestampSkewSeconds)
					}
					require.NotContains(t, string(result), publicKey)
					if signature := r.Header.Get(registry.HeaderAgentSig); signature != "" {
						require.NotContains(t, string(result), signature, "signature must not escape")
					}
				})
			}
		})
	}
}

func TestTrustProofFailureBasesOpenAPI(t *testing.T) {
	var doc map[string]any
	require.NoError(t, json.Unmarshal(openapiJSON, &doc))
	var node any = doc
	for _, key := range []string{"paths", "/trust/verify", "post", "responses", "200", "content", "application/json", "schema", "properties"} {
		object, ok := node.(map[string]any)
		require.True(t, ok, "expected object before %s", key)
		node, ok = object[key]
		require.True(t, ok, "missing %s", key)
	}
	properties := node.(map[string]any)
	basis := properties["basis"].(map[string]any)
	require.ElementsMatch(t, []string{
		"chain-only", "possession-verified", "proof-registry-key-mismatch", "proof-invalid-signature",
		"proof-stale-timestamp", "proof-no-registry-key", "proof-malformed", "proof-missing",
		"proof-body-unbound", "proof-body-mismatch", "proof-challenge-oversized", "proof-registry-unavailable",
	}, basis["enum"])
	skew := properties["timestamp_skew_seconds"].(map[string]any)
	require.Equal(t, "number", skew["type"])
}
