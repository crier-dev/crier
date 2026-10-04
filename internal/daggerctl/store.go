package daggerctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxRecordLineBytes bounds one stored JSONL line, so a corrupt file cannot be
// read into unbounded memory.
const maxRecordLineBytes = 1 << 20

// bundleFileName is the single append log a JSONLStore keeps: one line per
// RECORD VERSION, never rewritten, with keep-LAST applied on read.
const bundleFileName = "runs.jsonl"

// Store holds crier's run records. It is an APPEND LOG, exactly like the
// session store (internal/session/jsonl_store.go): every state change appends
// a new version of the record and a read reduces the log by keep-LAST per run
// id. Nothing is rewritten in place, so a concurrent reader can never observe a
// half-written record.
type Store interface {
	// Append writes one record version. It never merges: the log keeps every
	// version and the reduction applies keep-LAST.
	Append(ctx context.Context, rec *RunRecord) error
	// Get returns the newest version of a run's record, or ErrRunNotFound.
	// The returned record is a COPY: mutating it does not change the store.
	Get(ctx context.Context, runID string) (*RunRecord, error)
	// List returns every run's newest version, ordered by creation time then
	// run id, so a caller sees a deterministic list.
	List(ctx context.Context) ([]*RunRecord, error)
	// Close releases any held resources.
	Close() error
}

// cloneRecord deep-copies a record so a store never hands out (or keeps) a
// reference a caller can mutate behind its back.
func cloneRecord(rec *RunRecord) *RunRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	if rec.Evidence != nil {
		cp.Evidence = append([]string(nil), rec.Evidence...)
	}
	if rec.Nodes != nil {
		cp.Nodes = append([]Node(nil), rec.Nodes...)
	}
	return &cp
}

// reduceLatest applies keep-LAST per run id, preserving file order for the
// surviving versions.
func reduceLatest(recs []*RunRecord) []*RunRecord {
	idx := make(map[string]int, len(recs))
	kept := make([]*RunRecord, 0, len(recs))
	for _, rec := range recs {
		if rec == nil || rec.RunID == "" {
			continue
		}
		if i, ok := idx[rec.RunID]; ok {
			kept[i] = rec
			continue
		}
		idx[rec.RunID] = len(kept)
		kept = append(kept, rec)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.Before(kept[j].CreatedAt)
		}
		return kept[i].RunID < kept[j].RunID
	})
	return kept
}

// MemoryStore is a process-lifetime Store. It is what the server runs when no
// store directory is configured, and what the tests use.
type MemoryStore struct {
	mu   sync.Mutex
	runs map[string]*RunRecord
}

// NewMemoryStore returns an empty in-memory run store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: make(map[string]*RunRecord)}
}

var _ Store = (*MemoryStore)(nil)

// Append stores a copy of rec as the newest version of its run.
func (m *MemoryStore) Append(_ context.Context, rec *RunRecord) error {
	if rec == nil || strings.TrimSpace(rec.RunID) == "" {
		return fmt.Errorf("%w: run id is empty", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[rec.RunID] = cloneRecord(rec)
	return nil
}

// Get returns the newest version of the run's record.
func (m *MemoryStore) Get(_ context.Context, runID string) (*RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.runs[runID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRunNotFound, runID)
	}
	return cloneRecord(rec), nil
}

// List returns every run, ordered deterministically.
func (m *MemoryStore) List(_ context.Context) ([]*RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*RunRecord, 0, len(m.runs))
	for _, rec := range m.runs {
		out = append(out, cloneRecord(rec))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	return out, nil
}

// Close is a no-op for the in-memory store.
func (m *MemoryStore) Close() error { return nil }

// JSONLStore is the durable Store: one append-only file of record versions,
// fsynced per append, reduced on read. It follows the session store's shape
// (internal/session/jsonl_store.go) because the property that matters is the
// same one: the log is the log of record, and a reader can always rebuild the
// newest state from it without a side table.
type JSONLStore struct {
	root string
	mu   sync.Mutex
}

// NewJSONLStore opens (creating when absent) a store directory. Records live
// in <root>/runs.jsonl.
func NewJSONLStore(root string) (*JSONLStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: store directory is empty", ErrInvalidInput)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("daggerctl jsonl store: create directory: %w", err)
	}
	return &JSONLStore{root: root}, nil
}

// Root returns the directory the log lives in.
func (s *JSONLStore) Root() string { return s.root }

// Path returns the append log's path.
func (s *JSONLStore) Path() string { return filepath.Join(s.root, bundleFileName) }

var _ Store = (*JSONLStore)(nil)

// Append writes one record version and fsyncs it before returning.
func (s *JSONLStore) Append(_ context.Context, rec *RunRecord) error {
	if rec == nil || strings.TrimSpace(rec.RunID) == "" {
		return fmt.Errorf("%w: run id is empty", ErrInvalidInput)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("daggerctl jsonl store: encode record: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("daggerctl jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("daggerctl jsonl store: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("daggerctl jsonl store: fsync: %w", err)
	}
	return nil
}

// readAll reads every record version, in file order. An absent log is an empty
// log — a store that has never recorded a run is not a failure.
func (s *JSONLStore) readAll() ([]*RunRecord, error) {
	f, err := os.Open(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("daggerctl jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLineBytes)
	var recs []*RunRecord
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec RunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("daggerctl jsonl store: %s line %d: %w", s.Path(), lineNo, err)
		}
		recs = append(recs, &rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("daggerctl jsonl store: read log: %w", err)
	}
	return recs, nil
}

// Get returns the newest version of the run's record.
func (s *JSONLStore) Get(_ context.Context, runID string) (*RunRecord, error) {
	recs, err := s.readAll()
	if err != nil {
		return nil, err
	}
	for _, rec := range reduceLatest(recs) {
		if rec.RunID == runID {
			return cloneRecord(rec), nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrRunNotFound, runID)
}

// List returns every run's newest version.
func (s *JSONLStore) List(_ context.Context) ([]*RunRecord, error) {
	recs, err := s.readAll()
	if err != nil {
		return nil, err
	}
	kept := reduceLatest(recs)
	out := make([]*RunRecord, 0, len(kept))
	for _, rec := range kept {
		out = append(out, cloneRecord(rec))
	}
	return out, nil
}

// Close is a no-op: every append opens and fsyncs its own handle, which is
// what makes concurrent appenders safe on an append-only file.
func (s *JSONLStore) Close() error { return nil }
