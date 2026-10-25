package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

func registryTrustFixture() (trust.ChainAnchor, trust.ChainDelegation) {
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	leaf := ed25519.NewKeyFromSeed(seed)
	at := time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)
	a := trust.ChainAnchor{ID: "test-anchor", PublicKey: hex.EncodeToString(root.Public().(ed25519.PublicKey)), Label: "Test operator", CreatedAt: at.Truncate(time.Microsecond)}
	d := trust.ChainDelegation{Voucher: a.ID, SubjectKey: hex.EncodeToString(leaf.Public().(ed25519.PublicKey)), Scope: []string{"message-signing"}, MaxDepth: 0, IssuedAt: at}
	trust.SignChainDelegation(&d, root)
	return a, d
}

// One contract is run on BOTH real backend implementations. PostgreSQL calls
// this same function after applying the embedded migration set in a container.
func exerciseTrustStore(t *testing.T, s TrustStore) {
	t.Helper()
	a, d := registryTrustFixture()
	empty, err := s.ListTrustAnchors()
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)
	require.NoError(t, s.AddTrustAnchor(a))
	require.ErrorIs(t, s.AddTrustAnchor(a), ErrTrustExists)
	conflict := a
	conflict.PublicKey = d.SubjectKey
	require.ErrorIs(t, s.AddTrustAnchor(conflict), ErrTrustExists)
	alias := a
	alias.ID = "alias"
	alias.PublicKey = strings.ToUpper(a.PublicKey)
	require.ErrorIs(t, s.AddTrustAnchor(alias), ErrTrustExists)
	anchors, err := s.ListTrustAnchors()
	require.NoError(t, err)
	require.Len(t, anchors, 1)
	require.Equal(t, a.ID, anchors[0].ID)
	require.Equal(t, a.Label, anchors[0].Label)
	require.True(t, a.CreatedAt.Equal(anchors[0].CreatedAt))
	require.NoError(t, s.AddTrustDelegation(d))
	require.ErrorIs(t, s.AddTrustDelegation(d), ErrTrustExists)
	records, err := s.ListTrustDelegations(strings.ToUpper(d.SubjectKey))
	require.NoError(t, err)
	require.Equal(t, []trust.ChainDelegation{d}, records)
	require.Equal(t, trust.TrustChainReachable, trust.VerifyChain(anchors, d.SubjectKey, d.Scope, records).Verdict)
	// Returned scopes must not mutate the held signature or widen its authority.
	records[0].Scope[0] = "admin"
	again, err := s.ListTrustDelegations(d.SubjectKey)
	require.NoError(t, err)
	require.Equal(t, d, again[0])
	bad := d
	bad.Scope = []string{"admin"}
	require.ErrorIs(t, s.AddTrustDelegation(bad), ErrInvalidStoreInput)
	bad = d
	bad.Voucher = "unheld"
	require.ErrorIs(t, s.AddTrustDelegation(bad), ErrInvalidStoreInput)
	bad = d
	bad.MaxDepth = -1
	require.ErrorIs(t, s.AddTrustDelegation(bad), ErrInvalidStoreInput)
	emptyD, err := s.ListTrustDelegations(a.PublicKey)
	require.NoError(t, err)
	require.NotNil(t, emptyD)
	require.Empty(t, emptyD)
}

func TestMemoryTrustStore(t *testing.T) { exerciseTrustStore(t, NewMemoryStore()) }
func TestAuthorityTrustStore(t *testing.T) {
	t.Parallel()
	exerciseTrustStore(t, NewAuthorityStore(NewMemoryStore(), nil, ""))
}
