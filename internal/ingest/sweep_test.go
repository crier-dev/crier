package ingest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sweepSetup builds a handler + store + captureInbox and returns the board dir
// whose .coding-hermes/board the sweep reads. The boardctl source is NOT yet
// registered — call registerSweepSource unless the test needs it absent. The
// board file is written DIRECTLY by the tests — no ingest POST — exactly the
// hook-less delivery the §10 sweep must make non-fatal.
func sweepSetup(t *testing.T) (*Handler, *Store, *captureInbox, string) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cap := &captureInbox{store: store}
	h := &Handler{Store: store, Inbox: cap, Now: func() time.Time { return time.Unix(1762627200, 0).UTC() }}
	boardDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(boardDir, ".coding-hermes", "board"), 0700); err != nil {
		t.Fatal(err)
	}
	return h, store, cap, boardDir
}

func registerSweepSource(t *testing.T, h *Handler) {
	t.Helper()
	src := Source{ID: DefaultBoardctlSourceID, Secret: []byte("sweep-secret"), Mapping: "passthrough", DeliverTo: "board-agent", Tags: []string{"webhook", "src:boardctl"}, CreatedAt: h.now()}
	if err := h.Store.CreateSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
}

func writeBoard(t *testing.T, boardDir string, lines ...map[string]any) {
	t.Helper()
	var raw strings.Builder
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	path := filepath.Join(boardDir, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.WriteFile(path, []byte(raw.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func row(id, title, status string) map[string]any {
	return map[string]any{"id": id, "title": title, "status": status, "priority": "P1"}
}

func TestSweepDeliversHooklessPendingRowsOnce(t *testing.T) {
	h, store, cap, boardDir := sweepSetup(t)
	registerSweepSource(t, h)
	writeBoard(t, boardDir,
		row("INT-420", "hook-less row", "pending"),
		row("INT-421", "already done", "complete"),
		row("fixtures", "fixture header", "pending"),
		row("INT-422", "another pending row", "pending"),
	)
	ctx := context.Background()
	n, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The complete row and the fixtures header are never delivered.
	if n != 2 || len(cap.messages) != 2 {
		t.Fatalf("enqueued=%d delivered=%d, want 2/2", n, len(cap.messages))
	}
	// SEEN and ADDED are ONE logical event: a second sweep re-reading the
	// unchanged board enqueues nothing.
	n, err = sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID)
	if err != nil || n != 0 || len(cap.messages) != 2 {
		t.Fatalf("second sweep enqueued=%d delivered=%d err=%v, want 0/2/nil", n, len(cap.messages), err)
	}
	// The delivery went to the source's registered agent.
	for _, agent := range cap.agents {
		if agent != "board-agent" {
			t.Fatalf("delivered to %q, want board-agent", agent)
		}
	}
	// The ledger holds the logical keys for later sweeps.
	rows, _ := ReadBoardRows(boardDir)
	for _, r := range rows {
		if !r.Pending || r.Fixture {
			continue
		}
		if seen, err := store.SeenEventKey(ctx, DefaultBoardctlSourceID, r.LogicalEventKey()); err != nil || !seen {
			t.Fatalf("logical key for %s not in ledger: seen=%v err=%v", r.ID, seen, err)
		}
	}
}

func TestSweepSeenThenAddedCollapseToOneEvent(t *testing.T) {
	h, _, cap, boardDir := sweepSetup(t)
	registerSweepSource(t, h)
	ctx := context.Background()
	// The hook fired first: the row was SEEN during a pull and POSTed, so the
	// ledger already holds its logical event key under a hook nonce.
	writeBoard(t, boardDir, row("INT-430", "pulled row", "pending"))
	rows, err := ReadBoardRows(boardDir)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read board: %v rows=%d", err, len(rows))
	}
	key := rows[0].LogicalEventKey()
	if strings.HasPrefix(key, "boardctl:boardctl:") || !strings.Contains(key, "boardctl:INT-430:") {
		t.Fatalf("logical event key %q does not follow boardctl:<row-id>:<content-hash>", key)
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	dup, _, err := h.Store.Accept(ctx, DefaultBoardctlSourceID, key, base64.RawURLEncoding.EncodeToString(nonceBytes), "hook-message-id", []byte(`{"id":"INT-430"}`), h.now())
	if err != nil || dup {
		t.Fatalf("seed hook acceptance: dup=%v err=%v", dup, err)
	}
	// Now the sweep sees the SAME row on the board (the ADDED path a later
	// commit would take, or the sweep's own pass). It must queue NOTHING.
	n, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID)
	if err != nil || n != 0 || len(cap.messages) != 0 {
		t.Fatalf("sweep re-queued a SEEN row: enqueued=%d delivered=%d err=%v", n, len(cap.messages), err)
	}
}

func TestSweepSameIDContentEditIsNewEvent(t *testing.T) {
	h, _, cap, boardDir := sweepSetup(t)
	registerSweepSource(t, h)
	ctx := context.Background()
	writeBoard(t, boardDir, row("INT-440", "original title", "pending"))
	if _, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 1 {
		t.Fatalf("first pass delivered %d, want 1", len(cap.messages))
	}
	// The row id is kept but the content is edited: a NEW logical event, not a
	// suppressed duplicate.
	writeBoard(t, boardDir, row("INT-440", "edited title", "pending"))
	n, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID)
	if err != nil || n != 1 || len(cap.messages) != 2 {
		t.Fatalf("edited row not re-delivered: enqueued=%d delivered=%d err=%v", n, len(cap.messages), err)
	}
}

func TestSweepRebaseRowIDChangeDedupesOnContentHash(t *testing.T) {
	h, _, cap, boardDir := sweepSetup(t)
	registerSweepSource(t, h)
	ctx := context.Background()
	// Pre-rebase: row OLD-1 was delivered with content C.
	content := row("OLD-1", "rebased row", "pending")
	writeBoard(t, boardDir, content)
	if _, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID); err != nil {
		t.Fatal(err)
	}
	if len(cap.messages) != 1 {
		t.Fatalf("pre-rebase delivery count %d, want 1", len(cap.messages))
	}
	// The rebase rewrites the row id; the content is unchanged. The re-queued
	// clone must be absorbed on the content hash, not delivered twice.
	writeBoard(t, boardDir, row("NEW-1", "rebased row", "pending"))
	n, err := sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID)
	if err != nil || n != 0 || len(cap.messages) != 1 {
		t.Fatalf("rebase clone not absorbed: enqueued=%d delivered=%d err=%v", n, len(cap.messages), err)
	}
	// ... while a genuinely NEW row (new id AND new content) still delivers.
	writeBoard(t, boardDir,
		row("NEW-1", "rebased row", "pending"),
		row("NEW-2", "brand new row", "pending"),
	)
	if n, err = sweepOnce(ctx, h, boardDir, DefaultBoardctlSourceID); err != nil || n != 1 || len(cap.messages) != 2 {
		t.Fatalf("new row suppressed by rebase dedup: enqueued=%d delivered=%d err=%v", n, len(cap.messages), err)
	}
}

func TestSweepUnregisteredSourceIsVisibleNotSilent(t *testing.T) {
	h, _, _, boardDir := sweepSetup(t)
	writeBoard(t, boardDir, row("INT-450", "row", "pending"))
	if _, err := sweepOnce(context.Background(), h, boardDir, DefaultBoardctlSourceID); err == nil {
		t.Fatal("sweep with unregistered source returned nil error — the failure must be visible")
	}
}

func TestReadBoardRowsSkipsUnparsableLines(t *testing.T) {
	_, _, _, boardDir := sweepSetup(t)
	path := filepath.Join(boardDir, ".coding-hermes", "board", "tasks.jsonl")
	body := "{\"id\":\"INT-1\",\"status\":\"pending\"}\nnot json at all\n\n{\"no id\"}\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadBoardRows(boardDir)
	if err != nil || len(rows) != 1 || rows[0].ID != "INT-1" || !rows[0].Pending {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}
