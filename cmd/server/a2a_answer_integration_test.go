//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
)

func TestA2AAnswerPostgresReopen(t *testing.T) {
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
	serving := registry.NewAuthorityStore(store, nil, "")
	h := registry.NewHandler(serving)
	bus := relay.New(0)
	r := mux.NewRouter()
	r.HandleFunc("/relay/publish", bus.HandlePublish).Methods(http.MethodPost)
	registerA2ARoute(r, serving, h.HandleDeliver, h.HandleUpdateAgent, bus, a2aOptions{streamPoll: time.Millisecond, streamBudget: 10 * time.Second})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	expected := make(map[string]map[string]any)
	for _, scenario := range []struct{ outputFirst, noStream bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		outputFirst := scenario.outputFirst
		accepted := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, srv.URL, sendMessageBody("terminal-agent", a2a.MethodSendMessage, fmt.Sprintf("pg-output-%t-no-stream-%t", outputFirst, scenario.noStream)))))
		id := taskIDOf(t, accepted)
		var stream *sseStream
		if !scenario.noStream {
			stream = openSSE(t, srv.URL, subscribeBody("terminal-agent", id))
			stream.next(t, "opening task")
		}
		if outputFirst {
			publishAgentAnswer(t, srv.URL, "terminal-agent", id)
			if stream != nil {
				stream.next(t, "answer before ack")
			}
			got := rpcResult(t, mustA2AOK(t, http.DefaultClient, srv.URL, getTaskBody("terminal-agent", id, "")))
			require.Equal(t, string(a2a.TaskStateSubmitted), rpcTaskState(t, got), "output alone never proves acknowledgement")
		}
		_, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
		require.NoError(t, err)
		require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
		before, found, err := store.TaskObservation("terminal-agent", id)
		require.NoError(t, err)
		require.True(t, found)
		if !outputFirst {
			if stream != nil {
				stream.nextStatus(t, string(a2a.TaskStateWorking))
			}
			publishAgentAnswer(t, srv.URL, "terminal-agent", id)
			if stream != nil {
				stream.next(t, "answer after ack")
			}
		}
		if stream != nil {
			stream.nextStatus(t, string(a2a.TaskStateCompleted))
			stream.waitClosed(t)
		}
		after, found, err := store.TaskObservation("terminal-agent", id)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, before, after, "output must not rewrite immutable ack evidence")
		got := rpcResult(t, mustA2AOK(t, http.DefaultClient, srv.URL, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, got))
		require.Len(t, got["artifacts"], 1)
		expected[id] = got
	}
	srv.Close()
	store.Close()
	reopened, err := registry.NewPostgresStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)
	base := reloadedA2AServer(t, reopened)
	for id, want := range expected {
		got := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, want, got, "answer, state, timestamps and request history survive PostgreSQL reopen")
	}
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", `"includeArtifacts":true`)))
	require.Len(t, rpcTasks(t, listing), len(expected))
	for _, task := range rpcTasks(t, listing) {
		require.Equal(t, expected[task["id"].(string)], task)
	}
}
