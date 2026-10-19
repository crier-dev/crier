package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

func TestSessionFanoutNamespaceDeliveryAudit(t *testing.T) {
	policies, err := namespace.Parse(`{"namespaces":[{"name":"team"}]}`, nil)
	require.NoError(t, err)
	store := registry.NewMemoryStore()
	require.NoError(t, store.Register(&registry.Agent{ID: "scoped", Namespace: "team"}))
	require.NoError(t, store.Register(&registry.Agent{ID: "default"}))
	h := registry.NewHandler(store)
	h.SetNamespacePolicies(policies)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	msg := &Message{ID: "audit-message", SessionID: "room", ThreadID: "thread", Author: AuthorRef{Agent: "speaker"}, Kind: MessagePlain, Payload: json.RawMessage(`{"text":"hello"}`)}
	outcomes, err := Fanout(context.Background(), registry.NewSessionDeliverer(h), msg, []string{"scoped", "default"}, nil)
	require.NoError(t, err)
	require.Len(t, outcomes, 2)
	for _, outcome := range outcomes {
		require.Equal(t, OutcomeAccepted, outcome.Outcome)
	}
	var audits []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for scanner.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		if line["msg"] == "inbox deliver scoped to namespace" {
			audits = append(audits, line)
		}
	}
	require.NoError(t, scanner.Err())
	require.Len(t, audits, 1, "one scoped delivery, no audit for the default realm")
	require.Equal(t, "DEBUG", audits[0]["level"])
	require.Equal(t, "scoped", audits[0]["target"])
	require.Equal(t, "team", audits[0]["namespace"])
	require.Equal(t, "speaker", audits[0]["sender"])
	require.Len(t, audits[0], 6, "same time/level/msg + target/namespace/sender vocabulary as HTTP")
	entries, _, err := store.Retrieve("scoped", 0, 1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "team", entries[0].Namespace)
}
