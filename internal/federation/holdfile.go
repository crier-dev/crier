package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// FileHoldQueue is the durable hold queue: held deliveries are persisted in
// one JSON document so they survive a source-relay restart (spec §8 "link
// down → durable queue at source"). Selected with CR_FED_QUEUE_FILE.
//
// Format and recovery: the document is {version, items}; every mutation
// rewrites it atomically —
//
//	marshal → write+fsync <path>.tmp → rename <path> → <path>.bak
//	        → rename <path>.tmp → <path>
//
// Both renames are atomic, so a crash at any point still leaves a parseable
// document: the new one at <path>, or the previous good one at <path>.bak
// (recovered on open). A crash between the two renames leaves <path> absent
// with the good document in <path>.bak, which OpenFileHoldQueue also
// recovers. At most the newest mutation is lost, never the whole queue.
//
// Queue bounds (MaxItems/MaxBodyBytes) are enforced exactly like the memory
// implementation. Persistence failures are returned to the caller after the
// in-memory state has been updated, so the caller logs them; the queue stays
// usable and the next successful mutation rewrites the document.
type FileHoldQueue struct {
	mu           sync.Mutex
	path         string
	items        []*HoldItem
	MaxItems     int
	MaxBodyBytes int
}

var _ HoldQueue = (*FileHoldQueue)(nil)

// errHoldFileMissing marks an absent document (first run, or the crash window
// between the two renames — where <path>.bak holds the good state).
var errHoldFileMissing = errors.New("federation: hold queue file missing")

const holdFileVersion = 1

// holdFile is the on-disk document.
type holdFile struct {
	Version int         `json:"version"`
	Items   []*HoldItem `json:"items"`
}

// OpenFileHoldQueue opens (or creates, on first write) the durable hold
// queue at path and loads the pending deliveries. A missing file is an empty
// queue; an unreadable or malformed file is recovered from <path>.bak, and
// only if both are unusable does it fail — losing held messages silently
// would be worse than refusing to start.
func OpenFileHoldQueue(path string) (*FileHoldQueue, error) {
	if path == "" {
		return nil, errors.New("federation: hold queue file path is empty")
	}
	q := &FileHoldQueue{path: path}
	items, err := loadHoldFile(path)
	switch {
	case err == nil:
		q.items = items
	case errors.Is(err, errHoldFileMissing):
		// Either a first run or the crash window between the two renames;
		// the backup tells which.
		if backup, berr := loadHoldFile(path + ".bak"); berr == nil {
			q.items = backup
			if len(backup) > 0 {
				slog.Warn("federation: hold queue document missing, recovered from backup",
					"path", path, "recovered", len(backup))
			}
		}
	default:
		backup, berr := loadHoldFile(path + ".bak")
		if berr != nil {
			return nil, fmt.Errorf("federation: open hold queue %s: %w", path, err)
		}
		slog.Warn("federation: hold queue document unusable, recovered from backup",
			"path", path, "error", err, "recovered", len(backup))
		q.items = backup
	}
	return q, nil
}

// Path returns the document path.
func (q *FileHoldQueue) Path() string { return q.path }

// loadHoldFile reads one document. A missing file yields errHoldFileMissing;
// structurally broken items inside an otherwise valid document are dropped
// with a warning (the rest of the queue stays recoverable).
func loadHoldFile(path string) ([]*HoldItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errHoldFileMissing
		}
		return nil, err
	}
	var doc holdFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Version != holdFileVersion {
		return nil, fmt.Errorf("parse %s: unsupported version %d", path, doc.Version)
	}
	items := make([]*HoldItem, 0, len(doc.Items))
	for _, item := range doc.Items {
		if err := validateHoldItem(item, DefaultMaxHoldBodyBytes); err != nil {
			slog.Warn("federation: dropping unusable held delivery from the queue document",
				"path", path, "error", err)
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func (q *FileHoldQueue) bodyCap() int {
	if q.MaxBodyBytes > 0 {
		return q.MaxBodyBytes
	}
	return DefaultMaxHoldBodyBytes
}

func (q *FileHoldQueue) itemCap() int {
	if q.MaxItems > 0 {
		return q.MaxItems
	}
	return DefaultMaxHoldItems
}

// Enqueue stores a held delivery and persists the queue.
func (q *FileHoldQueue) Enqueue(item *HoldItem) error {
	if err := validateHoldItem(item, q.bodyCap()); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if cap := q.itemCap(); len(q.items) >= cap {
		return fmt.Errorf("federation: hold queue full (%d held deliveries)", cap)
	}
	q.items = append(q.items, item.clone())
	return q.persistLocked()
}

// Update replaces the stored item with the same ID and persists the queue.
func (q *FileHoldQueue) Update(item *HoldItem) error {
	if err := validateHoldItem(item, q.bodyCap()); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, existing := range q.items {
		if existing.ID == item.ID {
			q.items[i] = item.clone()
			return q.persistLocked()
		}
	}
	return fmt.Errorf("federation: hold queue: unknown item %q", item.ID)
}

// Remove deletes the item with the given ID (no-op when absent) and persists
// the queue.
func (q *FileHoldQueue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, existing := range q.items {
		if existing.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return q.persistLocked()
		}
	}
	return nil
}

// List returns copies of the pending items.
func (q *FileHoldQueue) List() []*HoldItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*HoldItem, 0, len(q.items))
	for _, item := range q.items {
		out = append(out, item.clone())
	}
	return out
}

// Len returns the number of pending items.
func (q *FileHoldQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// persistLocked atomically replaces the on-disk document with the in-memory
// state (see the type doc for the crash guarantees).
func (q *FileHoldQueue) persistLocked() error {
	data, err := json.Marshal(holdFile{Version: holdFileVersion, Items: q.items})
	if err != nil {
		return fmt.Errorf("federation: marshal hold queue: %w", err)
	}
	if dir := filepath.Dir(q.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("federation: create hold queue dir: %w", err)
		}
	}
	tmp := q.path + ".tmp"
	// 0600: the document carries message bodies.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("federation: write hold queue: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("federation: write hold queue: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("federation: sync hold queue: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("federation: close hold queue: %w", err)
	}
	if _, err := os.Stat(q.path); err == nil {
		if err := os.Rename(q.path, q.path+".bak"); err != nil {
			return fmt.Errorf("federation: rotate hold queue backup: %w", err)
		}
	}
	if err := os.Rename(tmp, q.path); err != nil {
		return fmt.Errorf("federation: commit hold queue: %w", err)
	}
	return nil
}
