//go:build integration

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestA2AAckEvidencePostgres(t *testing.T) {
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
	testA2AAckEvidence(t, store)
	base := reloadedA2AServer(t, store)
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", "")))
	before := map[string]map[string]any{}
	for _, raw := range listing["tasks"].([]any) {
		id := raw.(map[string]any)["id"].(string)
		before[id] = rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
	}
	require.Len(t, before, 3)
	store.Close()
	reopened, err := registry.NewPostgresStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(reopened.Close)
	base = reloadedA2AServer(t, reopened)
	for id, want := range before {
		got := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
		require.Equal(t, want, got, "full evidence survives PostgreSQL pool reopen")
		require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, got))
	}
}
