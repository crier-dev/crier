package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func TestTrustBackendParity(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemoryStore()
			if backend == "sqlite" {
				sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "trust.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
				s = sqlite
			}
			exerciseTrustBackendParity(t, s)
		})
	}
}

// The integration-tagged PostgreSQL test calls this SAME table through the
// same AuthorityStore/HTTP adapter. Assert complete verdicts, not just names.
func exerciseTrustBackendParity(t *testing.T, backend Store) {
	t.Helper()
	s := NewAuthorityStore(backend, nil, "")
	exerciseTrustStore(t, s)
	a, first := registryTrustFixture()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	middle := ed25519.NewKeyFromSeed(seed)
	seed[0] = 2
	leaf := ed25519.NewKeyFromSeed(seed)
	first.Scope = []string{"message-signing", trust.TransitiveCapability}
	first.MaxDepth = 1
	trust.SignChainDelegation(&first, root)
	second := trust.ChainDelegation{Voucher: trust.ChainKeyID(first.SubjectKey), SubjectKey: hex.EncodeToString(leaf.Public().(ed25519.PublicKey)), Scope: []string{"message-signing"}, IssuedAt: first.IssuedAt}
	trust.SignChainDelegation(&second, middle)
	require.NoError(t, s.AddTrustDelegation(first))
	require.NoError(t, s.AddTrustDelegation(second))
	chain := []trust.ChainDelegation{first, second}
	unknown := append([]trust.ChainDelegation{}, chain...)
	unknown[0].Voucher = "unknown"
	depth := append([]trust.ChainDelegation{}, chain...)
	depth[0].MaxDepth = 0
	trust.SignChainDelegation(&depth[0], root)
	bad := append([]trust.ChainDelegation{}, chain...)
	bad[1].Signature = first.Signature
	h := NewTrustHandler(s, "operator-secret", "reach-secret")
	for _, tc := range []struct {
		name  string
		chain []trust.ChainDelegation
		scope []string
		want  trust.ChainVerdict
	}{
		{"verified", chain, []string{"message-signing"}, trust.ChainVerdict{Verdict: trust.TrustChainReachable, Basis: trust.TrustBasisChainOnly, AnchorID: a.ID, Depth: 2}},
		{"unknown", unknown, []string{"message-signing"}, trust.ChainVerdict{Verdict: trust.TrustUnknownAnchor}},
		{"depth", depth, []string{"message-signing"}, trust.ChainVerdict{Verdict: trust.TrustDepthExceeded, Link: 1}},
		{"scope", chain, []string{"admin"}, trust.ChainVerdict{Verdict: trust.TrustScopeInsufficient, Link: 1, MissingCapability: "admin"}},
		{"signature", bad, []string{"message-signing"}, trust.ChainVerdict{Verdict: trust.TrustBadSignature, Link: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"signer": second.SubjectKey, "scope": tc.scope, "chain": tc.chain})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			h.HandleVerify(w, httptest.NewRequest("POST", "/trust/verify", strings.NewReader(string(raw))))
			require.Equal(t, 200, w.Code, w.Body.String())
			var got trust.ChainVerdict
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Equal(t, tc.want, got)
			wantJSON, err := json.Marshal(tc.want)
			require.NoError(t, err)
			require.JSONEq(t, string(wantJSON), w.Body.String())
			t.Logf("POST /trust/verify -> %d %s", w.Code, w.Body.String())
		})
	}
	exerciseMalformedTrustRecords(t, s)
	// Empty scope is a valid signed empty grant, not nil and not a wildcard.
	first.Scope = []string{}
	trust.SignChainDelegation(&first, root)
	require.NoError(t, s.AddTrustDelegation(first))
	records, err := s.ListTrustDelegations(first.SubjectKey)
	require.NoError(t, err)
	found := false
	for _, d := range records {
		if d.Signature == first.Signature {
			require.Equal(t, first, d)
			found = true
		}
	}
	require.True(t, found, "signed empty capability list must survive storage")
}

func exerciseMalformedTrustRecords(t *testing.T, s TrustStore) {
	t.Helper()
	a, d := registryTrustFixture()
	for _, tc := range []struct {
		name   string
		mutate func(*trust.ChainAnchor)
		want   string
	}{
		{"blank-id", func(a *trust.ChainAnchor) { a.ID = " " }, "anchor id, label and created_at are required"},
		{"blank-label", func(a *trust.ChainAnchor) { a.Label = "\t" }, "anchor id, label and created_at are required"},
		{"nul-id", func(a *trust.ChainAnchor) { a.ID += "\x00" }, "anchor id and label must not contain NUL"},
		{"nul-label", func(a *trust.ChainAnchor) { a.Label += "\x00" }, "anchor id and label must not contain NUL"},
		{"nonhex-key", func(a *trust.ChainAnchor) { a.PublicKey = strings.Repeat("z", 64) }, "public key is not 32-byte hex ed25519: trust: verification request is malformed (TRUST_MALFORMED)"},
		{"short-key", func(a *trust.ChainAnchor) { a.PublicKey = "ab" }, "public key is not 32-byte hex ed25519: trust: verification request is malformed (TRUST_MALFORMED)"},
		{"nul-key", func(a *trust.ChainAnchor) { a.PublicKey += "\x00" }, "public key is not 32-byte hex ed25519: trust: verification request is malformed (TRUST_MALFORMED)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := a
			tc.mutate(&bad)
			err := s.AddTrustAnchor(bad)
			require.ErrorIs(t, err, ErrInvalidStoreInput)
			require.EqualError(t, err, "invalid store input: "+tc.want)
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*trust.ChainDelegation)
		want   string
	}{
		{"depth-over-limit", func(d *trust.ChainDelegation) { d.MaxDepth = trust.DefaultMaxChainDepth + 1 }, fmt.Sprintf("voucher, issued_at and max_depth in [0,%d] are required", trust.DefaultMaxChainDepth)},
		{"nul-voucher", func(d *trust.ChainDelegation) { d.Voucher += "\x00" }, "voucher must not contain NUL"},
		{"nil-scope", func(d *trust.ChainDelegation) { d.Scope = nil }, "scope must be a capability list (use [] for no capabilities)"},
		{"nul-scope", func(d *trust.ChainDelegation) { d.Scope = []string{"a\x00b"} }, "scope capabilities must not contain NUL"},
		{"blank-scope", func(d *trust.ChainDelegation) { d.Scope = []string{" "} }, "scope capabilities must be nonempty and unique"},
		{"duplicate-scope", func(d *trust.ChainDelegation) { d.Scope = []string{"a", "a"} }, "scope capabilities must be nonempty and unique"},
		{"bad-subject", func(d *trust.ChainDelegation) { d.SubjectKey = strings.Repeat("g", 64) }, "public key is not 32-byte hex ed25519: trust: verification request is malformed (TRUST_MALFORMED)"},
		{"bad-signature", func(d *trust.ChainDelegation) { d.Signature = strings.Repeat("z", 128) }, "signature must be 64-byte hex ed25519"},
		{"short-signature", func(d *trust.ChainDelegation) { d.Signature = "ab" }, "signature must be 64-byte hex ed25519"},
		{"nul-signature", func(d *trust.ChainDelegation) { d.Signature += "\x00" }, "signature must be 64-byte hex ed25519"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := d
			tc.mutate(&bad)
			err := s.AddTrustDelegation(bad)
			require.ErrorIs(t, err, ErrInvalidStoreInput)
			require.EqualError(t, err, "invalid store input: "+tc.want)
		})
	}
}

func TestSQLiteTrustReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trust.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	exerciseTrustStore(t, s)
	require.NoError(t, s.Close())
	// A new handle and actual migrated file, not an in-process map readback.
	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NotSame(t, s, reopened)
	a, d := registryTrustFixture()
	anchors, err := reopened.ListTrustAnchors()
	require.NoError(t, err)
	require.Equal(t, []trust.ChainAnchor{a}, anchors)
	chain, err := reopened.ListTrustDelegations(d.SubjectKey)
	require.NoError(t, err)
	require.Equal(t, []trust.ChainDelegation{d}, chain)
	require.Equal(t, 123456789, chain[0].IssuedAt.Nanosecond())
	require.Equal(t, trust.ChainVerdict{Verdict: trust.TrustChainReachable, Basis: trust.TrustBasisChainOnly, AnchorID: a.ID, Depth: 1}, trust.VerifyChain(anchors, d.SubjectKey, d.Scope, chain))
}
