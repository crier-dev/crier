package permissions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrincipalReadFingerprintOnly(t *testing.T) {
	const token = "fixture-only-token-for-fingerprint"
	var p Principal
	require.NoError(t, json.Unmarshal([]byte(`{"id":"prin_one","kind":"principal","status":"active","token":"`+token+`","token_hash":"never-return-this-hash","token_fingerprint":"forged"}`), &p))
	sum := sha256.Sum256([]byte(token))
	require.Equal(t, hex.EncodeToString(sum[:4]), p.TokenFingerprint)
	encoded, err := json.Marshal(p.Record(time.Now().UTC()))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), token)
	require.NotContains(t, string(encoded), hex.EncodeToString(sum[:]))
	require.NotContains(t, string(encoded), "token")
	// Reuse must clear the previous projection; a JSON fingerprint alone is not
	// evidence of a token. No-token mint/append bytes stay unchanged.
	require.NoError(t, json.Unmarshal([]byte(`{"id":"prin_one","token_fingerprint":"forged"}`), &p))
	require.Empty(t, p.TokenFingerprint)
	require.NoError(t, json.Unmarshal([]byte(`{"id":"prin_one","token":""}`), &p))
	require.Empty(t, p.TokenFingerprint)
}

func TestSnapshotManagementInventories(t *testing.T) {
	now := time.Now().UTC()
	snapshot, err := Replay([]*Record{
		(&Principal{ID: "prin_z", Status: PrincipalActive}).Record(now),
		(&Principal{ID: "prin_a", Status: PrincipalActive}).Record(now),
		(&Principal{ID: "prin_a", Status: PrincipalSuspended}).Record(now.Add(-time.Hour)),
		(&Binding{ID: "bind_old", Principal: "prin_z", Agent: "z", AsAgent: true}).Record(now),
		(&Binding{ID: "bind_new", Principal: "prin_z", Agent: "z", AsAgent: false}).Record(now.Add(-time.Hour)),
		(&Binding{ID: "bind_a", Principal: "prin_a", Agent: "z", AsAgent: true}).Record(now),
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Principals(), 2)
	require.Equal(t, "prin_a", snapshot.Principals()[0].ID)
	require.Equal(t, PrincipalSuspended, snapshot.Principals()[0].Status)
	require.Len(t, snapshot.Bindings(), 2)
	require.Equal(t, "bind_a", snapshot.Bindings()[0].ID)
	require.Equal(t, "bind_new", snapshot.Bindings()[1].ID)
	require.False(t, snapshot.Bindings()[1].AsAgent)
	empty, err := Replay(nil)
	require.NoError(t, err)
	require.NotNil(t, empty.Principals())
	require.NotNil(t, empty.Bindings())
	var absent *Snapshot
	require.Empty(t, absent.Principals())
	require.Empty(t, absent.Bindings())
}
