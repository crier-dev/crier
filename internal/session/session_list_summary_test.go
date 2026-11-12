package session

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/namespace"
)

func testNamespaceRegistry(t *testing.T, names ...string) *namespace.Registry {
	t.Helper()
	policies := make([]namespace.Policy, 0, len(names))
	for _, name := range names {
		policies = append(policies, namespace.Policy{Name: name, Auth: namespace.AuthShared})
	}
	reg, err := namespace.NewRegistry(policies, nil)
	require.NoError(t, err)
	return reg
}

func TestSQLSessionListSummaryMatchesStateView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	recs := buildScenario()
	appendRecords(t, store, recs)

	summaries, err := store.SessionSummaries(ctx)
	require.NoError(t, err)
	members, err := store.SessionMemberships(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.NotEmpty(t, members)

	h := newAPIHarness(t, store)
	var fast sessionsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions", nil, &fast, nil))
	state, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	slow := sessionsResponse{Sessions: []sessionView{h.h.viewOf(state)}, Count: 1}
	fastJSON, err := json.Marshal(fast)
	require.NoError(t, err)
	slowJSON, err := json.Marshal(slow)
	require.NoError(t, err)
	require.Equal(t, slowJSON, fastJSON)
}

func TestSQLSessionListSummaryFiltersAndMembershipClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	appendRecords(t, store, []*Record{
		(&Session{ID: "a", Namespace: "realm-a", Kind: KindChannel, CreatedAt: at(1), CreatedBy: AuthorRef{Agent: fixAgentA}}).CreateRecord(1, at(1)),
		(&Session{ID: "b", Namespace: "realm-a", Kind: KindDirect, CreatedAt: at(2), CreatedBy: AuthorRef{Agent: fixAgentA}}).CreateRecord(1, at(2)),
		(&Session{ID: "c", Namespace: "realm-b", Kind: KindChannel, CreatedAt: at(3), CreatedBy: AuthorRef{Agent: fixAgentA}}).CreateRecord(1, at(3)),
	})
	for i, id := range []string{"a", "b"} {
		member := &Member{SessionID: id, MemberType: MemberAgent, MemberID: fixAgentA, Role: RoleMember, AddedAt: at(4)}
		require.NoError(t, store.Append(ctx, member.AddRecord(int64(i+2), at(4), nil)))
	}

	h := newAPIHarness(t, store)
	h.h.opts.Namespaces = testNamespaceRegistry(t, "realm-a", "realm-b")
	h.h.now = func() time.Time { return at(5) }
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"realm", "", []string{"a", "b"}},
		{"kind", "?kind=direct", []string{"b"}},
		{"participants", "?min_participants=1", []string{"a", "b"}},
		{"none", "?min_messages=1", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got sessionsResponse
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions"+tc.query, nil, &got, map[string]string{"X-Crier-Namespace": "realm-a"}))
			ids := make([]string, 0, len(got.Sessions))
			for _, session := range got.Sessions {
				ids = append(ids, session.ID)
			}
			require.Equal(t, tc.want, ids)
		})
	}
}

func TestJSONLStoreKeepsStateLoadingListFallback(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	id := h.createRoom(t)
	var got sessionsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions", nil, &got, nil))
	require.Equal(t, []string{id}, []string{got.Sessions[0].ID})
}

// ch:trace row=PERF-002 spec=none:board-acceptance wave=none:serial-fallback test=TestSQLSessionListSummaryMatchesStateView doc=docs/performance.md evidence=docs/perf-002-benchmark.txt witness=none:live-pg-not-required verdict=none:foreman-judge commit=none:worker-commit memory=none:foreman-write
