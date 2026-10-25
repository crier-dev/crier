package session

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func lateJoinHistoryContract(t *testing.T, open func() Repository) {
	t.Helper()
	store := open()
	h := newAPIHarness(t, store)
	id := h.createRoom(t)
	first := postText(t, h, id, fixAgentA, "initial context")
	var added participantView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
		map[string]any{"member_type": "agent", "member_id": "nimbus-2"}, &added, nil))
	require.Equal(t, first, added.ContextShare.BoundaryMessageID)
	postText(t, h, id, fixAgentA, "after join")
	view := getContextView(t, h, id, "agent", "nimbus-2")
	require.Equal(t, []string{first}, view.GivenMessageIDs, "summary must stay pinned to its recorded edge")
	assertLateJoinEvent(t, h, id, "nimbus-2", "summary", first, "")
	var changed map[string]any
	require.Equal(t, http.StatusOK, h.do(t, http.MethodPut, "/sessions/"+id+"/participants/agent/nimbus-2/context",
		map[string]any{"mode": "since", "boundary_message_id": first}, &changed, nil))
	require.NoError(t, store.Close())

	// Open another handle on the durable data. A latest-share projection would
	// lose the original summary event, so assert BOTH decisions after restart.
	store = open()
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	h = newAPIHarness(t, store)
	assertLateJoinEvent(t, h, id, "nimbus-2", "summary", first, "")
	assertLateJoinEvent(t, h, id, "nimbus-2", "since", first, "")
	assertLateJoinEvent(t, h, id, fixAgentA, "none", "", "nothing to share")
}

func TestLateJoinSummaryEdgeUsesSeq(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC()
	st := &State{Messages: []*Message{
		{ID: "edge", Seq: 2, CreatedAt: at},
		{ID: "after", Seq: 3, CreatedAt: at.Add(-time.Hour)},
	}}
	for _, mode := range []ContextShareMode{ShareSummary, ShareFull} {
		given := materializedShare(st, &ContextShare{Mode: mode, BoundaryMessageID: "edge"}, at)
		require.Len(t, given, 1)
		require.Equal(t, "edge", given[0].ID, "equal timestamps cannot drop the cited edge; clock skew cannot include a later seq")
	}
}

func TestLateJoinHistoryDurable(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			path := be.pathFn(t)
			lateJoinHistoryContract(t, func() Repository { return be.open(t, path) })
		})
	}
}

func TestLateJoinHistoryPostgres(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("CRIER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL late-join integration requires CRIER_TEST_POSTGRES_DSN (isolated test database)")
	}
	// The shipped pgx adapter owns task schema initialization. The separate
	// database/sql PostgreSQL constructor currently omits chat_tasks; this
	// contract does not seed a fake table to conceal that unrelated defect.
	for _, adapter := range []string{"pgx"} {
		t.Run(adapter, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			admin, err := NewPostgresSQLStore(ctx, dsn)
			require.NoError(t, err)
			schema := fmt.Sprintf("df447_%d", time.Now().UnixNano())
			_, err = admin.db.ExecContext(ctx, `CREATE SCHEMA `+schema)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, e := admin.db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
				require.NoError(t, e)
				require.NoError(t, admin.Close())
			})
			cfg, err := parseLifecyclePGConfig(dsn, schema)
			require.NoError(t, err)
			lateJoinHistoryContract(t, func() Repository {
				repo, e := NewPostgresStore(ctx, cfg)
				require.NoError(t, e)
				return repo
			})
		})
	}
}
