//go:build integration

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestA2APersistence_PostgresRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine", tcpostgres.WithDatabase("crier"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	store, err := registry.NewPostgresStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(store.Close)
	registerStoreAgent(t, store, "terminal-agent", true)
	base := reloadedA2AServer(t, store)
	expected := make(map[string]map[string]any)
	for _, outcome := range []string{"ack", "cancel"} {
		accepted := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, base,
			sendMessageBody("terminal-agent", a2a.MethodSendMessage, "durable-"+outcome))))
		id := taskIDOf(t, accepted)
		stream := openSSE(t, base, subscribeBody("terminal-agent", id))
		stream.next(t, "opening task")
		state := a2a.TaskStateCanceled
		if outcome == "ack" {
			entries, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
			state = a2a.TaskStateCompleted
		} else {
			rpcResult(t, mustA2AOK(t, http.DefaultClient, base, cancelTaskBody("terminal-agent", id)))
		}
		stream.nextStatus(t, string(state))
		stream.waitClosed(t)
		task := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, string(state), rpcTaskState(t, task))
		require.NotEmpty(t, task["history"], "the creating message must survive with its terminal evidence")
		expected[id] = task
	}
	// Closing the pool and reopening a store models an actual store reload, not
	// merely a fresh HTTP handler sharing a process-local evidence map.
	store.Close()
	reopened, err := registry.NewPostgresStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)
	base = reloadedA2AServer(t, reopened)
	for id, want := range expected {
		got := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, want, got, "state, timestamp, identity, context and history survive pool reopen")
	}
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", "")))
	require.Len(t, listing["tasks"], 2)
	for _, raw := range listing["tasks"].([]any) {
		task := raw.(map[string]any)
		require.Equal(t, expected[task["id"].(string)], task)
	}
	// The SQL upsert cannot downgrade a terminal fact or rewrite its timestamp.
	for id, want := range expected {
		o, found, err := reopened.TaskObservation("terminal-agent", id)
		require.NoError(t, err)
		require.True(t, found)
		o.Outcome = "unknown"
		o.ObservedAt = time.Now().UTC()
		require.NoError(t, reopened.RecordTaskObservation("terminal-agent", o))
		got := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, want, got)
	}
	// Real SQL failure: a failed evidence insert must roll back cancellation's
	// DELETE, leaving a retrievable entry rather than an invented CANCELED.
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()
	for id := range expected {
		_, err := pool.Exec(ctx, `DELETE FROM a2a_task_observations WHERE agent_id = $1 AND task_id = $2`, "terminal-agent", id)
		require.NoError(t, err)
		_, raw, _ := a2aRPC(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, ""))
		code, _ := rpcError(t, raw)
		require.Equal(t, a2a.CodeTaskNotFoundError, code, "removed evidence must not manufacture a terminal state")
		break
	}
	_, err = pool.Exec(ctx, `CREATE FUNCTION reject_a2a_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected observation failure'; END; $$;
CREATE TRIGGER reject_a2a_observation BEFORE INSERT ON a2a_task_observations FOR EACH ROW EXECUTE FUNCTION reject_a2a_observation();`)
	require.NoError(t, err)
	task := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, base, sendMessageBody("terminal-agent", a2a.MethodSendMessage, "rollback"))))
	id := taskIDOf(t, task)
	_, raw, _ := a2aRPC(t, http.DefaultClient, base, cancelTaskBody("terminal-agent", id))
	code, _ := rpcError(t, raw)
	require.Equal(t, a2a.CodeInternalError, code)
	entry, err := reopened.Peek("terminal-agent", id)
	require.NoError(t, err)
	require.Equal(t, id, entry.ID)
	_, found, err := reopened.TaskObservation("terminal-agent", id)
	require.NoError(t, err)
	require.False(t, found)
}
