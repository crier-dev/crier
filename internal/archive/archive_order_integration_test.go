package archive

// archive_order_integration_test.go — the POSITIVE half of
// specs/DUCKBRAIN-ARCHIVE.md §8 test 2 / R-D2 ("deleted only after a 2xx").
//
// archive_integration_test.go proves the refusal direction (a 500 retains the
// row). Nothing proved the accept direction end-to-end through the store: that
// a past-retention dead letter is POSTed to DuckBrain and that the retention
// DELETE happens only AFTER that POST has been answered 2xx. This test observes
// the ordering from inside the archive request — the fake DuckBrain looks the
// row up in the live store while the POST is in flight, so "the row was still
// there when we archived it" is measured, not inferred from a final count.
//
// Mutation-checked: with the archive callback inverted to run after the delete
// (or with the DELETE hoisted above the callback), row_present_at_archive is
// false and this test fails.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestSuccessfulArchivePostsBeforeRetentionDelete(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte("order-test-key"), 0600))
	store, err := registry.NewSQLiteStore(filepath.Join(t.TempDir(), "registry.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	old := time.Now().Add(-registry.DefaultDeadLetterRetention - time.Hour).UTC()
	dl := &registry.DeadLetter{
		MessageID: "archive-me", AgentID: "agent", Payload: []byte(`{"k":"v"}`),
		CreatedAt: old, ExpiredAt: old, DeadLetteredAt: old, Reason: "expired",
	}
	_, err = store.AppendDeadLetter(dl)
	require.NoError(t, err)

	// The fake DuckBrain answers inside the archive call and records what the
	// store held at that instant.
	var (
		mu            sync.Mutex
		posts         int
		presentAtPost bool
		summary       string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		rows, listErr := store.ListDeadLetters("agent", 10)
		mu.Unlock()
		if listErr != nil {
			summary = "list: " + listErr.Error()
			http.Error(w, "store unavailable", http.StatusInternalServerError)
			return
		}
		presentAtPost = len(rows) == 1 && rows[0].MessageID == "archive-me"
		summary = r.Method + " " + r.URL.Path + " key=" + r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	arch := NewDuckBrainArchiver(Config{
		BaseURL: server.URL, KeyFile: keyFile, Namespace: "crier", MaxBodyBytes: 96000,
	}, server.Client())

	removed := store.PurgeExpiredDeadLetters(func(rec *registry.DeadLetter) bool {
		return ArchiveDeadLetter(context.Background(), arch, rec, 96000)
	})
	require.Equal(t, 1, removed, "an archived dead letter must be removed exactly once")

	mu.Lock()
	gotPosts, gotPresent, gotSummary := posts, presentAtPost, summary
	mu.Unlock()
	require.Equal(t, 1, gotPosts, "exactly one archive POST per dead letter")
	require.True(t, gotPresent,
		"ORDERING: the dead letter must still exist WHILE it is being archived (archive precedes the retention DELETE); request was %q", gotSummary)

	after, err := store.ListDeadLetters("agent", 10)
	require.NoError(t, err)
	require.Empty(t, after, "after a 2xx archive the row is deleted")

	// The outcome is counted, so a silent success cannot be told from a run
	// that never archived.
	w := httptest.NewRecorder()
	metrics.Default.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.GreaterOrEqual(t, metricValue(t, w.Body.String(), `crier_archive_attempts_total{outcome="archived"}`), 1.0)
}

// metricValue parses one gauge/counter sample out of a Prometheus text body.
func metricValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, series+" ")), 64)
		require.NoError(t, err)
		return f
	}
	t.Fatalf("metric series %s not present in /metrics output", series)
	return 0
}
