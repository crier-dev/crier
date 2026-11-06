package ingress

// route_sql_test.go — the ingress-side arms of CRIER-INGRESS §3 for the SQL
// route store: an inbox push on a SQL backend leaves a queryable
// `ingress.route` row, and a route write that FAILS is reported in the response
// instead of being swallowed. The JSONL arm is the pre-existing
// TestIngressInboxModeCallsTheInboxPath, whose expectations this row does not
// change.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// newSQLiteTestEnv is newTestEnv with the SQLite session store and the SQL
// route store wired — the pair cmd/server selects when CR_SESSION_BACKEND=
// sqlite. The store is returned so a test can read the row back from the
// database rather than from the handler.
func newSQLiteTestEnv(t *testing.T) (*testEnv, *session.SQLStore) {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewSQLiteStore(filepath.Join(root, "sessions.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	routes, err := NewSQLRouteStore(store)
	if err != nil {
		t.Fatalf("open sql route store: %v", err)
	}
	now := time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC)
	h := &Handler{
		Repo:  store,
		Route: routes,
		Idem:  NewMemoryIdempotencyStore(),
		Now:   func() time.Time { return now },
	}
	return &testEnv{handler: h, repo: store, root: root, now: now}, store
}

// resolvedInbox is the production adapter's shape for a CAPABILITY push: the
// registry selects a holder and the delivery outcome names it, which is what
// gives the route row its resolved agent id.
type resolvedInbox struct{ entryID, target string }

func (r *resolvedInbox) Deliver(_ context.Context, agentID, capability string, _ InboxMessage) ([]session.DeliveryOutcome, error) {
	target := r.target
	if target == "" {
		target = agentID
	}
	return []session.DeliveryOutcome{{
		Target: target, Outcome: session.OutcomeAccepted, InboxEntryID: r.entryID, UpdatedAt: time.Now().UTC(),
	}}, nil
}

func TestIngressInboxModeWritesTheRouteRowToSQL(t *testing.T) {
	ctx := context.Background()
	env, store := newSQLiteTestEnv(t)
	env.handler.Inbox = &resolvedInbox{entryID: "e_sql1", target: "bravo"}

	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"capability": "build"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "build 4412 failed on main",
		"tags":   []string{"build"},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	// The row the SQL backend holds, read as columns: this is §3's "where did
	// this push actually land", answerable without replaying the JSONL log.
	var (
		kind, agentID, capability, messageID string
		authorType, authorID, createdAt      string
		tags                                 *string
		routeID                              string
	)
	if err := store.DB().QueryRowContext(ctx, `
		SELECT kind, route_id, agent_id, capability, message_id,
		       author_type, author_id, tags, created_at
		FROM chat_ingress_routes`).
		Scan(&kind, &routeID, &agentID, &capability, &messageID,
			&authorType, &authorID, &tags, &createdAt); err != nil {
		t.Fatalf("read ingress route row: %v", err)
	}
	if kind != "ingress.route" {
		t.Fatalf("kind = %q, want ingress.route", kind)
	}
	if !strings.HasPrefix(routeID, "r_") {
		t.Fatalf("route_id = %q, want the handler-minted r_ id", routeID)
	}
	if agentID != "bravo" {
		t.Fatalf("agent_id = %q, want the capability-selected holder the delivery resolved", agentID)
	}
	if capability != "build" {
		t.Fatalf("capability = %q, want build", capability)
	}
	if messageID != "e_sql1" {
		t.Fatalf("message_id = %q, want the inbox entry id", messageID)
	}
	if authorType != "agent" || authorID != "ci-bot" {
		t.Fatalf("author = %q/%q, want agent/ci-bot", authorType, authorID)
	}
	if tags == nil || *tags != `["build"]` {
		t.Fatalf("tags = %v, want the stored [\"build\"]", tags)
	}
	if createdAt != env.now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("created_at = %q, want the handler clock %q", createdAt, env.now.UTC().Format(time.RFC3339Nano))
	}

	// Selection happened at boot, so NOTHING went to the JSONL route log: no
	// ingress directory is created and no second copy of the fact exists.
	if _, err := os.Stat(filepath.Join(env.root, "ingress")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("JSONL route log touched on a SQL backend (stat err = %v)", err)
	}
}

// failingRouteStore is a RouteStore that always fails, standing in for the
// disk/connection failure the receipt path must REPORT (§3).
type failingRouteStore struct{ err error }

func (f failingRouteStore) RecordRoute(context.Context, RouteRecord) error { return f.err }

func TestIngressRouteFailureIsReportedNotSwallowed(t *testing.T) {
	env := newTestEnv(t)
	env.handler.Inbox = &recordingInbox{entryID: "e_flaky"}
	env.handler.Route = failingRouteStore{err: errors.New("disk full")}

	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "alpha"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a push whose receipt failed is not a success (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeStoreError {
		t.Fatalf("code = %q, want %q", code, CodeStoreError)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "e_flaky") {
		t.Fatalf("body %s must name the inbox entry id that WAS stored", body)
	}
	if !strings.Contains(body, "disk full") {
		t.Fatalf("body %s must carry the route store's failure", body)
	}
}

// TestIngressSQLRouteStoreFailureIsReported is the same acceptance on the SQL
// path: the store's own write error reaches the response. A closed store is a
// real "cannot write" state (a database handle lost after boot), and it makes
// the error come from the SQL store rather than from a test double.
func TestIngressSQLRouteStoreFailureIsReported(t *testing.T) {
	env, store := newSQLiteTestEnv(t)
	env.handler.Inbox = &recordingInbox{entryID: "e_closed"}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "alpha"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeStoreError {
		t.Fatalf("code = %q, want %q", code, CodeStoreError)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "e_closed") || !strings.Contains(body, "ingress.route record failed") {
		t.Fatalf("body %s must name the entry id AND state that the route record failed", body)
	}
}
