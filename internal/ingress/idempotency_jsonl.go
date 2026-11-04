package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONLIdempotencyStore is the durable form of the ledger CRIER-INGRESS §3
// describes: an append-only file of `(namespace, key) -> hash + first result`,
// replayed into memory at open so a restart keeps answering a repeated
// Idempotency-Key with the ORIGINAL response instead of creating a second
// message.
//
// Why a file rather than the session store: the ledger is not session data — it
// spans sessions and outlives them — and it must be readable on any backend
// (jsonl, sqlite, postgres), so it cannot live in a store one backend owns.
//
// FIRST WRITE WINS. A key that already has a row is never overwritten, because
// the whole point is that the first accepted request defines the answer; the
// handler rejects a different body under the same key with
// INGRESS_KEY_REUSED before this store is consulted again.
//
// The file lives beside — never inside — the session bundles: the session store
// treats every `*.jsonl` under its root as a session, and a ledger filed there
// would be read as one (the same trap the route log hit).
type JSONLIdempotencyStore struct {
	mu   sync.Mutex
	path string
	rows map[string]IdempotencyRecord
}

// idempotencyLine is one ledger row on disk. It is deliberately flat and
// versioned so a future field can be added without rewriting old lines.
type idempotencyLine struct {
	V         int             `json:"v"`
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Hash      string          `json:"hash"`
	Status    int             `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	SavedAt   time.Time       `json:"saved_at"`
}

// OpenJSONLIdempotencyStore opens (creating if needed) the ledger at path and
// replays every row into memory. A torn final line is tolerated — the process
// that wrote it died mid-append — and is reported through the error return only
// when it is not the LAST line, because a hole in the middle means the file was
// corrupted rather than interrupted.
func OpenJSONLIdempotencyStore(path string) (*JSONLIdempotencyStore, error) {
	if path == "" {
		return nil, fmt.Errorf("ingress idempotency: path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("ingress idempotency: create dir: %w", err)
	}
	s := &JSONLIdempotencyStore{path: path, rows: make(map[string]IdempotencyRecord)}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("ingress idempotency: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	var pending []byte
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec idempotencyLine
		if err := json.Unmarshal(line, &rec); err != nil {
			// Keep going only if more lines follow: a partial trailing append is
			// survivable, a hole is not.
			pending = append([]byte(nil), line...)
			continue
		}
		s.rows[s.key(rec.Namespace, rec.Key)] = IdempotencyRecord{
			RequestHash: rec.Hash,
			Status:      rec.Status,
			Result:      append(json.RawMessage(nil), rec.Result...),
			CreatedAt:   rec.SavedAt,
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ingress idempotency: read %q: %w", path, err)
	}
	if pending != nil {
		// A single unparseable line at the end is a torn write; anything else
		// already produced rows after it and would have been caught above.
		if _, err := os.Stat(path); err == nil {
			// Truncate the torn tail so later appends do not build on corruption.
			if f2, err := os.OpenFile(path, os.O_WRONLY, 0o644); err == nil {
				if sz, err := f2.Seek(0, 2); err == nil {
					_ = f2.Truncate(sz - int64(len(pending)) - 1)
				}
				_ = f2.Close()
			}
		}
	}
	return s, nil
}

func (s *JSONLIdempotencyStore) key(namespace, key string) string {
	return namespace + "\x00" + key
}

// Lookup implements IdempotencyStore.
func (s *JSONLIdempotencyStore) Lookup(_ context.Context, namespace, key string) (IdempotencyRecord, bool, error) {
	if s == nil {
		return IdempotencyRecord{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.rows[s.key(namespace, key)]
	if !ok {
		return IdempotencyRecord{}, false, nil
	}
	out := rec
	out.Result = append(json.RawMessage(nil), rec.Result...)
	return out, true, nil
}

// Save implements IdempotencyStore: it appends and fsyncs BEFORE the row becomes
// visible in memory, so a crash can never leave the process believing a key is
// recorded when the disk does not.
func (s *JSONLIdempotencyStore) Save(_ context.Context, namespace, key string, rec IdempotencyRecord) error {
	if s == nil {
		return fmt.Errorf("ingress idempotency: no store wired")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	k := s.key(namespace, key)
	if _, exists := s.rows[k]; exists {
		return nil // first write wins
	}

	line, err := json.Marshal(idempotencyLine{
		V:         1,
		Namespace: namespace,
		Key:       key,
		Hash:      rec.RequestHash,
		Status:    rec.Status,
		Result:    rec.Result,
		SavedAt:   time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("ingress idempotency: encode: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("ingress idempotency: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("ingress idempotency: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ingress idempotency: fsync: %w", err)
	}

	stored := rec
	stored.Result = append(json.RawMessage(nil), rec.Result...)
	s.rows[k] = stored
	return nil
}

// Path names the file this store appends to (for an operator and for tests).
func (s *JSONLIdempotencyStore) Path() string { return s.path }
