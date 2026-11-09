// falsifier_test.go — the INT-015 gate (CRIER-GITHOOK-DISPATCH §10, brief
// falsifier). This file MUST NOT reference symbols defined in sweep.go: the
// pre-change proof stashes that file and runs this test against the remaining
// tree. Pre-change, SweepFunc is nil (no sweep is linked in) and this test
// times out waiting for a delivery that can never happen; post-change the
// sweep delivers the row within one sweep interval. That flip — fail pre,
// pass post — is the row's acceptance evidence.
package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFalsifierHooksNeverFireRowStillDelivered writes a board row DIRECTLY
// into the board file (the fast-forward-pull case: no commit for a post-commit
// hook to see, no ingest POST, hooks never fire) and requires the row to reach
// the registered agent within ONE sweep interval.
func TestFalsifierHooksNeverFireRowStillDelivered(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	inbox := &captureInbox{store: store}
	h := &Handler{Store: store, Inbox: inbox, Now: func() time.Time { return time.Unix(1762627200, 0).UTC() }}
	src := Source{ID: "boardctl", Secret: []byte("falsifier-secret"), Mapping: "passthrough", DeliverTo: "board-agent", Tags: []string{"webhook", "src:boardctl"}, CreatedAt: h.now()}
	if err := store.CreateSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}

	// The hook-less write: the row lands on the board from "anywhere" (a
	// script, a fast-forward pull). No HTTP, no hook.
	boardDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(boardDir, ".coding-hermes", "board"), 0700); err != nil {
		t.Fatal(err)
	}
	rowLine, err := json.Marshal(map[string]any{"id": "INT-015-FAL", "title": "delivered with hooks never firing", "status": "pending", "priority": "P1"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(boardDir, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.WriteFile(path, append(rowLine, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	// One sweep interval, scaled down for the test: the sweep implementation
	// is driven on a short cadence through the same seam the server loop uses.
	// Pre-change the seam is nil and NOTHING can deliver the row — the test
	// burns the interval and fails.
	interval := 25 * time.Millisecond
	deadline := time.Now().Add(2 * time.Second)
	sweepCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if SweepFunc != nil {
			if _, err := SweepFunc(sweepCtx, h, boardDir, "boardctl"); err != nil {
				t.Fatalf("sweep pass: %v", err)
			}
		}
		if len(inbox.messages) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hooks never fired and the row was NOT delivered within one sweep interval (%v): SweepFunc armed=%v — the durable path is missing", interval, SweepFunc != nil)
		}
		<-ticker.C
	}
	// The delivery is the boardctl event for the right row, to the right agent.
	if len(inbox.agents) != 1 || inbox.agents[0] != "board-agent" {
		t.Fatalf("agents=%v, want exactly [board-agent]", inbox.agents)
	}
	var payload struct {
		ID           string `json:"id"`
		Event        string `json:"event"`
		DiscoveredBy string `json:"discovered_by"`
	}
	if err := json.Unmarshal(inbox.messages[0].Payload, &payload); err != nil {
		t.Fatalf("delivered payload %s: %v", inbox.messages[0].Payload, err)
	}
	if payload.ID != "INT-015-FAL" || payload.Event != "row_added" || !strings.Contains(payload.DiscoveredBy, "reconciliation-sweep") {
		t.Fatalf("delivered payload %#v, want the INT-015-FAL row_added event discovered by the sweep", payload)
	}
	// Exactly ONE logical event: the next sweep interval re-reads the same
	// board and queues nothing.
	if _, err := SweepFunc(sweepCtx, h, boardDir, "boardctl"); err != nil {
		t.Fatal(err)
	}
	if len(inbox.messages) != 1 {
		t.Fatalf("deliveries after second interval = %d, want 1 (one logical event)", len(inbox.messages))
	}
}
