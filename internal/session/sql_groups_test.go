package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CR-CHAT-022 — the SQL-backed group roster (SQLite + PostgreSQL dialects)
// ---------------------------------------------------------------------------

// TestSQLGroupStore_CRUDAndKeepLast drives the SQLGroupStore contract over a
// fresh SQLite view: create, duplicate-create refusal, update preserving the
// creation facts, roster replace on update, list ordering, and the name rule.
func TestSQLGroupStore_CRUDAndKeepLast(t *testing.T) {
	store := newTestSQLiteStore(t)
	gs := store.Groups()
	ctx := context.Background()

	require.NoError(t, gs.Create(ctx, &Group{
		Name: "infra", Namespace: "acme", Members: []string{"atlas", "nimbus"},
		CreatedAt: time.Now(), CreatedBy: "kara", UpdatedAt: time.Now(), UpdatedBy: "kara",
	}))
	require.True(t, errors.Is(gs.Create(ctx, &Group{Name: "infra"}), ErrGroupExists))

	// Read back: the roster, the realm and both actors.
	g, err := gs.Get(ctx, "infra")
	require.NoError(t, err)
	require.Equal(t, []string{"atlas", "nimbus"}, g.Members)
	require.Equal(t, "acme", g.Namespace)
	require.Equal(t, "kara", g.CreatedBy)
	require.Equal(t, "kara", g.UpdatedBy)

	// Update: the roster is replaced in full, the creation facts survive.
	require.NoError(t, gs.Update(ctx, &Group{
		Name: "infra", Namespace: "acme", Members: []string{"orion"},
		CreatedAt: time.Now(), CreatedBy: "WRONG", UpdatedAt: time.Now(), UpdatedBy: "sam",
	}))
	g, err = gs.Get(ctx, "infra")
	require.NoError(t, err)
	require.Equal(t, []string{"orion"}, g.Members)
	require.Equal(t, "kara", g.CreatedBy, "an edit is never a re-create")
	require.Equal(t, "sam", g.UpdatedBy)

	// A second group, and the list is name-sorted.
	require.NoError(t, gs.Create(ctx, &Group{
		Name: "a-team", Members: []string{"atlas"},
		CreatedAt: time.Now(), CreatedBy: "kara", UpdatedAt: time.Now(),
	}))
	all, err := gs.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "a-team", all[0].Name)

	// Not-found, and the @team: name rule shared with the JSONL store.
	_, err = gs.Get(ctx, "nope")
	require.True(t, errors.Is(err, ErrGroupNotFound))
	require.True(t, errors.Is(gs.Update(ctx, &Group{Name: "nope"}), ErrGroupNotFound))
	require.True(t, errors.Is(gs.Create(ctx, &Group{Name: "has space"}), ErrInvalidGroup))

	// A member id must be an ident too.
	require.True(t, errors.Is(gs.Create(ctx, &Group{
		Name: "bad", Members: []string{"not an ident"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}), ErrInvalidGroup), "a non-ident member id is refused")
}

// TestSQLGroupStore_RosterSurvivesReopen: the view is durable — a fresh store
// over the SAME database file reads the same roster (the SQLite restart case).
func TestSQLGroupStore_RosterSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/sessions.sqlite"

	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	require.NoError(t, store.Groups().Create(ctx, &Group{
		Name: "infra", Members: []string{"atlas", "nimbus"},
		CreatedAt: time.Now(), CreatedBy: "kara", UpdatedAt: time.Now(), UpdatedBy: "kara",
	}))
	require.NoError(t, store.Close())

	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	g, err := reopened.Groups().Get(ctx, "infra")
	require.NoError(t, err)
	require.Equal(t, []string{"atlas", "nimbus"}, g.Members)
	require.Equal(t, "kara", g.CreatedBy)
}
