package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestHoldFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "fed-hold.json")
}

// TestOpenFileHoldQueueFirstRunIsEmpty: a missing document is an empty queue
// (nothing to recover, nothing to complain about).
func TestOpenFileHoldQueueFirstRunIsEmpty(t *testing.T) {
	path := newTestHoldFile(t)
	q, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("OpenFileHoldQueue: %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("Len = %d, want 0 on first run", q.Len())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("open must not create the document before the first write (stat err = %v)", err)
	}
	if _, err := OpenFileHoldQueue(""); err == nil {
		t.Error("an empty path must be rejected")
	}
}

// TestFileHoldQueueSurvivesReopen is the durability regression test: queued
// work is reloaded, field-for-field, by a brand-new queue over the same file
// — the source-relay restart path.
func TestFileHoldQueueSurvivesReopen(t *testing.T) {
	path := newTestHoldFile(t)
	q, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute).Truncate(time.Millisecond)
	items := []*HoldItem{
		{
			ID: "m-1", AgentID: "remote-a", Body: json.RawMessage(`{"payload":{"text":"one"}}`),
			Sender: "agent-local", RequestID: "req-1", SessionID: "sess-1",
			EnqueuedAt: time.Now().Truncate(time.Millisecond), Deadline: deadline,
			NextAttemptAt: time.Now().Add(time.Second).Truncate(time.Millisecond),
			Attempts:      3, LastStatus: 503, LastError: "link down",
		},
		{
			ID: "m-2", AgentID: "remote-b", Body: json.RawMessage(`{"payload":{"text":"two"}}`),
			EnqueuedAt: time.Now().Truncate(time.Millisecond), Deadline: deadline,
		},
	}
	for _, item := range items {
		if err := q.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}

	// A second process: a new queue over the same document.
	reopened, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("reopened Len = %d, want 2 (queued work must survive a restart)", reopened.Len())
	}
	got := map[string]*HoldItem{}
	for _, item := range reopened.List() {
		got[item.ID] = item
	}
	first, ok := got["m-1"]
	if !ok {
		t.Fatal("m-1 did not survive the reopen")
	}
	if first.AgentID != "remote-a" || first.Sender != "agent-local" || first.RequestID != "req-1" || first.SessionID != "sess-1" {
		t.Errorf("m-1 lost context: %+v", first)
	}
	if string(first.Body) != `{"payload":{"text":"one"}}` {
		t.Errorf("m-1 body = %s, want the original bytes", first.Body)
	}
	if first.Attempts != 3 || first.LastStatus != 503 || first.LastError != "link down" {
		t.Errorf("m-1 retry state lost: %+v", first)
	}
	if !first.Deadline.Equal(deadline) {
		t.Errorf("m-1 deadline = %s, want %s (the original budget, not a fresh one)", first.Deadline, deadline)
	}
	if !first.NextAttemptAt.Equal(items[0].NextAttemptAt) {
		t.Errorf("m-1 next attempt = %s, want %s", first.NextAttemptAt, items[0].NextAttemptAt)
	}

	// Mutations from the reopened queue are durable too.
	if err := reopened.Remove("m-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	third, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("reopen #2: %v", err)
	}
	if third.Len() != 1 || third.List()[0].ID != "m-2" {
		t.Errorf("third open = %+v, want just m-2", third.List())
	}
}

// TestFileHoldQueueRecoversFromBackup: a torn/unreadable document is
// recovered from the previous good copy instead of losing the queue (or
// refusing to start).
func TestFileHoldQueueRecoversFromBackup(t *testing.T) {
	path := newTestHoldFile(t)
	q, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(&HoldItem{ID: "m-1", AgentID: "a", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue m-1: %v", err)
	}
	// Second mutation rotates the first document to <path>.bak.
	if err := q.Enqueue(&HoldItem{ID: "m-2", AgentID: "b", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue m-2: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("expected a rotated backup document: %v", err)
	}

	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatalf("corrupt document: %v", err)
	}
	recovered, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("reopen must recover from the backup, got: %v", err)
	}
	if recovered.Len() != 1 || recovered.List()[0].ID != "m-1" {
		t.Errorf("recovered = %+v, want the previous good document (m-1)", recovered.List())
	}
}

// TestFileHoldQueueMissingMainDocumentRecoversBackup covers the crash window
// between persistLocked's two renames: the main document is gone and the
// backup holds the good state.
func TestFileHoldQueueMissingMainDocumentRecoversBackup(t *testing.T) {
	path := newTestHoldFile(t)
	q, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(&HoldItem{ID: "m-1", AgentID: "a", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue m-1: %v", err)
	}
	if err := q.Enqueue(&HoldItem{ID: "m-2", AgentID: "b", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue m-2: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove main document: %v", err)
	}
	recovered, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if recovered.Len() != 1 || recovered.List()[0].ID != "m-1" {
		t.Errorf("recovered = %+v, want the backup (m-1)", recovered.List())
	}
}

// TestFileHoldQueueUnrecoverableDocumentFailsFast: when neither the document
// nor its backup can be parsed, refuse to start rather than silently drop
// held deliveries.
func TestFileHoldQueueUnrecoverableDocumentFailsFast(t *testing.T) {
	path := newTestHoldFile(t)
	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(path+".bak", []byte("also torn"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	if _, err := OpenFileHoldQueue(path); err == nil {
		t.Fatal("an unrecoverable document must fail fast, not start empty")
	}
}

// TestFileHoldQueueBoundsAndPermissions: the durable queue enforces the same
// bounds as the memory queue and keeps the document unreadable by others.
func TestFileHoldQueueBoundsAndPermissions(t *testing.T) {
	path := newTestHoldFile(t)
	q, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	q.MaxItems = 1
	q.MaxBodyBytes = 16
	if err := q.Enqueue(&HoldItem{ID: "m-1", AgentID: "a", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(&HoldItem{ID: "m-2", AgentID: "a", Body: json.RawMessage(`{"payload":{}}`)}); err == nil {
		t.Error("MaxItems must be enforced")
	}
	if err := q.Enqueue(&HoldItem{ID: "m-3", AgentID: "a", Body: json.RawMessage(`{"payload":"much longer than the cap"}`)}); err == nil {
		t.Error("MaxBodyBytes must be enforced")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("document mode = %o, want 600 (it carries message bodies)", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temp document must not be left behind (stat err = %v)", err)
	}
}

// TestHeldDeliverySurvivesSourceRelayRestart is the end-to-end durability
// regression: process 1 holds a delivery because its link is down and dies;
// process 2 reopens the queue, resumes the hold, and delivers the original
// bytes exactly once when the link is back.
func TestHeldDeliverySurvivesSourceRelayRestart(t *testing.T) {
	path := newTestHoldFile(t)

	// ---- process 1: link down, delivery held, then "crash" ----
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // unreachable link, as when a relay host is gone

	proc1Client := NewClient([]string{dead.URL}, time.Second, "")
	proc1Queue, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("process 1 open: %v", err)
	}
	proc1 := NewHoldManager(proc1Client, proc1Queue, testHoldConfig(time.Minute))
	proc1Client.SetHoldManager(proc1)

	body := []byte(`{"payload":{"text":"survive the restart"},"sender":"agent-local","delivery_mode":"blocking","request_id":"req-restart"}`)
	_, _, held, err := proc1Client.ForwardOrHold(context.Background(), "agent-remote", body, HoldMeta{
		MessageID: "msg-restart", Sender: "agent-local", RequestID: "req-restart",
	})
	mustHold(t, held, err)
	dead.Close() // the link is still down when process 1 goes away

	// ---- process 2: new client, new manager, same durable queue ----
	relay := newFlakyRelay(t, http.StatusOK)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	proc2Client := NewClient([]string{srv.URL}, time.Second, "")
	proc2Queue, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("process 2 reopen: %v", err)
	}
	if proc2Queue.Len() != 1 {
		t.Fatalf("reopened queue Len = %d, want the held delivery", proc2Queue.Len())
	}
	proc2 := NewHoldManager(proc2Client, proc2Queue, testHoldConfig(time.Minute))
	proc2Client.SetHoldManager(proc2)
	proc2.Start()
	defer proc2.Stop()

	waitFor(t, 2*time.Second, "the resumed hold to be delivered", func() bool { return proc2Queue.Len() == 0 })
	time.Sleep(30 * time.Millisecond)

	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.bodies) != 1 {
		t.Fatalf("forwarded %d times after the restart, want exactly 1", len(relay.bodies))
	}
	if string(relay.bodies[0]) != string(body) {
		t.Errorf("forwarded body = %s, want the original bytes", relay.bodies[0])
	}
	if relay.hops[0] != "1" {
		t.Errorf("hop header = %q, want \"1\"", relay.hops[0])
	}

	// The durable document now holds nothing pending.
	final, err := OpenFileHoldQueue(path)
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	if final.Len() != 0 {
		t.Errorf("durable queue Len = %d, want 0 after delivery", final.Len())
	}
}
