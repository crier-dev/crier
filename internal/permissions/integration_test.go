//go:build integration

// The integration battery runs against a REAL PostgreSQL, because the claims it
// checks are SQL-level claims an in-memory fake cannot make: that the ACL's
// tables exist under their fixed names, that keep-LAST is enforced by the
// (id, seq) append-only key, and that the PostgreSQL store and the JSONL log
// reduce to the SAME effective state (the dual-store acceptance of
// CR-CHAT-003).
//
// The build tag matches the repo's convention (internal/session's and
// internal/registry's Postgres tests): `make test` and the commit guard stay
// database-free and fast, and the battery runs with
//
//	go test -tags=integration -count=1 -timeout 5m ./internal/permissions
package permissions

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		pgUser = "test"
		pgPass = "test"
		pgDB   = "test"
	)
	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase(pgDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPass),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container start: %v\n", err)
		os.Exit(1)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres host: %v\n", err)
		_ = ctr.Terminate(context.Background())
		os.Exit(1)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres mapped port: %v\n", err)
		_ = ctr.Terminate(context.Background())
		os.Exit(1)
	}
	testDSN = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", pgUser, pgPass, host, port.Port(), pgDB)

	code := m.Run()

	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer termCancel()
	if err := ctr.Terminate(termCtx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres terminate: %v\n", err)
	}
	os.Exit(code)
}

// newTestPostgresStore returns a store over a schema wiped clean, so each test
// starts from nothing.
func newTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, testDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, err = store.pool.Exec(ctx, `DROP TABLE IF EXISTS
		permission_agents, permission_grants, permission_bindings, permission_principals CASCADE`)
	require.NoError(t, err)
	require.NoError(t, store.ApplySchema(ctx))
	return store
}

// TestPostgres_SchemaShapeIsTheSpecShape pins the table and column names, so
// that the view and a client cannot invent two schemas and the CHECK
// vocabularies cannot drift from the Go layer's closed sets.
func TestPostgres_SchemaShapeIsTheSpecShape(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()

	tables := []string{"permission_principals", "permission_bindings", "permission_grants", "permission_agents"}
	for _, tbl := range tables {
		var exists bool
		require.NoError(t, store.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).Scan(&exists))
		require.Truef(t, exists, "table %s must exist", tbl)
	}

	// The append-only key (id, seq) is what makes keep-LAST and the tombstone
	// real on this backend.
	for _, tbl := range tables {
		var cols int
		require.NoError(t, store.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name IN ('id','seq')`, tbl).Scan(&cols))
		require.Equal(t, 2, cols, "%s must key on (id, seq)", tbl)
	}

	// A hand-written row outside the closed vocabulary is refused by the DB.
	_, err := store.pool.Exec(ctx, `INSERT INTO permission_agents (id, ts, class) VALUES ('x', now(), 'robot')`)
	require.Error(t, err, "the class CHECK constraint must refuse an unknown class")
	_, err = store.pool.Exec(ctx, `INSERT INTO permission_grants
		(id, ts, principal, subject_type, subject_ref, actions) VALUES ('g','now', 'p','planet','x',ARRAY['send'])`)
	require.Error(t, err, "the subject_type CHECK constraint must refuse an unknown subject")
}

// TestPostgres_RoundTrip writes one record of each kind and reads the folded
// snapshot back.
func TestPostgres_RoundTrip(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()

	require.NoError(t, store.Append(ctx, activePrincipal("prin_bane", "owner", "acme").Record(at(0))))
	require.NoError(t, store.Append(ctx, (&Binding{ID: "bind_1", Principal: "prin_bane", Agent: "atlas", AsAgent: true, CreatedAt: at(1)}).Record(at(1))))
	require.NoError(t, store.Append(ctx, (&Grant{
		ID: "grant_1", Principal: "prin_bane", Subject: agentAddr("atlas"),
		Actions: []Action{ActionSend, ActionRead}, GrantedBy: "prin_bane", GrantedAt: at(2),
	}).Record(at(2))))
	require.NoError(t, store.Append(ctx, (&AgentInfo{
		ID: "atlas", Class: ClassPersonal, Owner: "prin_bane", Namespace: "acme",
		Capabilities: []string{"planning"},
		Scopes:       []Scope{{ID: "s1", Reach: Reach{Principals: []string{"prin_bane"}}}},
	}).Record(at(3))))

	sn, err := store.Snapshot(ctx)
	require.NoError(t, err)

	require.Equal(t, RoleOwner, sn.Principal("prin_bane").Role)
	require.Equal(t, PrincipalActive, sn.Principal("prin_bane").Status)
	require.True(t, sn.Binding("prin_bane", "atlas").AsAgent)

	g := sn.LiveGrantFor("prin_bane", agentAddr("atlas"), ActionSend, at(4))
	require.NotNil(t, g)
	require.Equal(t, []Action{ActionSend, ActionRead}, g.Actions)

	a := sn.Agent("atlas")
	require.Equal(t, ClassPersonal, a.Class)
	require.Equal(t, []string{"planning"}, a.Capabilities)
	require.Len(t, a.Scopes, 1)
	require.True(t, a.reachableByPrincipal("prin_bane"))
}

// TestPostgres_KeepLastAndTombstone proves the append-only key gives keep-LAST
// and that a tombstone is retained rather than overwriting its predecessor.
func TestPostgres_KeepLastAndTombstone(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()

	require.NoError(t, store.Append(ctx, (&Grant{
		ID: "g", Principal: "prin_ana", Subject: agentAddr("quill"),
		Actions: []Action{ActionSend}, GrantedBy: "o", GrantedAt: at(0),
	}).Record(at(0))))

	sn, err := store.Snapshot(ctx)
	require.NoError(t, err)
	require.NotNil(t, sn.LiveGrantFor("prin_ana", agentAddr("quill"), ActionSend, at(1)))

	require.NoError(t, RevokeGrant(ctx, store, "g", "prin_owner", at(2)))

	sn, err = store.Snapshot(ctx)
	require.NoError(t, err)
	require.Nil(t, sn.LiveGrantFor("prin_ana", agentAddr("quill"), ActionSend, at(3)),
		"the tombstone is the latest version, so the grant is absent")

	var versions int
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM permission_grants WHERE id = 'g'`).Scan(&versions))
	require.Equal(t, 2, versions, "the original version is retained")
}

// TestPostgresJSONLParity proves the two backends reduce the SAME identical
// scenario to the same effective state — CR-CHAT-003's dual-store acceptance.
func TestPostgresJSONLParity(t *testing.T) {
	ctx := context.Background()
	pg := newTestPostgresStore(t)
	js, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	scenario := []*Record{
		activePrincipal("prin_bane", "owner", "acme").Record(at(0)),
		activePrincipal("prin_viewer", "viewer", "acme").Record(at(0)),
		(&Binding{ID: "b1", Principal: "prin_bane", Agent: "atlas", AsAgent: true, CreatedAt: at(1)}).Record(at(1)),
		(&Binding{ID: "b2", Principal: "prin_viewer", Agent: "quill", AsAgent: true, CreatedAt: at(1)}).Record(at(1)),
		(&Grant{ID: "g1", Principal: "prin_bane", Subject: agentAddr("atlas"), Actions: []Action{ActionSend, ActionRead}, GrantedBy: "prin_bane", GrantedAt: at(2)}).Record(at(2)),
		(&Grant{ID: "g2", Principal: "prin_viewer", Subject: agentAddr("quill"), Actions: []Action{ActionRead}, GrantedBy: "prin_bane", GrantedAt: at(2)}).Record(at(2)),
		(&Grant{ID: "g3", Principal: "prin_viewer", Subject: agentAddr("quill"), Actions: []Action{ActionSend}, GrantedBy: "prin_bane", GrantedAt: at(2)}).Record(at(2)),
		(&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_bane", Namespace: "acme", Capabilities: []string{"planning"}}).Record(at(3)),
		(&AgentInfo{ID: "deploy-bot", Class: ClassService, Namespace: "acme", Scopes: []Scope{{ID: "s", Reach: Reach{Principals: []string{"prin_bane"}}}}}).Record(at(3)),
	}
	for _, rec := range scenario {
		require.NoError(t, pg.Append(ctx, rec))
		require.NoError(t, js.Append(ctx, rec))
	}
	// Revoke g3 on BOTH, so the tombstone path is compared too.
	require.NoError(t, RevokeGrant(ctx, pg, "g3", "prin_bane", at(10)))
	require.NoError(t, RevokeGrant(ctx, js, "g3", "prin_bane", at(10)))

	pgs, err := pg.Snapshot(ctx)
	require.NoError(t, err)
	jss, err := js.Snapshot(ctx)
	require.NoError(t, err)

	require.Equal(t, jss.Principal("prin_bane"), pgs.Principal("prin_bane"))
	require.Equal(t, jss.Principal("prin_viewer"), pgs.Principal("prin_viewer"))
	require.Equal(t, jss.Binding("prin_bane", "atlas"), pgs.Binding("prin_bane", "atlas"))
	require.Equal(t, jss.Agent("atlas"), pgs.Agent("atlas"))
	require.Equal(t, jss.Agent("deploy-bot"), pgs.Agent("deploy-bot"))
	require.Equal(t, jss.Grants(), pgs.Grants())

	// And the ACL itself agrees on both.
	pc, jc := NewChecker(pg), NewChecker(js)
	pc.now = func() time.Time { return at(100) }
	jc.now = func() time.Time { return at(100) }
	for _, in := range []CheckInput{
		{Sender: humanSender("prin_viewer", "quill"), Action: ActionSend, Subject: agentAddr("quill")},
		{Sender: humanSender("prin_viewer", "quill"), Action: ActionRead, Subject: agentAddr("quill")},
		{Sender: EffectiveSender{Kind: SenderAnonymous}, Action: ActionSend, Subject: agentAddr("atlas")},
		{Sender: humanSender("prin_bane", "atlas"), Action: ActionSend, Subject: agentAddr("atlas")},
	} {
		pres, err := pc.MayDeliver(ctx, in)
		require.NoError(t, err)
		jres, err := jc.MayDeliver(ctx, in)
		require.NoError(t, err)
		require.Equal(t, jres, pres, "both backends must decide %+v identically", in)
	}
}
