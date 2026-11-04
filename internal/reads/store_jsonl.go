// store_jsonl.go — the JSONL read-cursor store (spec §4).
//
// One file per session at `reads/{session_id}.jsonl` under the session log
// root, ONE LINE PER ADVANCE, append-only, last line wins at replay. Compaction
// is out of scope; the replay folds to the highest seq anyway, so a later line
// carrying a lower value can never lower a cursor.
//
// Monotonicity is enforced with a read-modify-write under this store's own
// mutex (the "same lock the transcript append uses" half of §3.2), and a
// cross-process append is safe because a cursor line is far smaller than a pipe
// buffer and the fold takes the maximum regardless of arrival order.
package reads

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
	"time"
)

// cursorFormatVersion is the only line version this store writes and reads. An
// unknown version is refused loudly, exactly as the session log refuses one.
const cursorFormatVersion = 1

// readsDirName is the subdirectory of the session log root the cursors live in
// (§4: `reads/{session_id}.jsonl`).
const readsDirName = "reads"

// maxCursorLineBytes bounds one cursor line. A line carries five short fields;
// the cap exists so a corrupt file cannot be read into unbounded memory.
const maxCursorLineBytes = 64 << 10

// cursorVersion is ONE line of the append-only cursor log.
type cursorVersion struct {
	V              int       `json:"v"`
	SessionID      string    `json:"session_id"`
	MemberType     string    `json:"member_type"`
	MemberID       string    `json:"member_id"`
	ReadThroughSeq int64     `json:"read_through_seq"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// JSONLStore is the JSONL-backed cursor store. root is the SESSION log root, so
// the files land at `<root>/reads/<session_id>.jsonl` — the layout §4 names.
type JSONLStore struct {
	root string
	mu   sync.Mutex
}

var _ Store = (*JSONLStore)(nil)

// NewJSONLStore opens (creating when absent) the reads directory under a
// session log root. A JSONL deployment therefore keeps its cursors beside the
// transcripts they describe, with no second location to configure.
func NewJSONLStore(sessionLogRoot string) (*JSONLStore, error) {
	if strings.TrimSpace(sessionLogRoot) == "" {
		return nil, fmt.Errorf("%w: log root is empty", ErrStoreError)
	}
	dir := filepath.Join(sessionLogRoot, readsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: create reads dir: %v", ErrStoreError, err)
	}
	return &JSONLStore{root: sessionLogRoot}, nil
}

// ReadsDir is the directory the cursor files live in.
func (s *JSONLStore) ReadsDir() string { return filepath.Join(s.root, readsDirName) }

// Close releases nothing: every advance opens, appends and fsyncs its own
// handle, which is what makes concurrent appenders safe on an append-only file.
func (s *JSONLStore) Close() error { return nil }

// path returns the cursor file for a session, refusing an id that is not
// path-safe. Session ids are opaque URL-safe tokens (CHAT-SESSIONS §1.3), so a
// separator or a traversal sequence is refused rather than escaped.
func (s *JSONLStore) path(sessionID string) (string, error) {
	if err := safeSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(s.ReadsDir(), sessionID+".jsonl"), nil
}

// Get returns one member's stored cursor, or ErrCursorNotFound when the member
// has never advanced (§2.5: absent = 0).
func (s *JSONLStore) Get(_ context.Context, sessionID, memberType, memberID string) (*Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursors, err := s.fold(sessionID)
	if err != nil {
		return nil, err
	}
	c, ok := cursors[memberKey{memberType: memberType, memberID: memberID}]
	if !ok {
		return nil, fmt.Errorf("%w: session %q %s %q", ErrCursorNotFound, sessionID, memberType, memberID)
	}
	return &c, nil
}

// List returns every cursor row of a session, ordered by (member_type,
// member_id). It includes rows for members that have since been removed — the
// read-state surface hides those, the store keeps the historical fact (§7).
func (s *JSONLStore) List(_ context.Context, sessionID string) ([]Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	folded, err := s.fold(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]Cursor, 0, len(folded))
	for _, c := range folded {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MemberType != out[j].MemberType {
			return out[i].MemberType < out[j].MemberType
		}
		return out[i].MemberID < out[j].MemberID
	})
	return out, nil
}

// Advance appends one cursor version and is monotonic: a value at or below the
// stored one is absorbed and the STORED cursor is returned, so two concurrent
// advances converge on the highest seq and neither errors (§2.2, §7).
func (s *JSONLStore) Advance(_ context.Context, c Cursor) (Cursor, error) {
	if c.SessionID == "" || c.MemberType == "" || c.MemberID == "" {
		return Cursor{}, fmt.Errorf("%w: cursor is missing its session or member", ErrStoreError)
	}
	if c.ReadThroughSeq < 0 {
		return Cursor{}, fmt.Errorf("%w: negative seq %d", ErrStoreError, c.ReadThroughSeq)
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	c.UpdatedAt = c.UpdatedAt.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.path(c.SessionID)
	if err != nil {
		return Cursor{}, err
	}
	stored, err := s.foldFile(p, c.SessionID)
	if err != nil {
		return Cursor{}, err
	}
	cur := stored[memberKey{memberType: c.MemberType, memberID: c.MemberID}]
	if cur.ReadThroughSeq >= c.ReadThroughSeq {
		// Monotonic: never move backwards, and an equal value is a no-op.
		return cur, nil
	}

	line, err := marshalCursorVersion(c)
	if err != nil {
		return Cursor{}, err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: open cursor log: %v", ErrStoreError, err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return Cursor{}, fmt.Errorf("%w: append cursor: %v", ErrStoreError, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Cursor{}, fmt.Errorf("%w: fsync cursor: %v", ErrStoreError, err)
	}
	if err := f.Close(); err != nil {
		return Cursor{}, fmt.Errorf("%w: close cursor log: %v", ErrStoreError, err)
	}
	return c, nil
}

// fold reads a session's cursor log and folds it to one cursor per member: the
// highest seq wins, and the timestamp of the line that carried it survives.
func (s *JSONLStore) fold(sessionID string) (map[memberKey]Cursor, error) {
	p, err := s.path(sessionID)
	if err != nil {
		return nil, err
	}
	return s.foldFile(p, sessionID)
}

func (s *JSONLStore) foldFile(p, sessionID string) (map[memberKey]Cursor, error) {
	out := map[memberKey]Cursor{}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("%w: open cursor log: %v", ErrStoreError, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4*1024), maxCursorLineBytes)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		v, err := parseCursorVersion([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrStoreError, p, lineNo, err)
		}
		if v.SessionID != sessionID {
			return nil, fmt.Errorf("%w: %s line %d names session %q, want %q",
				ErrStoreError, p, lineNo, v.SessionID, sessionID)
		}
		k := memberKey{memberType: v.MemberType, memberID: v.MemberID}
		if prev, ok := out[k]; ok && prev.ReadThroughSeq > v.ReadThroughSeq {
			continue
		}
		out[k] = Cursor{
			SessionID:      v.SessionID,
			MemberType:     v.MemberType,
			MemberID:       v.MemberID,
			ReadThroughSeq: v.ReadThroughSeq,
			UpdatedAt:      v.UpdatedAt.UTC(),
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: read cursor log: %v", ErrStoreError, err)
	}
	return out, nil
}

// marshalCursorVersion renders one cursor line (trailing newline included) —
// the only byte form this store appends.
func marshalCursorVersion(c Cursor) ([]byte, error) {
	b, err := json.Marshal(cursorVersion{
		V:              cursorFormatVersion,
		SessionID:      c.SessionID,
		MemberType:     c.MemberType,
		MemberID:       c.MemberID,
		ReadThroughSeq: c.ReadThroughSeq,
		UpdatedAt:      c.UpdatedAt.UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal cursor: %v", ErrStoreError, err)
	}
	return append(b, '\n'), nil
}

// parseCursorVersion parses and validates one cursor line. An unknown version
// or a missing identity is refused, never skipped: the log is the log.
func parseCursorVersion(line []byte) (*cursorVersion, error) {
	var v cursorVersion
	if err := json.Unmarshal(line, &v); err != nil {
		return nil, fmt.Errorf("invalid cursor line: %v", err)
	}
	if v.V != cursorFormatVersion {
		return nil, fmt.Errorf("unsupported cursor version %d (want %d)", v.V, cursorFormatVersion)
	}
	if v.SessionID == "" || v.MemberType == "" || v.MemberID == "" {
		return nil, fmt.Errorf("cursor line without session_id/member_type/member_id")
	}
	if v.ReadThroughSeq < 0 {
		return nil, fmt.Errorf("cursor line with negative read_through_seq %d", v.ReadThroughSeq)
	}
	if v.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("cursor line without updated_at")
	}
	return &v, nil
}

// safeSessionID is the filename guard for the JSONL store; it mirrors the one
// the session log applies to its own files.
func safeSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: session id is empty", ErrStoreError)
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: session id %q is not path-safe", ErrStoreError, id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("%w: session id %q contains %q", ErrStoreError, id, r)
	}
	return nil
}
