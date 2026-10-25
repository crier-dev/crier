package registry

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// Execute, rather than grep, the same selected DDL startup uses. SQLite accepts
// arbitrary type names, so separate constraint tests also check their semantics.
func checkSQLiteMigrationChain(t *testing.T, source fs.FS) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "parse.db"))
	if err != nil {
		return err
	}
	defer func() { require.NoError(t, db.Close()) }()
	files, err := fs.Glob(source, "migrations/*.up.sql")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("empty migration chain")
	}
	for _, name := range files {
		version, err := strconv.Atoi(strings.Split(filepath.Base(name), "_")[0])
		if err != nil {
			return err
		}
		raw, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(t.Context(), sqliteMigrationDDL(version, string(raw))); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		t.Logf("SQLite DDL accepted: %s", name)
	}
	return nil
}

func TestSQLiteMigrationChainSyntax(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkSQLiteMigrationChain(t, migrationFS))
}

func TestSQLiteMigrationChainSyntaxRejectsBrokenFixture(t *testing.T) {
	t.Parallel()
	files, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	require.NoError(t, err)
	fixture := fstest.MapFS{}
	maxVersion := 0
	for _, name := range files {
		raw, err := migrationFS.ReadFile(name)
		require.NoError(t, err)
		fixture[name] = &fstest.MapFile{Data: raw}
		v, err := strconv.Atoi(strings.Split(filepath.Base(name), "_")[0])
		require.NoError(t, err)
		if v > maxVersion {
			maxVersion = v
		}
	}
	name := fmt.Sprintf("migrations/%03d_broken_fixture.up.sql", maxVersion+1)
	fixture[name] = &fstest.MapFile{Data: []byte(`CREATE TABLE portability_poison (key TEXT CHECK (key ~ '^[a-f]+$'));`)}
	err = checkSQLiteMigrationChain(t, fixture)
	require.ErrorContains(t, err, name) // Not an earlier real migration failing.
	require.ErrorContains(t, err, `near "~": syntax error`)
	t.Logf("guard rejected deliberately broken next migration: %v", err)
	fixture[name].Data = []byte(`CREATE TABLE portability_poison (key TEXT);`)
	require.NoError(t, checkSQLiteMigrationChain(t, fixture)) // Positive control.
}

func assertSQLiteMigrationVersions(t *testing.T, s *SQLiteStore) {
	t.Helper()
	files, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM registry_schema_migrations`).Scan(&count))
	require.Equal(t, len(files), count, "no migration may be skipped or invented")
	for _, name := range files {
		v, err := strconv.Atoi(strings.Split(filepath.Base(name), "_")[0])
		require.NoError(t, err)
		require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM registry_schema_migrations WHERE version=?`, v).Scan(&count))
		require.Equal(t, 1, count, name)
	}
}

func TestSQLiteFullChainRegisterReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.db")
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	assertSQLiteMigrationVersions(t, s)
	require.NoError(t, s.Register(&Agent{ID: "migration-durable", Capabilities: []string{"worker"}}))
	before, err := s.Get("migration-durable")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	assertSQLiteMigrationVersions(t, s)
	after, err := s.Get("migration-durable")
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func sqliteMigrationFixture(t *testing.T, through int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE registry_schema_migrations(version INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	files, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	require.NoError(t, err)
	for _, name := range files {
		v, err := strconv.Atoi(strings.Split(filepath.Base(name), "_")[0])
		require.NoError(t, err)
		if v > through {
			continue
		}
		raw, err := migrationFS.ReadFile(name)
		require.NoError(t, err)
		_, err = db.Exec(sqliteMigrationDDL(v, string(raw)))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO registry_schema_migrations VALUES(?)`, v)
		require.NoError(t, err)
	}
	return path, db
}

// Freeze the actual pre-trust schema at version 16, not latest-1: a new
// migration must never silently move this fixture's historical boundary.
func TestSQLiteMigration017UpgradePreservesAgent(t *testing.T) {
	t.Parallel()
	path, db := sqliteMigrationFixture(t, 16)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='trust_anchors'`).Scan(&n))
	require.Zero(t, n, "fixture must precede trust tables")
	_, err := db.Exec(`INSERT INTO agents(id,registered_at,last_seen) VALUES('pre-trust',1,1)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	s, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	assertSQLiteMigrationVersions(t, s)
	agent, err := s.Get("pre-trust")
	require.NoError(t, err)
	require.Equal(t, "pre-trust", agent.ID)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM trust_anchors`).Scan(&n))
	require.Zero(t, n)
}

func TestSQLiteMigration017Constraints(t *testing.T) {
	t.Parallel()
	s := newTestSQLiteStore(t)
	key, sig := strings.Repeat("a1", 32), strings.Repeat("b2", 64)
	for _, bad := range []string{"", "abc", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("A", 64), key + "\x00junk", "a\x00" + strings.Repeat("g", 62)} {
		_, err := s.db.Exec(`INSERT INTO trust_anchors VALUES('bad',?,'label',1)`, bad)
		require.Error(t, err, "invalid public_key %q", bad)
		_, err = s.db.Exec(`INSERT INTO trust_delegations VALUES(?,?,'{}')`, sig, bad)
		require.Error(t, err, "invalid subject_key %q", bad)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("b", 127), strings.Repeat("b", 129), strings.Repeat("G", 128), strings.Repeat("B", 128), sig + "\x00junk", "b\x00" + strings.Repeat("g", 126)} {
		_, err := s.db.Exec(`INSERT INTO trust_delegations VALUES(?,?,'{}')`, bad, key)
		require.Error(t, err, "invalid signature %q", bad)
	}
	for _, bad := range []string{"", "{", "[]", "null", `"text"`, "1", "true"} {
		_, err := s.db.Exec(`INSERT INTO trust_delegations VALUES(?,?,?)`, sig, key, bad)
		require.Error(t, err, "invalid object record %q", bad)
	}
	for _, query := range []string{
		`INSERT INTO trust_anchors VALUES(NULL,'` + key + `','label',1)`,
		`INSERT INTO trust_anchors VALUES(' ','` + key + `','label',1)`,
		`INSERT INTO trust_anchors VALUES('id','` + key + `',' ',1)`,
		`INSERT INTO trust_delegations VALUES(NULL,'` + key + `','{}')`,
		`INSERT INTO trust_delegations VALUES('` + sig + `','` + key + `',NULL)`,
	} {
		_, err := s.db.Exec(query)
		require.Error(t, err, query)
	}
	_, err := s.db.Exec(`INSERT INTO trust_anchors VALUES('valid',?,'label',1)`, key)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO trust_anchors VALUES('duplicate',?,'label',1)`, key)
	require.Error(t, err, "public_key remains unique")
	_, err = s.db.Exec(`INSERT INTO trust_delegations VALUES(?,?,?)`, sig, key, `{"issued_at":"2026-10-19T12:34:56.123456789Z"}`)
	require.NoError(t, err)
	var record string
	require.NoError(t, s.db.QueryRow(`SELECT record FROM trust_delegations WHERE signature=?`, sig).Scan(&record))
	require.Equal(t, `{"issued_at":"2026-10-19T12:34:56.123456789Z"}`, record)
}
