package archive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestFailedPostRetainsSQLiteDeadLetter(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key"), 0600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()
	store, err := registry.NewSQLiteStore(filepath.Join(t.TempDir(), "registry.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	old := time.Now().Add(-registry.DefaultDeadLetterRetention - time.Hour).UTC()
	dl := &registry.DeadLetter{MessageID: "retain-me", AgentID: "agent", Payload: []byte("payload"), CreatedAt: old, ExpiredAt: old, DeadLetteredAt: old, Reason: "expired"}
	_, err = store.AppendDeadLetter(dl)
	require.NoError(t, err)
	arch := NewDuckBrainArchiver(Config{BaseURL: server.URL, KeyFile: keyFile, Namespace: "crier", MaxBodyBytes: 96000}, server.Client())
	removed := store.PurgeExpiredDeadLetters(func(rec *registry.DeadLetter) bool { return ArchiveDeadLetter(context.Background(), arch, rec, 96000) })
	require.Zero(t, removed)
	rows, err := store.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	w := httptest.NewRecorder()
	metrics.Default.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Contains(t, w.Body.String(), `crier_archive_attempts_total{outcome="failed"} 1`)
}
