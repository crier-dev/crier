package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxRecordLineBytes bounds one JSONL line. A payload is opaque and may carry
// an asset REFERENCE but never bytes (§3.10 / D9), so a line is small; the cap
// exists so a corrupt file cannot be read into unbounded memory.
const maxRecordLineBytes = 4 << 20

// bundleSuffix is the per-session file name inside a bundle directory
// (CHAT-STORAGE.md §6.1's one-file-per-kind layout, re-sharded per session as
// the spec's §7.1 proposed default for the transport form).
const bundleSuffix = ".jsonl"

// JSONLStore is the JSONL half of the dual backend: the ORDERED APPEND LOG and
// the transport form (§2.1, §5.1). One record-version per line, `v` first,
// append-only, never rewritten — the log is the log of record.
//
// It is a first-class peer, not an export: a session can be written to disk,
// shipped anywhere and re-imported into a fresh instance by replaying the
// lines, with no distributed transaction (§1, §4).
type JSONLStore struct {
	root string
}

var _ Store = (*JSONLStore)(nil)

// NewJSONLStore opens (creating when absent) a log root directory. One file
// per session lives under it: <root>/<session_id>.jsonl.
func NewJSONLStore(root string) (*JSONLStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: log root is empty", ErrInvalidRecord)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("session jsonl store: create root: %w", err)
	}
	return &JSONLStore{root: root}, nil
}

// Root returns the log root the store appends to.
func (s *JSONLStore) Root() string { return s.root }

// Close releases nothing today (every append opens and fsyncs its own handle,
// which is what makes concurrent appenders safe on an append-only file); it
// exists so JSONLStore satisfies Store alongside PostgresStore.
func (s *JSONLStore) Close() error { return nil }

// path returns the log file for a session, refusing an id that is not
// path-safe (the spec fixes session ids as opaque URL-safe tokens).
func (s *JSONLStore) path(sessionID string) (string, error) {
	id, err := trimSessionID(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, id+bundleSuffix), nil
}

// Append writes ONE record-version line and fsyncs it before returning
// (§2.3). The line is appended, never merged: the log keeps every version and
// keep-LAST is applied on read.
func (s *JSONLStore) Append(_ context.Context, rec *Record) error {
	line, err := rec.MarshalLine()
	if err != nil {
		return err
	}
	p, err := s.path(rec.SessionID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("session jsonl store: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session jsonl store: fsync: %w", err)
	}
	return nil
}

// Records returns a session's records ordered by seq, with keep-LAST per
// (session_id, seq) applied (§5.1): a later line with the same ordering key
// wins, an identical duplicate is a no-op.
func (s *JSONLStore) Records(_ context.Context, sessionID string) ([]*Record, error) {
	p, err := s.path(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
		}
		return nil, fmt.Errorf("session jsonl store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLineBytes)

	lastIdx := map[int64]int{}
	var kept []*Record
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rec, err := ParseRecord([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrInvalidRecord, p, lineNo, err)
		}
		if rec.SessionID != sessionID {
			return nil, fmt.Errorf("%w: %s line %d names session %q, want %q",
				ErrInvalidRecord, p, lineNo, rec.SessionID, sessionID)
		}
		if idx, ok := lastIdx[rec.Seq]; ok {
			kept[idx] = rec
			continue
		}
		lastIdx[rec.Seq] = len(kept)
		kept = append(kept, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session jsonl store: read log: %w", err)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })
	return kept, nil
}

// Load reduces the log to the session State — the same State the PostgreSQL
// view assembles, which is the round-trip acceptance of CR-CHAT-002.
func (s *JSONLStore) Load(ctx context.Context, sessionID string) (*State, error) {
	recs, err := s.Records(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return Replay(sessionID, recs)
}

// Sessions lists the session ids present in the log root, sorted. It is what
// makes a whole-realm bundle export enumerable without a side index, and the
// enumeration GET /sessions is a view over (CR-CHAT-019). The context is
// accepted so the signature is one across all three backends (the SQL stores
// need it); this store's read is local and ignores it.
func (s *JSONLStore) Sessions(ctx context.Context) ([]string, error) {
	_ = ctx
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("session jsonl store: read root: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), bundleSuffix) {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), bundleSuffix))
	}
	sort.Strings(ids)
	return ids, nil
}

// Export writes one session's log as a self-contained bundle file
// <dir>/<session_id>.jsonl and returns its path. The bundle carries the SAME
// lines as the log — it is a filtered copy, not a re-encoding — which is what
// makes import a replay (§4) rather than a translation (CHAT-STORAGE.md §6.1).
func (s *JSONLStore) Export(ctx context.Context, sessionID, dir string) (string, error) {
	recs, err := s.Records(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("session jsonl store: create bundle dir: %w", err)
	}
	name, err := trimSessionID(sessionID)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, name+bundleSuffix)
	var buf strings.Builder
	for _, rec := range recs {
		line, err := rec.MarshalLine()
		if err != nil {
			return "", err
		}
		buf.Write(line)
	}
	if err := os.WriteFile(dst, []byte(buf.String()), 0o644); err != nil {
		return "", fmt.Errorf("session jsonl store: write bundle: %w", err)
	}
	return dst, nil
}

// Import reads a bundle file back into records, in file order. It delegates to
// the shared ReadBundle (sync.go) so the log's importer and the SQL view's
// bundle importer are ONE parser: an unknown line is unknown state and is
// refused rather than skipped (§6.3 rule 5).
func (s *JSONLStore) Import(path string) ([]*Record, error) {
	return ReadBundle(path)
}

// LoadBundle imports a bundle file and reduces it to the session State, so a
// bundle that travelled to a fresh instance can be read without first being
// re-imported into a log root.
func (s *JSONLStore) LoadBundle(path string) (*State, error) {
	recs, err := s.Import(path)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: bundle %s is empty", ErrInvalidRecord, path)
	}
	return Replay(recs[0].SessionID, recs)
}

// ---------------------------------------------------------------------------
// Session CRUD + member management over the LOG — the same method set
// PostgresStore exposes, so a caller can drive either backend identically.
//
// Unlike the query view, the log CAN compute the next seq from its own record
// stream (NextSeq), so a caller that wants the store to be its allocator uses
// NextSeq; a caller that is already the single appender passes its own.
// ---------------------------------------------------------------------------

// NextSeq returns the seq the next record for a session should carry: the
// highest seq in the log plus one, or 1 when the session has no log yet.
func (s *JSONLStore) NextSeq(ctx context.Context, sessionID string) (int64, error) {
	recs, err := s.Records(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return 1, nil
		}
		return 0, err
	}
	return NextSeq(recs), nil
}

// CreateSession appends the session.create record.
func (s *JSONLStore) CreateSession(ctx context.Context, sess *Session, seq int64) error {
	return s.Append(ctx, sess.CreateRecord(seq, sess.CreatedAt))
}

// Session reduces the log to one session object.
func (s *JSONLStore) Session(ctx context.Context, id string) (*Session, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Session, nil
}

// AddMember records a join, with its late-join context answer when there is one.
func (s *JSONLStore) AddMember(ctx context.Context, m *Member, seq int64, share *ContextShare) error {
	return s.Append(ctx, m.AddRecord(seq, m.AddedAt, share))
}

// SetMemberContext records a later change to a member's context share.
func (s *JSONLStore) SetMemberContext(ctx context.Context, mc *MemberContext, seq int64) error {
	return s.Append(ctx, mc.Record(seq, mc.SetAt))
}

// RemoveMember records a removal.
func (s *JSONLStore) RemoveMember(ctx context.Context, m *Member, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, m.RemoveRecord(seq, at, actor, reason))
}

// PostMessage appends a message record (root or reply).
func (s *JSONLStore) PostMessage(ctx context.Context, m *Message) error {
	return s.Append(ctx, m.Record())
}

// Branch records a deliberate branch — the only new-thread_id record.
func (s *JSONLStore) Branch(ctx context.Context, t *Thread, seq int64, reason string) error {
	return s.Append(ctx, t.BranchRecord(seq, t.CreatedAt, reason))
}

// CloseSession appends a session.close record.
func (s *JSONLStore) CloseSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef, reason string) error {
	return s.Append(ctx, CloseRecord(id, seq, at, actor, reason))
}

// ReopenSession appends a session.reopen record.
func (s *JSONLStore) ReopenSession(ctx context.Context, id string, seq int64, at time.Time, actor AuthorRef) error {
	return s.Append(ctx, ReopenRecord(id, seq, at, actor))
}

// Members returns the session's membership.
func (s *JSONLStore) Members(ctx context.Context, id string) ([]*Member, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Members, nil
}

// Messages returns the transcript in seq order.
func (s *JSONLStore) Messages(ctx context.Context, id string) ([]*Message, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Messages, nil
}

// Threads returns the thread tree.
func (s *JSONLStore) Threads(ctx context.Context, id string) ([]*Thread, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Threads, nil
}

// ContextShares returns the latest context-share row per member.
func (s *JSONLStore) ContextShares(ctx context.Context, id string) ([]*MemberContext, error) {
	st, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.ContextShares, nil
}
