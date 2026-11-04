package ingress

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestJSONLIdempotencySurvivesRestart is the point of the durable ledger: a
// sender that retries with the same Idempotency-Key after the server restarts
// must get the ORIGINAL answer, not a second message. The in-memory ledger could
// not do this, and CRIER-INGRESS §3 asks for a stored one.
func TestJSONLIdempotencySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ingress", "idempotency.jsonl")

	first, err := OpenJSONLIdempotencyStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	body := json.RawMessage(`{"message_id":"m_1","seq":4}`)
	if err := first.Save(ctx, "default", "key-1", IdempotencyRecord{
		RequestHash: "hash-1",
		Status:      201,
		Result:      body,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A NEW store instance over the same file is what a restart looks like.
	second, err := OpenJSONLIdempotencyStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok, err := second.Lookup(ctx, "default", "key-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok {
		t.Fatal("the key did not survive the reopen: a retry would create a second message")
	}
	if got.RequestHash != "hash-1" || got.Status != 201 {
		t.Fatalf("replayed record = %+v, want hash-1/201", got)
	}
	if string(got.Result) != string(body) {
		t.Fatalf("replayed body = %s, want %s (a replay must be byte-identical)", got.Result, body)
	}

	// First write wins: a second Save on the same key changes nothing.
	if err := second.Save(ctx, "default", "key-1", IdempotencyRecord{
		RequestHash: "hash-DIFFERENT", Status: 500, Result: json.RawMessage(`{"nope":true}`),
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	third, err := OpenJSONLIdempotencyStore(path)
	if err != nil {
		t.Fatalf("reopen 3: %v", err)
	}
	got, _, _ = third.Lookup(ctx, "default", "key-1")
	if got.RequestHash != "hash-1" || got.Status != 201 {
		t.Fatalf("the ledger was overwritten: %+v", got)
	}

	// Keys are namespaced: the same key in another realm is a different request.
	if _, ok, _ := third.Lookup(ctx, "other-realm", "key-1"); ok {
		t.Fatal("a key leaked across namespaces")
	}
}

// TestJSONLIdempotencyToleratesATornTail keeps a crashed append from poisoning
// the ledger: an unparseable LAST line is dropped, and the rows before it still
// answer.
func TestJSONLIdempotencyToleratesATornTail(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ingress", "idempotency.jsonl")

	s, err := OpenJSONLIdempotencyStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Save(ctx, "default", "good", IdempotencyRecord{RequestHash: "h", Status: 200, Result: json.RawMessage(`{"ok":true}`)}); err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open for torn write: %v", err)
	}
	if _, err := f.WriteString(`{"v":1,"namespace":"default","key":"torn","ha`); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	_ = f.Close()

	reopened, err := OpenJSONLIdempotencyStore(path)
	if err != nil {
		t.Fatalf("reopen with a torn tail must not fail: %v", err)
	}
	if _, ok, _ := reopened.Lookup(ctx, "default", "good"); !ok {
		t.Fatal("the good row was lost because of a torn neighbour")
	}
	if _, ok, _ := reopened.Lookup(ctx, "default", "torn"); ok {
		t.Fatal("a torn row was accepted as a real record")
	}
	// And the ledger still accepts new keys after repairing the tail.
	if err := reopened.Save(ctx, "default", "after", IdempotencyRecord{RequestHash: "h2", Status: 200}); err != nil {
		t.Fatalf("save after repair: %v", err)
	}
}
