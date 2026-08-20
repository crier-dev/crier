package webhook

import (
	"encoding/json"
	"sync"
	"time"
)

// QueueItem is one queued webhook delivery (endpoint down / transient failures).
type QueueItem struct {
	AgentID   string    `json:"agent_id"`
	Envelope  *Envelope `json:"envelope"`
	Retries   int       `json:"retries"`
	CreatedAt time.Time `json:"created_at"`
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
	mu  sync.Mutex
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

// marshalJSON is a helper for tests/debug.
func (q *QueueItem) marshalJSON() ([]byte, error) {
	return json.Marshal(q)
}
