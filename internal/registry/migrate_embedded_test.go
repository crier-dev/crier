package registry

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestEmbeddedMigrationsLoad is the guard for the one migration failure the
// integration-tagged tests cannot catch without a live PostgreSQL: an embedded
// migration SET that golang-migrate refuses to OPEN.
//
// iofs.New is the exact call RunMigrations makes, and it fails on two things
// nothing else in the test suite reads: two migrations carrying the same version
// (measured once on a merge that landed both `007_add_inbox_priority.sql` and
// `007_add_namespaces.sql` — RunMigrations then answered `duplicate migration
// file: 007_add_namespaces.down.sql` and every PostgreSQL deployment failed at
// boot) and a migration file whose version prefix does not parse. Both are
// structural, so this costs milliseconds and needs no database.
func TestEmbeddedMigrationsLoad(t *testing.T) {
	if _, err := iofs.New(migrationFS, "migrations"); err != nil {
		t.Fatalf("the embedded migration set does not load: %v", err)
	}

	ups, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	downs, err := fs.Glob(migrationFS, "migrations/*.down.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no embedded .up.sql migration found: the embedded set is empty")
	}
	if len(ups) != len(downs) {
		t.Fatalf("%d up migration(s) but %d down migration(s): every migration must carry both halves", len(ups), len(downs))
	}
	for i, up := range ups {
		if want := strings.TrimSuffix(up, ".up.sql"); want+".down.sql" != downs[i] {
			t.Fatalf("migration %s has no matching down half (found %s)", up, downs[i])
		}
	}
}
