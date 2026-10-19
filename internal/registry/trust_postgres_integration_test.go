//go:build integration

package registry

import (
	"context"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"
)

func TestPostgresTrustStore(t *testing.T) {
	s := newTestStore(t)
	exerciseTrustStore(t, NewAuthorityStore(s, nil, ""))
	s.Close()
	reopened, err := NewPostgresStore(context.Background(), testConnString)
	require.NoError(t, err)
	defer reopened.Close()
	anchors, err := reopened.ListTrustAnchors()
	require.NoError(t, err)
	require.Len(t, anchors, 1)
	_, d := registryTrustFixture()
	chain, err := reopened.ListTrustDelegations(d.SubjectKey)
	require.NoError(t, err)
	require.Equal(t, []trust.ChainDelegation{d}, chain)
	require.Equal(t, trust.TrustVerified, trust.VerifyChain(anchors, d.SubjectKey, d.Scope, chain).Verdict)
	// In particular issued_at still has nanoseconds after a pool close/reopen.
	require.Equal(t, 123456789, chain[0].IssuedAt.Nanosecond())
}

func TestMigration017TrustRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)
	require.NoError(t, RunMigrations(ctx, testConnString))
	source, err := iofs.New(migrationFS, "migrations")
	require.NoError(t, err)
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	require.NoError(t, err)
	defer m.Close()
	for _, version := range []uint{16, 17} {
		require.NoError(t, m.Migrate(version))
		for _, table := range []string{"trust_anchors", "trust_delegations"} {
			var exists bool
			require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
			require.Equal(t, version == 17, exists, table)
		}
	}
}
