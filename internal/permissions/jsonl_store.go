package permissions

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxRecordLineBytes bounds one JSONL line. A permission record is small (a
// grant carries ids and an action list), so the cap exists only so a corrupt
// file cannot be read into unbounded memory.
const maxRecordLineBytes = 1 << 20

// storeFileName is the single append log a JSONL store owns. One file is
// enough: every record type shares the log and keep-LAST is applied by the
// fold, exactly as internal/session keeps one file per session.
const storeFileName = "permissions.jsonl"

// JSONLStore is the JSONL half of the dual store: the ORDERED APPEND LOG and
// the transport form. One record-version per line, append-only, never
// rewritten — the log is the log of record.
//
// It is a first-class peer, not an export: a permission set can be written to
// disk, shipped anywhere and re-imported by replaying the lines.
type JSONLStore struct {
	root string
}

var _ Store = (*JSONLStore)(nil)

// NewJSONLStore opens (creating when absent) a store root directory. The log
// lives at <root>/permissions.jsonl.
func NewJSONLStore(root string) (*JSONLStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: store root is empty", ErrInvalidRecord)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("permissions jsonl store: create root: %w", err)
	}
	return &JSONLStore{root: root}, nil
}

// Root returns the directory the store appends to.
func (s *JSONLStore) Root() string { return s.root }

// Path returns the log file path.
func (s *JSONLStore) Path() string { return filepath.Join(s.root, storeFileName) }

// Close releases nothing today (every append opens and fsyncs its own handle,
// which is what makes concurrent appenders safe on an append-only file); it
// exists so JSONLStore satisfies Store alongside PostgresStore.
func (s *JSONLStore) Close() error { return nil }

// Append writes ONE record-version line and fsyncs it before returning. The
// line is appended, never merged: the log keeps every version and keep-LAST is
// applied on read.
func (s *JSONLStore) Append(_ context.Context, rec *Record) error {
	line, err := rec.MarshalLine()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("permissions jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("permissions jsonl store: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("permissions jsonl store: fsync: %w", err)
	}
	return nil
}

// Records returns the log's records in file order. A missing log reads as an
// EMPTY store, not an error: a deployment that has written no permission
// record yet has an armed-but-empty ACL, which is a legitimate state.
func (s *JSONLStore) Records(_ context.Context) ([]*Record, error) {
	f, err := os.Open(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("permissions jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLineBytes)

	var recs []*Record
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rec, err := ParseRecord([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrInvalidRecord, s.Path(), lineNo, err)
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("permissions jsonl store: read log: %w", err)
	}
	return recs, nil
}

// Snapshot folds the log to the reduced state, applying keep-LAST per
// (type, id).
func (s *JSONLStore) Snapshot(ctx context.Context) (*Snapshot, error) {
	recs, err := s.Records(ctx)
	if err != nil {
		return nil, err
	}
	return Replay(recs)
}
