package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

// Boot production run(), with BOTH distinct credentials and ACL disabled:
// trust is not accidentally coupled to delivery-policy enablement.
func TestTrustHTTPRoutes(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) { exerciseTrustHTTPRoutes(t, backend) })
	}
}

func exerciseTrustHTTPRoutes(t *testing.T, backend string) {
	t.Helper()
	base := startTestServerWithEnv(t, map[string]string{"CR_REGISTRY_BACKEND": backend, "CR_REGISTRY_SQLITE_PATH": filepath.Join(t.TempDir(), "trust.db"), "CR_AUTH_TOKEN": df297MessageToken, "CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken, "CR_PERMISSIONS_ENABLED": "false", "CR_DATABASE_URL": ""})
	client := &http.Client{Timeout: 5 * time.Second}
	evidence := map[string]any{}
	call := func(method, path, token string, body any, want int) string {
		t.Helper()
		raw := ""
		if body != nil {
			b, err := json.Marshal(body)
			require.NoError(t, err)
			raw = string(b)
		}
		status, response := df297Request(t, client, method, base+path, token, "", raw)
		require.Equal(t, want, status, "%s %s: %s", method, path, response)
		t.Logf("HTTP %s %s -> %d %s", method, path, status, response)
		if want == 200 || want == 201 {
			evidence[method+" "+path] = map[string]any{"request": body, "response": json.RawMessage(response), "status": status}
		}
		return response
	}
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	middle := ed25519.NewKeyFromSeed(seed)
	seed[0] = 2
	leaf := ed25519.NewKeyFromSeed(seed)
	pub := func(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Public().(ed25519.PublicKey)) }
	a := trust.ChainAnchor{ID: "anchor-test", PublicKey: pub(root), Label: "Test operator"}
	chain := []trust.ChainDelegation{
		{Voucher: a.ID, SubjectKey: pub(middle), Scope: []string{"message-signing", trust.TransitiveCapability}, MaxDepth: 1, IssuedAt: time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)},
		{Voucher: trust.ChainKeyID(pub(middle)), SubjectKey: pub(leaf), Scope: []string{"message-signing"}, MaxDepth: 0, IssuedAt: time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)},
	}
	trust.SignChainDelegation(&chain[0], root)
	trust.SignChainDelegation(&chain[1], middle)
	verify := func(c []trust.ChainDelegation, scope []string, want string) trust.ChainVerdict {
		t.Helper()
		response := call("POST", "/trust/verify", df297MessageToken, map[string]any{"signer": pub(leaf), "scope": scope, "chain": c}, 200)
		var verdict trust.ChainVerdict
		require.NoError(t, json.Unmarshal([]byte(response), &verdict))
		require.Equal(t, want, verdict.Verdict)
		evidence[want] = json.RawMessage(response)
		evidence["example_"+want] = map[string]any{"request": map[string]any{"signer": pub(leaf), "scope": scope, "chain": c}, "response": json.RawMessage(response)}
		return verdict
	}
	verify(chain, []string{"message-signing"}, trust.TrustUnknownAnchor)
	for _, path := range []string{"/trust/anchors", "/trust/delegations"} {
		call("POST", path, df297MessageToken, a, 403)
		call("GET", path, df297MessageToken, nil, 403)
		call("POST", path, "", a, 401)
	}
	call("GET", "/trust/anchors", df297AdminToken, nil, 200)
	call("POST", "/trust/anchors", df297AdminToken, a, 201)
	call("POST", "/trust/anchors", df297AdminToken, a, 409)
	response := call("GET", "/trust/anchors", df297AdminToken, nil, 200)
	var held struct {
		Anchors []trust.ChainAnchor `json:"anchors"`
	}
	require.NoError(t, json.Unmarshal([]byte(response), &held))
	require.Len(t, held.Anchors, 1)
	require.False(t, held.Anchors[0].CreatedAt.IsZero())
	require.Equal(t, a.PublicKey, held.Anchors[0].PublicKey)
	for _, d := range chain {
		call("POST", "/trust/delegations", df297AdminToken, d, 201)
	}
	response = call("GET", "/trust/delegations?subject="+pub(leaf), df297AdminToken, nil, 200)
	var got struct {
		Delegations []trust.ChainDelegation `json:"delegations"`
	}
	require.NoError(t, json.Unmarshal([]byte(response), &got))
	require.Equal(t, chain[1:], got.Delegations)
	v := verify(chain, []string{"message-signing"}, trust.TrustVerified)
	require.Equal(t, a.ID, v.AnchorID)
	require.Equal(t, 2, v.Depth)
	v = verify(chain, []string{"admin"}, trust.TrustScopeInsufficient)
	require.Equal(t, "admin", v.MissingCapability)
	depth := append([]trust.ChainDelegation{}, chain...)
	depth[0].MaxDepth = 0
	trust.SignChainDelegation(&depth[0], root)
	require.Equal(t, 1, verify(depth, []string{"message-signing"}, trust.TrustDepthExceeded).Link)
	bad := append([]trust.ChainDelegation{}, chain...)
	bad[1].Signature = chain[0].Signature
	require.Equal(t, 2, verify(bad, []string{"message-signing"}, trust.TrustBadSignature).Link)
	call("POST", "/trust/delegations", df297AdminToken, bad[1], 400)
	call("GET", "/trust/delegations", df297AdminToken, nil, 400)
	call("POST", "/trust/verify", df297MessageToken, map[string]any{"signer": "not-hex"}, 400)
	// Operator credentials did not gain reach outside the management surface.
	call("POST", "/trust/verify", df297AdminToken, map[string]any{}, 403)
	call("GET", "/relay/topics", df297AdminToken, nil, 403)
	if dir := os.Getenv("DF362_EVIDENCE"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0700))
		b, err := json.MarshalIndent(evidence, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "http.json"), b, 0600))
	}
}

func TestTrustOperatorUnarmed(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_AUTH_TOKEN": "", "CR_PERMISSIONS_ADMIN_TOKEN": "", "CR_PERMISSIONS_ENABLED": "false"})
	status, _ := df297Request(t, &http.Client{Timeout: 5 * time.Second}, "POST", base+"/trust/anchors", "", "", `{}`)
	require.Equal(t, 403, status)
}
