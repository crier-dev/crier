//go:build integration

package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgresTaskRequestIdempotency(t *testing.T) {
	backends := []localBackend{
		{name: "pgxpool", pathFn: func(*testing.T) string { return testDSN }, open: func(t *testing.T, path string) Repository {
			s, err := NewPostgresStore(context.Background(), path)
			require.NoError(t, err)
			return s
		}},
		{name: "postgres_sql", pathFn: func(*testing.T) string { return testDSN }, open: func(t *testing.T, path string) Repository {
			s, err := NewPostgresSQLStore(context.Background(), path)
			require.NoError(t, err)
			s.DB().SetMaxOpenConns(1)
			return s
		}},
	}
	t.Run("replay", func(t *testing.T) { runTaskRequestIdempotency(t, backends) })
	t.Run("concurrent", func(t *testing.T) { runTaskRequestIdempotencyConcurrent(t, backends) })
	t.Run("shared_lock", func(t *testing.T) { runTaskRequestSharedLock(t, backends) })
}
