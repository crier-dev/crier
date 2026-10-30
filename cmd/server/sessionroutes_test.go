package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

func TestGroupStoreForSessionStoreKeepsPostgresInItsSessionBackend(t *testing.T) {
	groupRoot := t.TempDir() + "/must-not-be-created"

	groups, err := groupStoreForSessionStore(&session.PostgresStore{}, groupRoot)
	require.NoError(t, err)
	require.NotNil(t, groups)
	require.Equal(t, "*session.pgxGroupStore", reflect.TypeOf(groups).String())
	require.False(t, fileExists(groupRoot), "selecting PostgreSQL must not create its JSONL root")

	_, err = session.NewJSONLGroupStore(groupRoot)
	require.NoError(t, err, "the control proves the selected PostgreSQL path did not create the JSONL store")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGroupStoreForSessionStorePreservesSQLiteAndJSONLPairings(t *testing.T) {
	jsonl, err := session.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, jsonl.Close()) })

	groups, err := groupStoreForSessionStore(jsonl, t.TempDir())
	require.NoError(t, err)
	require.IsType(t, &session.JSONLGroupStore{}, groups)
}
