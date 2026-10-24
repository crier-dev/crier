package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// BenchmarkNewSQLiteStore measures what ONE test pays to stand up a fresh
// SQLite-backed view: open, arm the connection pragmas and apply the schema.
// It is the number the parallel-suite timeout (INT-CI-014) hangs on — the
// package runs 200+ tests, each opening its own store, so a millisecond here
// is a quarter-second of package wall time.
func BenchmarkNewSQLiteStore(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(b.TempDir(), "sessions.sqlite")
		store, err := NewSQLiteStore(path)
		if err != nil {
			b.Fatalf("NewSQLiteStore: %v", err)
		}
		if err := store.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkSQLiteOpenOnly is the control for BenchmarkNewSQLiteStore: the same
// open and the same connection pragmas, with NO schema. The difference between
// the two is what applying the view costs, which is what INT-CI-014 measures.
func BenchmarkSQLiteOpenOnly(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(b.TempDir(), "sessions.sqlite")
		db, err := sql.Open(sqliteDriverName, path)
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		for _, stmt := range (sqliteDialect{}).InitStatements() {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				b.Fatalf("init %q: %v", stmt, err)
			}
		}
		if err := db.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}
