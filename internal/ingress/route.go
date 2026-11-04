package ingress

// route.go — the ONE new record kind this row writes (COURIER-INGRESS §3).
//
// `ingress.route` answers "where did this push actually land": the inbox entry
// id it produced, the agent (or the capability that selected it), the author,
// the tags and when. It is written ONLY for mode=inbox, because the two session
// modes already record themselves — a session message is a transcript record and
// a new session is a session.create — and a second copy of the same fact is
// exactly the kind of parallel bookkeeping that drifts.
//
// Storage: §3 allows a JSONL store ("one line") and the SQL spelling ("a row in
// chat_messages with kind=ingress.route"). This row implements the JSONL form,
// written beside the session log root it describes. The SQL form needs a new
// message kind in three backends' projections; that is a storage change this
// row deliberately does not make, and RouteStore is the seam it would land
// behind — the handler depends on the interface, not on the file.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// RouteRecord is one `ingress.route` line (§3).
type RouteRecord struct {
	ID string `json:"id"`
	// AgentID is the RESOLVED recipient: the named agent, or the holder the
	// capability selector picked. It is never "": an entry with no destination
	// would describe a delivery that did not happen.
	AgentID string `json:"agent_id"`
	// Capability is the selector the caller named, "" when it addressed the
	// agent directly.
	Capability string `json:"capability"`
	// MessageID is the inbox entry id the delivery path returned — the id an
	// ack later names, so the route record and the ack cannot disagree.
	MessageID string            `json:"message_id"`
	Author    session.AuthorRef `json:"author"`
	// Tags are the normalized tags the entry was stored with (COURIER-TAGS
	// §2.4), so an inbox filter can answer for a queued message without
	// reading the session it came from.
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RouteStore records one ingress route.
type RouteStore interface {
	RecordRoute(ctx context.Context, rec RouteRecord) error
}

// JSONLRouteStore appends route records to `<root>/ingress-routes.jsonl`, one
// JSON object per line, fsync'd before the call returns: a route record that
// survives only in a buffer would be a receipt for a delivery that may not have
// happened.
type JSONLRouteStore struct {
	path string
	mu   sync.Mutex
}

// NewJSONLRouteStore opens (creating if needed) the route log under root.
func NewJSONLRouteStore(root string) (*JSONLRouteStore, error) {
	if root == "" {
		return nil, fmt.Errorf("ingress route log: root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ingress route log: create %q: %w", root, err)
	}
	return &JSONLRouteStore{path: filepath.Join(root, "ingress-routes.jsonl")}, nil
}

// Path names the file this store appends to (for an operator and for tests).
func (s *JSONLRouteStore) Path() string { return s.path }

// RecordRoute implements RouteStore.
func (s *JSONLRouteStore) RecordRoute(_ context.Context, rec RouteRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ingress route record: encode: %w", err)
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("ingress route record: open %q: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("ingress route record: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ingress route record: fsync: %w", err)
	}
	return nil
}
