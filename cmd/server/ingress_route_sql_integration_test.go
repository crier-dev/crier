//go:build integration

package main

// ingress_route_sql_integration_test.go — the END-TO-END PostgreSQL arm of
// CRIER-INGRESS §3: a real server wiring (openIngress) over a real PostgreSQL
// session store, an inbox-mode push through the handler, and the resulting row
// read back from the database.
//
// It closes the gap the unit batteries leave between them: internal/session
// proves the row is projected, cmd/server's unit test proves the selection
// picks the SQL store for a sqlite repository, and this proves the same
// selection + write on the engine OpenStore picks for
// CR_SESSION_BACKEND=postgres (the pgxpool-backed PostgresStore).
//
//	go test -tags=integration -count=1 -run TestIngressRouteRowOnPostgres -timeout 5m ./cmd/server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

// integrationInbox is the ingress.InboxWriter the production wiring satisfies
// with the registry's delivery core; here it accepts and names an entry.
type integrationInbox struct {
	entryID string
	target  string
}

func (i integrationInbox) Deliver(_ context.Context, agentID, _ string, _ ingress.InboxMessage) ([]session.DeliveryOutcome, error) {
	target := i.target
	if target == "" {
		target = agentID
	}
	return []session.DeliveryOutcome{{
		Target: target, Outcome: session.OutcomeAccepted, InboxEntryID: i.entryID, UpdatedAt: time.Now().UTC(),
	}}, nil
}

func TestIngressRouteRowOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("crier"), tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	repo, err := session.OpenStore(ctx, config.SessionBackendPostgres, session.StoreOptions{DatabaseURL: url})
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	root := t.TempDir()
	cfg := config.Config{}
	cfg.Session.LogRoot = root
	h, err := openIngress(cfg, repo, nil, nil, nil)
	require.NoError(t, err)
	now := time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)
	h.Now = func() time.Time { return now }
	h.Inbox = integrationInbox{entryID: "e_pg1", target: "bravo"}

	// A capability push: the delivery path resolves the holder, which is the
	// agent the route row must name.
	body, err := json.Marshal(map[string]any{
		"mode": "inbox", "target": map[string]any{"capability": "build"},
		"author": map[string]string{"agent": "ci-bot"}, "text": "build 4412 failed", "tags": []string{"build"},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/ingress/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	var (
		kind, agentID, capability, messageID, authorID string
		tags                                           *string
		createdAt                                      time.Time
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT kind, agent_id, capability, message_id, author_id, tags, created_at
		FROM chat_ingress_routes`).
		Scan(&kind, &agentID, &capability, &messageID, &authorID, &tags, &createdAt))
	require.Equal(t, "ingress.route", kind)
	require.Equal(t, "bravo", agentID, "the row must name the agent the capability resolved to")
	require.Equal(t, "build", capability)
	require.Equal(t, "e_pg1", messageID)
	require.Equal(t, "ci-bot", authorID)
	require.NotNil(t, tags)
	require.JSONEq(t, `["build"]`, *tags)
	require.True(t, now.Equal(createdAt), "created_at = %s, want %s", createdAt, now)

	// The SQL backend writes no JSONL route log: the receipt lives where the
	// messages live. (The ingress DIRECTORY exists — the idempotency ledger
	// shares it — but routes.jsonl must not.)
	_, err = os.Stat(filepath.Join(root, "ingress", "routes.jsonl"))
	require.True(t, os.IsNotExist(err), "postgres backend must not write the JSONL route log (stat err = %v)", err)
}
