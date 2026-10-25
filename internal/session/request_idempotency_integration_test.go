//go:build integration

package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgresMessageRequestIdempotency(t *testing.T) {
	t.Parallel()
	backends := []localBackend{
		{name: "pgxpool", pathFn: func(*testing.T) string { return testDSN }, open: func(t *testing.T, path string) Repository {
			s, err := NewPostgresStore(context.Background(), path)
			require.NoError(t, err)
			return s
		}},
		{name: "postgres_sql", pathFn: func(*testing.T) string { return testDSN }, open: func(t *testing.T, path string) Repository {
			s, err := NewPostgresSQLStore(context.Background(), path)
			require.NoError(t, err)
			// A waiting retry must not consume the winner's only data slot.
			s.DB().SetMaxOpenConns(1)
			return s
		}},
	}
	t.Run("replay", func(t *testing.T) { runMessageRequestIdempotency(t, backends) })
	t.Run("session_scope", func(t *testing.T) { runMessageRequestIdempotencySessionScope(t, backends) })
	t.Run("concurrent", func(t *testing.T) { runMessageRequestIdempotencyConcurrent(t, backends) })
}
