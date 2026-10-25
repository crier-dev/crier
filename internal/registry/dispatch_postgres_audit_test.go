package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"
)

// This test starts its OWN disposable Postgres, Unix socket only, with no
// Docker or existing database connection. It skips explicitly when local
// binaries are unavailable (or initdb cannot run as the current root user).
func TestDispatchPostgresSerializedRoundTripsAndLegacyMigration(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("scratch initdb refuses root; run as an unprivileged user")
	}
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
		if len(matches) == 0 {
			t.Skip("local PostgreSQL binaries unavailable; scratch persistence test not run")
		}
		initdb = matches[len(matches)-1]
	}
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	// Keep the Unix socket path below the OS limit even with a long GOTMPDIR.
	root, err := os.MkdirTemp("/tmp", "cr36-pg-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	data := filepath.Join(root, "data")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run := func(name string, args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		require.NoError(t, err, "%s: %s", name, out)
	}
	run(initdb, "-D", data, "-A", "trust", "-U", "cr36", "--no-locale", "--encoding=UTF8")
	postgresRunning := true
	t.Cleanup(func() {
		if !postgresRunning {
			return
		}
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		out, err := exec.CommandContext(stopCtx, pgctl, "-D", data, "-m", "immediate", "-w", "stop").CombinedOutput()
		require.NoError(t, err, "scratch postgres shutdown: %s", out)
	})
	run(pgctl, "-D", data, "-l", filepath.Join(root, "postgres.log"), "-o", "-F -c listen_addresses='' -k "+root, "-w", "start")
	dsn := "host=" + root + " user=cr36 dbname=postgres sslmode=disable"

	// Apply the actual pre-feature migration set first, write an old entry,
	// then upgrade through the production startup migration runner.
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	source, err := iofs.New(migrationFS, "migrations")
	require.NoError(t, err)
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(12))
	_, err = db.ExecContext(ctx, `INSERT INTO agents (id, public_key, registered_at, last_seen) VALUES ('legacy', decode(repeat('ab',32),'hex'), now(), now());
INSERT INTO inbox_entries (id,agent_id,payload,created_at,expires_at) VALUES ('legacy-message','legacy','{"old":true}',now(),now()+interval '1 hour');`)
	require.NoError(t, err)
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
	store, err := NewPostgresStore(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(store.Close)
	legacy, err := store.Peek("legacy", "legacy-message")
	require.NoError(t, err)
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	var members map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &members))
	for _, key := range []string{"model", "provider", "reply_to"} {
		require.NotContains(t, members, key, "upgrading a legacy row must add no metadata")
	}
	require.JSONEq(t, `{"old":true}`, string(legacy.Payload))
	batch, lease, err := store.Retrieve("legacy", time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	require.NoError(t, store.Ack("legacy", lease, []string{batch[0].ID}))

	factory := func(t *testing.T) *cr36Harness {
		t.Helper()
		_, err := store.pool.Exec(ctx, "TRUNCATE agents CASCADE")
		require.NoError(t, err)
		return newCR36HarnessWithStore(t, nil, store)
	}
	// Reuse the same raw HTTP matrix as memory, including null/blank/missing.
	t.Run("http_poll_matrix", func(t *testing.T) { testDispatchHintsRoundTrip(t, factory) })
	t.Run("remote_delivery_and_nonleasing_reads", func(t *testing.T) {
		h := factory(t)
		h.register(t, cr36Router)
		remote := NewRemoteStore(h.server.URL, "producer", "")
		entry := &InboxEntry{Payload: []byte(`{"task":"dispatch"}`), Model: NewHint(" Model-X "), Provider: NewHint("  "), ReplyTo: "original-id"}
		require.NoError(t, remote.Deliver(cr36Router, entry))
		peeked, err := store.Peek(cr36Router, entry.ID)
		require.NoError(t, err)
		listed, err := store.PeekInbox(cr36Router)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		for _, got := range []*InboxEntry{peeked, listed[0]} {
			require.Equal(t, " Model-X ", got.Model.String())
			require.Equal(t, "  ", got.Provider.String())
			require.True(t, got.Provider.IsDefault())
			require.Equal(t, "original-id", got.ReplyTo)
			require.Empty(t, got.LeaseID, "peek and list never claim work")
		}
		// Long-poll uses the same handler; RemoteStore.Retrieve above covers
		// the ordinary poll in the signed consumer round-trip test.
		status, body := h.send(t, http.MethodGet, "/agents/"+cr36Router+"/inbox?wait_seconds=1", "", nil)
		require.Equal(t, http.StatusOK, status, body)
		var got cr36Inbox
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		require.Len(t, got.Messages, 1)
		require.JSONEq(t, `" Model-X "`, string(got.Messages[0]["model"]))
		require.JSONEq(t, `"  "`, string(got.Messages[0]["provider"]))
		require.JSONEq(t, `"original-id"`, string(got.Messages[0]["reply_to"]))
	})

	t.Run("concurrent_service_idempotency_and_body_conflict", func(t *testing.T) {
		_, err := store.pool.Exec(ctx, "TRUNCATE agents CASCADE")
		require.NoError(t, err)
		for _, id := range []string{"scheduler", "router"} {
			require.NoError(t, store.Register(&Agent{ID: id, PublicKey: HexKey(make([]byte, 32))}))
		}
		const callers = 12
		results := make(chan *ServiceDeliveryReceipt, callers)
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				entry := &InboxEntry{Payload: json.RawMessage(`{"task":1}`), Sender: "scheduler", IdempotencyKey: "concurrent"}
				receipt := ServiceDeliveryReceipt{Status: http.StatusCreated, Response: []byte(`{"transport":"inbox"}`)}
				got, _, err := store.DeliverServiceIdempotent("scheduler", "router", "concurrent", sha256Hex([]byte("same-body")), entry, receipt)
				if err != nil {
					errs <- err
					return
				}
				results <- got
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		var messageID string
		for got := range results {
			require.NotEmpty(t, got.MessageID)
			if messageID == "" {
				messageID = got.MessageID
			}
			require.Equal(t, messageID, got.MessageID)
		}
		var rows int
		require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM inbox_entries WHERE agent_id='router'").Scan(&rows))
		require.Equal(t, 1, rows)
		_, _, err = store.DeliverServiceIdempotent("scheduler", "router", "concurrent", sha256Hex([]byte("changed-body")), &InboxEntry{Payload: json.RawMessage(`{"task":2}`), Sender: "scheduler", IdempotencyKey: "concurrent"}, ServiceDeliveryReceipt{Status: http.StatusCreated, Response: []byte(`{"transport":"inbox"}`)})
		require.ErrorIs(t, err, ErrServiceIdempotencyConflict)
	})

	t.Run("service_receipt_survives_database_and_handler_restart", func(t *testing.T) {
		_, err := store.pool.Exec(ctx, "TRUNCATE agents CASCADE")
		require.NoError(t, err)
		for _, id := range []string{"scheduler", "router"} {
			require.NoError(t, store.Register(&Agent{ID: id, PublicKey: HexKey(make([]byte, 32))}))
		}
		entry := &InboxEntry{ID: "service-message", Payload: json.RawMessage(`{"task":1}`), Sender: "scheduler", IdempotencyKey: "durable-key"}
		receipt := ServiceDeliveryReceipt{Status: http.StatusCreated, Response: []byte("{\"id\":\"service-message\",\"transport\":\"inbox\"}\n")}
		got, replayed, err := store.DeliverServiceIdempotent("scheduler", "router", "durable-key", sha256Hex([]byte("signed-body")), entry, receipt)
		require.NoError(t, err)
		require.False(t, replayed)
		require.Equal(t, receipt.Response, got.Response)

		// The durable replay receipt must outlive normal transport completion.
		// A foreign key from the receipt back to the queue row would make Ack
		// fail here or would delete the receipt that an after-ack retry needs.
		messages, leaseID, err := store.Retrieve("router", time.Minute, 1)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		require.Equal(t, "service-message", messages[0].ID)
		require.NoError(t, store.Ack("router", leaseID, []string{"service-message"}))
		persistedBeforeRestart, err := store.LookupServiceDelivery("scheduler", "router", "durable-key", sha256Hex([]byte("signed-body")))
		require.NoError(t, err)
		require.Equal(t, "service-message", persistedBeforeRestart.MessageID)

		store.Close()
		run(pgctl, "-D", data, "-m", "fast", "-w", "stop")
		postgresRunning = false
		run(pgctl, "-D", data, "-l", filepath.Join(root, "postgres-restart.log"), "-o", "-F -c listen_addresses='' -k "+root, "-w", "start")
		postgresRunning = true

		fresh, err := NewPostgresStore(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(fresh.Close)
		policy, err := ParseServiceDeliveryAllowlist("scheduler>router")
		require.NoError(t, err)
		secured := NewAuthorityStore(fresh, policy, "relay-a")
		freshHandler := NewHandler(secured)
		require.NoError(t, freshHandler.SetServiceDelivery("scheduler>router", "relay-a"))
		authority := &ServiceAuthority{sender: "scheduler", recipient: "router", idempotencyKey: "durable-key", bodyDigest: sha256Hex([]byte("signed-body"))}
		persisted, err := secured.lookupAuthorized(authority)
		require.NoError(t, err)
		require.Equal(t, receipt.Response, persisted.Response)
		retry := &InboxEntry{ID: "must-not-be-stored", Payload: json.RawMessage(`{"task":1}`), Sender: "scheduler", IdempotencyKey: "durable-key"}
		replayedReceipt, replayed, err := fresh.DeliverServiceIdempotent("scheduler", "router", "durable-key", sha256Hex([]byte("signed-body")), retry, receipt)
		require.NoError(t, err)
		require.True(t, replayed)
		require.Equal(t, "service-message", replayedReceipt.MessageID)
		var rows int
		require.NoError(t, fresh.pool.QueryRow(ctx, "SELECT count(*) FROM inbox_entries WHERE agent_id='router'").Scan(&rows))
		require.Zero(t, rows, "ack removed the transport row and replay did not recreate it")
	})
}
