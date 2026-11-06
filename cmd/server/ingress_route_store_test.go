package main

// ingress_route_store_test.go — CRIER-INGRESS §3's selection rule: the route
// receipt goes to the store that is actually answering. A SQL session backend
// projects a row; the JSONL backend keeps the JSONL log, byte for byte.
//
// This is the wiring arm of the acceptance ("on sqlite and postgres backends an
// inbox-mode push writes an ingress.route row"): the assertion is made on what
// openIngress would wire, for BOTH SQL repositories OpenStore can return.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

func TestIngressRouteStoreSelectionFollowsTheSessionBackend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	jsonlRepo, err := session.OpenStore(ctx, config.SessionBackendJSONL, session.StoreOptions{LogRoot: root})
	require.NoError(t, err)
	route, err := ingressRouteStore(jsonlRepo, root)
	require.NoError(t, err)
	require.IsType(t, &ingress.JSONLRouteStore{}, route,
		"the JSONL backend keeps the JSONL route log")

	sqlRepo, err := session.NewSQLiteStore(filepath.Join(root, "sessions.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlRepo.Close() })
	route, err = ingressRouteStore(sqlRepo, root)
	require.NoError(t, err)
	require.IsType(t, &ingress.SQLRouteStore{}, route,
		"the sqlite backend projects a route row")

	// The capability the selection turns on is implemented by BOTH SQL session
	// stores OpenStore can return — sqlite/postgres through the shared
	// SQLStore, and postgres through the pgxpool-backed PostgresStore (which
	// OpenStore selects for CR_SESSION_BACKEND=postgres). A compile-time
	// assertion keeps a future engine from silently losing the receipt and
	// falling back to a JSONL log that a SQL deployment does not read.
	var (
		_ session.IngressRouteWriter = (*session.SQLStore)(nil)
		_ session.IngressRouteWriter = (*session.PostgresStore)(nil)
	)
}
