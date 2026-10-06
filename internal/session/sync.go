package session

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// This file is the log ⇄ view seam of the dual backend (CR-CHAT-006): the
// re-projection of an ordered append log into a query view, the import of a
// portable bundle, and the read-only reconciliation that compares the two
// without ever healing them. It is deliberately engine-agnostic — every
// function takes a Store, so it drives SQLite, PostgreSQL and the JSONL log
// through the same code.

// ReadBundle reads a JSONL bundle file into records, in file order. It refuses
// a malformed or unknown-version line rather than skipping it: an unknown line
// is unknown state, and unknown state must not be silently dropped from a log
// of record (§6.3 rule 5). It is the ONE bundle reader — JSONLStore.Import
// delegates here, so the log and the importers share one parser.
func ReadBundle(path string) ([]*Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session jsonl store: open bundle: %w", err)
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
			return nil, fmt.Errorf("session jsonl store: %s line %d: %v", path, lineNo, err)
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session jsonl store: read bundle: %w", err)
	}
	return recs, nil
}

// ProjectLog re-projects a log's records into a query view — the ONE allowed
// write direction of §2.2: record → JSONL line → projection → view. It reports
// how many records it applied.
//
// It is idempotent by construction: each record is appended, and a store's
// append is keep-LAST per (session_id, seq) (§5.1), so re-projecting the same
// records twice changes nothing. That is what makes a bundle import safe to
// repeat, and what lets a crashed projection be healed by re-running it.
func ProjectLog(ctx context.Context, view Store, recs []*Record) (int, error) {
	applied := 0
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if err := view.Append(ctx, rec); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// ImportBundle reads a portable JSONL bundle (§6.1) and re-projects it into the
// view, returning the number of records applied. The bundle carries the SAME
// lines as the log — it is a filtered copy, not a re-encoding — so an import is
// a replay (§4), not a translation. Because Append is keep-LAST, importing the
// same bundle into SQLite, into PostgreSQL, or twice into either yields the
// same State.
func ImportBundle(ctx context.Context, view Store, path string) (int, error) {
	recs, err := ReadBundle(path)
	if err != nil {
		return 0, err
	}
	return ProjectLog(ctx, view, recs)
}

// Reconcile is the read-only half of the storage reconciler (§5.1): it compares
// a view against the log a session reduces to and REPORTS divergence instead of
// healing it (§5.3). It returns the view's State on agreement and a nil error;
// on divergence it returns the view's State and an error naming the session and
// both canonical-JSON digests, so the caller can decide — exactly as §5.3
// requires that a mismatch be a finding and not a background sync.
func Reconcile(ctx context.Context, view Store, sessionID string, recs []*Record) (*State, error) {
	fromView, err := view.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	fromLog, err := Replay(sessionID, recs)
	if err != nil {
		return nil, err
	}
	vb, err := CanonicalJSON(fromView)
	if err != nil {
		return nil, fmt.Errorf("reconcile %q: canonical view: %w", sessionID, err)
	}
	lb, err := CanonicalJSON(fromLog)
	if err != nil {
		return nil, fmt.Errorf("reconcile %q: canonical log: %w", sessionID, err)
	}
	if string(vb) != string(lb) {
		return fromView, fmt.Errorf("reconcile %q: view and log diverge (view sha256=%s log sha256=%s)",
			sessionID, digest(vb), digest(lb))
	}
	return fromView, nil
}

// digest is the short content hash Reconcile reports, so a divergence is
// identifiable without dumping both States.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
