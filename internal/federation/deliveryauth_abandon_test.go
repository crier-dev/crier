package federation_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
)

// DF-CRIER-394: on a downstream panic the buffered receipt is never flushed, so
// the audit line must not describe a classified delivery the client never got.
func TestInboundAuditLineMarksAbandonedReceipt(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	identity := func(next http.Handler) http.Handler { return next }
	panicHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom inside the inbox handler")
	})
	recovery := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recover() != nil {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"INTERNAL"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
	store := func() *federation.PeerStore { return nil }

	h := recovery(federation.DeliveryAuth(identity, store)(panicHandler))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/atlas/inbox", strings.NewReader(`{"message":"x"}`)))

	require.Equal(t, http.StatusInternalServerError, rec.Code, "the recovery middleware answers the client")
	require.Equal(t, string(federation.FedTokenOnly), rec.Header().Get(federation.VerdictHeader),
		"the verdict header still reaches the client")

	line := lastInboundAudit(t, logs.String())
	t.Logf("abandonment audit: %s", strings.TrimSpace(logs.String()))
	require.Equal(t, false, line["flushed"], "an abandoned receipt was not flushed")
	require.NotContains(t, line, "delivered", "abandonment must not use the reserved delivery key")
	require.Equal(t, string(federation.FedTokenOnly), line["verdict"])
	_, hasStatus := line["status"]
	require.False(t, hasStatus, "no status is claimed for a receipt this middleware never sent")
	require.Equal(t, true, line["abandoned"])
	require.NotContains(t, line, "receipt_flushed")

	// Pin the spec's exact JSON field quotation to the emitted audit line.
	const abandonmentFields = `"flushed":false,"abandoned":true`
	require.Contains(t, logs.String(), abandonmentFields)
	spec, err := os.ReadFile("../../specs/CHAT-SESSIONS.md")
	require.NoError(t, err)
	require.Contains(t, string(spec), "JSON fields `"+abandonmentFields+"`")

	// Control: a normal receipt reports flushing + status, not target execution.
	logs.Reset()
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})
	h = recovery(federation.DeliveryAuth(identity, store)(okHandler))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/atlas/inbox", strings.NewReader(`{"message":"x"}`)))
	line = lastInboundAudit(t, logs.String())
	t.Logf("completed audit: %s", strings.TrimSpace(logs.String()))
	delete(line, "time")
	require.Equal(t, map[string]any{
		"level": "INFO", "msg": "federation inbound delivery",
		"peer": "", "path": "/agents/atlas/inbox",
		"verdict": string(federation.FedTokenOnly), "status": float64(http.StatusAccepted),
		"receipt_flushed": true,
	}, line, "the completed audit contract is unchanged")
}

func lastInboundAudit(t *testing.T, raw string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		if l == "" {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &line))
		if line["msg"] == "federation inbound delivery" {
			found = line
		}
	}
	require.NotNil(t, found, "the inbound audit line is emitted")
	return found
}
