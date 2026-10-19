package registry

import "sync"

// InboxAckObserver is an optional, process-local observation of successful Ack
// calls. It is not a tombstone store: only currently watched entries consume
// memory, and stop releases a watch. An unobserved removal proves no outcome.
// The returned acknowledged function is safe to call concurrently with Ack.
// Register before reading the entry, and always defer stop.
type InboxAckObserver interface {
	WatchAcknowledgement(agentID, messageID string) (acknowledged func() bool, stop func())
}

type inboxAckKey struct{ agentID, messageID string }

type inboxAckWatch struct{ acknowledged bool }

// inboxAcknowledgements is embedded by both local stores. Its zero value is
// ready to use, including in the PostgreSQL mock fixtures. No callbacks execute
// under a store lock, and observers never acquire a store lock through it.
type inboxAcknowledgements struct {
	mu      sync.Mutex
	watches map[inboxAckKey]map[*inboxAckWatch]struct{}
}

func (a *inboxAcknowledgements) WatchAcknowledgement(agentID, messageID string) (func() bool, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.watches == nil {
		a.watches = make(map[inboxAckKey]map[*inboxAckWatch]struct{})
	}
	key := inboxAckKey{agentID, messageID}
	if a.watches[key] == nil {
		a.watches[key] = make(map[*inboxAckWatch]struct{})
	}
	watch := &inboxAckWatch{}
	a.watches[key][watch] = struct{}{}
	return func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return watch.acknowledged
		}, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			delete(a.watches[key], watch)
			if len(a.watches[key]) == 0 {
				delete(a.watches, key)
			}
		}
}

// recordAcknowledgement is called ONLY after the whole Ack succeeded (after
// transaction commit on PostgreSQL), never from CloseEntry or an expiry sweep.
func (a *inboxAcknowledgements) recordAcknowledgement(agentID string, messageIDs []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range messageIDs {
		for watch := range a.watches[inboxAckKey{agentID, id}] {
			watch.acknowledged = true
		}
	}
}

// WatchAcknowledgement preserves the local backend capability through the
// service-delivery policy wrapper used by the production composition root.
func (s *AuthorityStore) WatchAcknowledgement(agentID, messageID string) (func() bool, func()) {
	if observer, ok := s.Store.(InboxAckObserver); ok {
		return observer.WatchAcknowledgement(agentID, messageID)
	}
	return func() bool { return false }, func() {}
}

var (
	_ InboxAckObserver = (*AuthorityStore)(nil)
	_ InboxAckObserver = (*MemoryStore)(nil)
	_ InboxAckObserver = (*PostgresStore)(nil)
)
