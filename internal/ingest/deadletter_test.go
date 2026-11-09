// deadletter_test.go — the INT-015 dead-letter gate (CRIER-GITHOOK-DISPATCH
// §10 amendment 2). With the ingest fixture answering 503 past the retry
// budget, the batch must be QUERYABLE from the persistent dead-letter store —
// count, last failure, row ids — never a silent dropped.log. Pre-INT-015
// (origin/main) this cannot even compile: there is no dead-letter store, which
// is itself the proof the old code cannot satisfy the contract.
package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

// failingInbox answers every delivery like a down ingest fixture: 503-class
// failures until freed, then success (the operator re-drive).
type failingInbox struct {
	calls int
	freed bool
}

func (f *failingInbox) Deliver(_ context.Context, _ string, _ string, _ ingress.InboxMessage) ([]session.DeliveryOutcome, error) {
	f.calls++
	if f.freed {
		return []session.DeliveryOutcome{{InboxEntryID: "recovered"}}, nil
	}
	return nil, fmt.Errorf("ingest fixture answered %d service unavailable", http.StatusServiceUnavailable)
}

// registerSource registers an ingest source through the POST /ingress/sources
// route and returns the once-shown secret, so tests can sign hook-style POSTs.
func registerSource(t *testing.T, h *Handler, id, deliverTo string) []byte {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"mapping":"passthrough","deliver_to":%q,"tags":["webhook","src:boardctl"]}`, id, deliverTo)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/sources", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register source: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	secret, err := hex.DecodeString(result.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestDeadLetterRecordsExhaustedBatch(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	inbox := &failingInbox{}
	h := &Handler{Store: store, Inbox: inbox, Now: func() time.Time { return time.Unix(1762627200, 0).UTC() }, DeadLetterBudget: 3, ReplayWindow: 300 * time.Second}

	// Register the boardctl source through the route so the secret is known
	// and the batch can be intake the way the hook POSTs it: signed.
	secret := registerSource(t, h, "boardctl", "board-agent")
	body := []byte(`{"rows":[{"id":"INT-500","title":"lost batch"},{"id":"INT-501","title":"also lost"}]}`)
	rec := signedRequest(t, h, "boardctl", secret, body, h.now().Unix(), nonce(9001))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("intake with failing fixture: status=%d body=%s, want 503 message-delivery-pending", rec.Code, rec.Body.String())
	}

	// The outbox row IS durable (accepted before delivery was attempted).
	pending, err := store.PendingRows(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v, want the accepted batch row", len(pending), err)
	}
	eventKey := pending[0].Key

	// Drain into the 503 fixture once per attempt until the budget is spent.
	for i := 0; i < 2; i++ {
		if err := h.Drain(context.Background()); err == nil {
			t.Fatalf("drain %d: expected delivery failure", i+1)
		}
	}
	if inbox.calls != 3 {
		t.Fatalf("fixture saw %d delivery attempts, want 3 (1 at intake + 2 drains = the budget)", inbox.calls)
	}

	// The batch is QUERYABLE: count, last failure, row ids.
	letters, err := store.DeadLetters(context.Background())
	if err != nil || len(letters) != 1 {
		t.Fatalf("DeadLetters()=%#v err=%v, want exactly one entry", letters, err)
	}
	dl := letters[0]
	if dl.Count != 3 {
		t.Fatalf("count=%d, want 3 (the budget)", dl.Count)
	}
	if !strings.Contains(dl.LastError, "503") {
		t.Fatalf("last failure %q does not name the 503", dl.LastError)
	}
	if dl.LastAt != h.now() {
		t.Fatalf("last_at=%v, want the handler clock %v", dl.LastAt, h.now())
	}
	if len(dl.RowIDs) != 2 || dl.RowIDs[0] != "INT-500" || dl.RowIDs[1] != "INT-501" {
		t.Fatalf("row_ids=%v, want [INT-500 INT-501]", dl.RowIDs)
	}
	if dl.SourceID != "boardctl" || dl.EventKey != eventKey || dl.MessageID != pending[0].MessageID {
		t.Fatalf("identity %#v, want the boardctl batch", dl)
	}
	if _, err := store.DeadLetter(context.Background(), "boardctl", eventKey); err != nil {
		t.Fatalf("single-key read: %v", err)
	}

	// Past the budget Drain SKIPS the batch — no attempt, no drop: the row
	// stays in the ledger and the entry stays queryable.
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("drain past budget must skip, not fail: %v", err)
	}
	if inbox.calls != 3 {
		t.Fatalf("post-budget drain attempted delivery (calls=%d)", inbox.calls)
	}
	if pending, err := store.PendingRows(context.Background()); err != nil || len(pending) != 1 {
		t.Fatalf("dead-lettered row vanished from ledger: pending=%d err=%v", len(pending), err)
	}

	// Operator re-drive: the fixture is brought back AND the batch is
	// requeued (clearing the dead-letter entry resets the attempt count);
	// the next drain delivers and the ledger empties.
	if err := store.RequeueDeadLetter(context.Background(), "boardctl", eventKey); err != nil {
		t.Fatal(err)
	}
	inbox.freed = true
	if err := h.Drain(context.Background()); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	if inbox.calls != 4 {
		t.Fatalf("re-drive attempts=%d, want 4", inbox.calls)
	}
	if pending, err := store.PendingRows(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("re-driven batch still pending: %d err=%v", len(pending), err)
	}
	if remaining, err := store.DeadLetters(context.Background()); err != nil || len(remaining) != 0 {
		t.Fatalf("recovered batch still dead-lettered: %#v err=%v", remaining, err)
	}
}

func TestDeadLetterDefaultsToBudgetThree(t *testing.T) {
	h := &Handler{}
	if h.deadLetterBudget() != DefaultDeadLetterBudget {
		t.Fatalf("default budget %d, want %d", h.deadLetterBudget(), DefaultDeadLetterBudget)
	}
}
