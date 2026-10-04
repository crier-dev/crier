package permissions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestJSONLStoreRoundTrip writes one record of each kind and proves the folded
// snapshot carries exactly what was written — the store's own acceptance.
func TestJSONLStoreRoundTrip(t *testing.T) {
	s := newStore(t)

	mustAppend(t, s, activePrincipal("prin_bane", "owner", "acme").Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind_1", Principal: "prin_bane", Agent: "atlas", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&Grant{
		ID: "grant_1", Principal: "prin_bane",
		Subject: Subject{Type: SubjectAgent, Ref: "atlas"},
		Actions: []Action{ActionSend, ActionRead}, GrantedBy: "prin_bane", GrantedAt: at(2),
	}).Record(at(2)))
	mustAppend(t, s, (&AgentInfo{
		ID: "atlas", Class: ClassPersonal, Owner: "prin_bane", Namespace: "acme",
		Capabilities: []string{"planning"},
	}).Record(at(3)))

	sn := snap(t, s)
	require.Equal(t, PrincipalActive, sn.Principal("prin_bane").Status)
	require.Equal(t, RoleOwner, sn.Principal("prin_bane").Role)

	b := sn.Binding("prin_bane", "atlas")
	require.NotNil(t, b)
	require.True(t, b.AsAgent)

	g := sn.LiveGrantFor("prin_bane", Subject{Type: SubjectAgent, Ref: "atlas"}, ActionSend, at(4))
	require.NotNil(t, g)
	require.Equal(t, "grant_1", g.ID)

	a := sn.Agent("atlas")
	require.NotNil(t, a)
	require.Equal(t, ClassPersonal, a.Class)
	require.Equal(t, "prin_bane", a.Owner)
	require.Equal(t, []string{"planning"}, a.Capabilities)
}

// TestJSONLStoreKeepLast proves the fold is keep-LAST per (type, id): a later
// record-version at the same id wins, and the older line stays in the log.
func TestJSONLStoreKeepLast(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mustAppend(t, s, (&AgentInfo{ID: "deploy-bot", Class: ClassService}).Record(at(0)))
	mustAppend(t, s, (&AgentInfo{ID: "deploy-bot", Class: ClassPersonal, Owner: "prin_a"}).Record(at(1)))
	// Re-classing writes a new version; the log keeps BOTH.
	mustAppend(t, s, (&AgentInfo{ID: "deploy-bot", Class: ClassPersonal, Owner: "prin_a"}).Record(at(1)))
	mustAppend(t, s, (&AgentInfo{ID: "deploy-bot", Class: ClassService}).Record(at(2)))

	sn := snap(t, s)
	require.Equal(t, ClassService, sn.Agent("deploy-bot").Class)

	recs, err := s.Records(ctx)
	require.NoError(t, err)
	require.Len(t, recs, 4, "every version stays in the log as evidence")

	// An append-only file with no fsync-truncation: restarting the store over
	// the same root folds to the same state.
	s2, err := NewJSONLStore(s.Root())
	require.NoError(t, err)
	require.Equal(t, ClassService, snap(t, s2).Agent("deploy-bot").Class)
	require.NoError(t, s2.Close())
}

// TestRevokeGrantTombstones is §6.6: revocation is a TOMBSTONE, never an edit.
// The record stays, revoked_at is set, and the grant is treated as absent.
func TestRevokeGrantTombstones(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mustAppend(t, s, activePrincipal("prin_viewer", "viewer", "acme").Record(at(0)))
	mustAppend(t, s, (&Grant{
		ID: "grant_v", Principal: "prin_viewer",
		Subject: Subject{Type: SubjectAgent, Ref: "quill"},
		Actions: []Action{ActionRead}, GrantedBy: "prin_owner", GrantedAt: at(1),
	}).Record(at(1)))

	sub := Subject{Type: SubjectAgent, Ref: "quill"}
	require.NotNil(t, snap(t, s).LiveGrantFor("prin_viewer", sub, ActionRead, at(2)))

	require.NoError(t, RevokeGrant(ctx, s, "grant_v", "prin_owner", at(3)))

	sn := snap(t, s)
	require.Nil(t, sn.LiveGrantFor("prin_viewer", sub, ActionRead, at(4)), "a tombstoned grant is absent")

	// The record is still there, with the tombstone — it was never deleted.
	recs, err := s.Records(ctx)
	require.NoError(t, err)
	require.Len(t, recs, 3, "principal + grant + tombstone")
	tomb := sn.GrantsFor("prin_viewer", sub)
	require.Len(t, tomb, 1)
	require.NotNil(t, tomb[0].RevokedAt)
	require.Equal(t, "prin_owner", tomb[0].RevokedBy)
}

// TestRevokeUnknownGrantIsAnError proves a revocation of a grant that does not
// exist does not silently succeed.
func TestRevokeUnknownGrantIsAnError(t *testing.T) {
	s := newStore(t)
	err := RevokeGrant(context.Background(), s, "grant_missing", "prin_owner", at(0))
	require.ErrorIs(t, err, ErrGrantNotFound)
}

// TestJSONLStoreEmptyLogReadsEmpty proves the armed-but-empty state is
// legitimate: a store with no log is an empty snapshot, not an error.
func TestJSONLStoreEmptyLogReadsEmpty(t *testing.T) {
	s := newStore(t)
	sn := snap(t, s)
	require.Nil(t, sn.Principal("nobody"))
	require.Empty(t, sn.Grants())
	require.Nil(t, sn.Agent("nobody"))
	_, err := os.Stat(filepath.Join(s.Root(), storeFileName))
	require.True(t, os.IsNotExist(err), "no log is written until a record is appended")
}

// TestJSONLStoreRefusesCorruptLine proves an unknown line is reported, never
// skipped — the log is the log of record.
func TestJSONLStoreRefusesCorruptLine(t *testing.T) {
	s := newStore(t)
	require.NoError(t, os.WriteFile(s.Path(), []byte("{not json}\n"), 0o644))
	_, err := s.Snapshot(context.Background())
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// TestNullTimeHelpers proves a zero timestamp is stored as NULL rather than
// the year 1, and a real one round-trips.
func TestNullTimeHelpers(t *testing.T) {
	require.Nil(t, nullTime(time.Time{}))
	require.Nil(t, nullTimePtr(nil))
	zero := time.Time{}
	require.Nil(t, nullTimePtr(&zero))
	got := nullTime(at(5))
	require.NotNil(t, got)
	require.True(t, got.Equal(at(5)))
}
