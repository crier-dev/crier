package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

func TestA2AReplayLabelsAllResponseShapes(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, blocking := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/blocking=%t", streaming, blocking), func(t *testing.T) {
				store := registry.NewAuthorityStore(registry.NewMemoryStore(), nil, "")
				registerStoreAgent(t, store, "labels", true)
				calls := 0
				deliver := func(w http.ResponseWriter, _ *http.Request) {
					calls++
					reply := ""
					if blocking {
						reply = `,"reply":{"answer":"original answer"}`
					}
					// A pre-existing delivery cache marker must not be mistaken for
					// acquisition of an A2A replay reservation.
					_, _ = fmt.Fprintf(w, `{"id":"only-task","transport":"webhook","idempotent_replay":true%s}`, reply)
				}
				router := mux.NewRouter()
				registerA2ARoute(router, store, deliver, nil, nil, a2aOptions{replayWindow: 17 * time.Second, streamBudget: time.Millisecond})
				server := httptest.NewServer(router)
				defer server.Close()
				method, field := a2a.MethodSendMessage, "task"
				if streaming {
					method = a2a.MethodSendStreamingMessage
				}
				if blocking {
					field = "message"
				}
				for _, replay := range []string{"new", "replayed"} {
					var result map[string]any
					if streaming {
						stream := openSSE(t, server.URL, sendMessageBody("labels", method, "same"))
						result = stream.next(t, "opening response")
						require.NoError(t, stream.body.Body.Close())
						stream.waitClosed(t)
					} else {
						_, raw, _ := a2aRPC(t, server.Client(), server.URL, sendMessageBody("labels", method, "same"))
						result = rpcResult(t, raw)
					}
					object, ok := result[field].(map[string]any)
					require.True(t, ok, "%v", result)
					assertReplayLabel(t, object, "process", replay, 17)
				}
				require.Equal(t, 1, calls, "labels must not change the replay decision")
			})
		}
	}
}

func TestA2AReplayDeploymentInfoUsesStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store registry.Store
		want  string
	}{
		{"memory", registry.NewMemoryStore(), "process"},
		{"opaque", struct{ registry.Store }{registry.NewMemoryStore()}, "process"},
		{"sqlite", &registry.SQLiteStore{}, "durable"},
		{"postgres", &registry.PostgresStore{}, "durable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// These are read-only storage capability checks, not database I/O.
			for _, store := range []registry.Store{tc.store, registry.NewAuthorityStore(tc.store, nil, ""), registry.NewAuthorityStore(registry.NewAuthorityStore(tc.store, nil, ""), nil, "")} {
				for _, window := range []time.Duration{0, -time.Second, time.Second} {
					info := a2aIdempotencyInfo(store, window, true)
					require.Equal(t, tc.want, info.Durability)
					seconds := registry.DefaultIdempotencyWindow.Seconds()
					if window > 0 {
						seconds = window.Seconds()
					}
					require.Equal(t, seconds, info.WindowSeconds)
					require.Equal(t, "replayed", info.Replay)
				}
			}
		})
	}
}

func assertReplayCard(t *testing.T, base, durability string, seconds float64) {
	t.Helper()
	resp, err := http.Get(base + a2a.AgentCardPath + "?agent_id=restart-a2a")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	var card map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
	description, ok := card["description"].(string)
	require.True(t, ok)
	require.Contains(t, description, fmt.Sprintf("window is %g seconds", seconds))
	if durability == "process" {
		require.Contains(t, description, "life of the process only")
		require.Contains(t, description, "restart may mint a second task for the same messageId")
	} else {
		require.Contains(t, description, "survives process restart while the registry replay store is retained")
	}
}
