package webhook

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// QueueItem is one queued webhook delivery (endpoint down / transient failures).
type QueueItem struct {
	AgentID   string      `json:"agent_id"`
	Envelope  *Envelope   `json:"envelope,omitempty"`
	Batch     []*Envelope `json:"batch,omitempty"` // CR-FEAT-005: coalesced batch; redelivery POSTs it as ONE batch request
	Retries   int         `json:"retries"`
	CreatedAt time.Time   `json:"created_at"`
	// RequestID is the X-Request-Id of the HTTP request that accepted this
	// delivery (DF-CRIER-141). It rides with the item so the background
	// dispatch/outcome log lines carry the same correlation id as the
	// deliver request that enqueued it. Empty for non-HTTP callers.
	RequestID string `json:"request_id,omitempty"`
	// Guard carries the verdict for redelivery (CR-FEAT-010, spec §2.1):
	// redelivery and batch flush NEVER re-run the guard — the verdict rides
	// with the message. The envelope's crier.guard metadata is the
	// authoritative copy for the outbound POST headers; this field is the
	// durable carry for queue-item consumers.
	Guard *guard.Result `json:"guard,omitempty"`
}

// Queue stores pending webhook deliveries. v1 ships the in-memory
// implementation; a Postgres-backed queue is a fast-follow
// (spec §10, open decision 1).
type Queue interface {
	Push(item *QueueItem) error
	PopBatch(max int) []*QueueItem
	Len() int
}

// MemoryQueue is a FIFO queue with a mutex. Durable within process lifetime;
// server restart drains it (documented v1 trade-off for the memory backend).
type MemoryQueue struct {
	mu    sync.Mutex
	items []*QueueItem
}

// NewMemoryQueue builds an empty queue.
func NewMemoryQueue() *MemoryQueue {
	return &MemoryQueue{}
}

// Push appends an item.
func (q *MemoryQueue) Push(item *QueueItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, item)
	return nil
}

// PopBatch removes and returns up to max items (FIFO).
func (q *MemoryQueue) PopBatch(max int) []*QueueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	if max <= 0 || max > len(q.items) {
		max = len(q.items)
	}
	out := q.items[:max]
	q.items = q.items[max:]
	return out
}

// Len returns the number of queued items.
func (q *MemoryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// OldestCreatedAt reports the CreatedAt of the OLDEST pending item, and false
// when nothing is queued (MON-CRIER-004).
//
// It scans every item rather than reading items[0]: a redelivery re-Push
// appends the same item at the tail with its original CreatedAt, so FIFO
// position is NOT creation order. The oldest CreatedAt is the honest answer to
// "how long has the oldest un-delivered push been waiting".
//
// This is an optional capability of a Queue implementation (the driver reads it
// through an interface assertion), not part of the Queue interface: a queue
// that cannot report an age leaves the gauge at 0 instead of inventing one.
func (q *MemoryQueue) OldestCreatedAt() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var oldest time.Time
	for _, it := range q.items {
		if it == nil || it.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || it.CreatedAt.Before(oldest) {
			oldest = it.CreatedAt
		}
	}
	if oldest.IsZero() {
		return time.Time{}, false
	}
	return oldest, true
}

// marshalJSON is a helper for tests/debug.
func (q *QueueItem) marshalJSON() ([]byte, error) {
	return json.Marshal(q)
}
