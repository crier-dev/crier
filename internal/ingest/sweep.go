// sweep.go — the CRIER-GITHOOK-DISPATCH §10 reconciliation sweep (INT-015).
//
// Hooks are BEST-EFFORT intake only (spec §10 amendment 1): a fast-forward
// pull, a rebase, a spool loss or a crashed hook all miss the post-commit/
// post-merge fire. This file is the durable path: on a cadence crier reads the
// registered board's row set and diffs it against its own delivery ledger,
// enqueueing every eligible row whose logical event key has never been seen.
// The catch-all makes every hook miss non-fatal: a row is delivered within one
// sweep interval even if hooks NEVER fire.
//
// This file is the falsifier's stash target: removing it leaves SweepFunc nil
// (the seam lives in handler.go) and the hook-less delivery guarantee gone —
// the falsifier test times out against this pre-change state.
package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBoardctlSourceID is the ingest source the sweep reconciles against —
// the same source id the boardctl dispatch hooks POST to (spec §6).
const DefaultBoardctlSourceID = "boardctl"

// DefaultSweepInterval is the spec §10 cadence: 10 minutes.
const DefaultSweepInterval = 10 * time.Minute

func init() { SweepFunc = sweepOnce }

// BoardRow is the slice of a tasks.jsonl row the sweep needs: the row id, the
// delivery eligibility inputs and the exact content the logical event key
// hashes (spec §10 amendment 3: `boardctl:<row-id>:<content-hash>`).
type BoardRow struct {
	ID       string
	Pending  bool
	Fixture  bool
	contents map[string]any
}

// ContentHash is the hex content-hash component of the logical event key.
func (r BoardRow) ContentHash() string {
	sum := sha256.Sum256(r.canonical())
	return hex.EncodeToString(sum[:])
}

// LogicalEventKey is the ONE dedup key shared by hook intake and the sweep
// (§10 amendment 3). The event verb is deliberately absent — SEEN and ADDED
// for the same row collapse into one event. A rebase that changes the row id
// dedupes on the content hash (the clone's key hashes the same content); a
// same-id content edit produces a NEW key and is delivered, not suppressed.
func (r BoardRow) LogicalEventKey() string {
	return "boardctl:" + r.ID + ":" + r.ContentHash()
}

// canonical is the content hash input: the full row re-marshaled from its
// parsed form, which sorts map keys — so reformatting or key reordering of the
// same content hashes identically, while any content change rehashes. The row
// id is EXCLUDED: the id is the key's middle component, and §10 amendment 3
// requires a rebase that rewrites it to still dedupe on content. Every other
// field — title, priority, acceptance_criteria, status — is content: a same-id
// edit to any of them rehashes and re-delivers.
func (r BoardRow) canonical() []byte {
	contents := make(map[string]any, len(r.contents))
	for k, v := range r.contents {
		if k == "id" {
			continue
		}
		contents[k] = v
	}
	b, err := json.Marshal(contents)
	if err != nil {
		// json.Marshal of a map[string]any decoded from JSON cannot fail.
		return nil
	}
	return b
}

// ReadBoardRows parses the registered board's row set: JSONL under
// <board_dir>/.coding-hermes/board/tasks.jsonl (the same board_dir the
// boardbridge transport is configured with). Unparsable lines are skipped —
// the board's own lint owns those warnings; the sweep only needs eligible
// rows. A row is eligible when status is "pending" and it is not the fixtures
// header row (the same eligibility the hook detection uses, spec R8).
func ReadBoardRows(boardDir string) ([]BoardRow, error) {
	path := filepath.Join(boardDir, ".coding-hermes", "board", "tasks.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read board rows: %w", err)
	}
	var rows []BoardRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var contents map[string]any
		if err := json.Unmarshal([]byte(line), &contents); err != nil {
			continue
		}
		id, _ := contents["id"].(string)
		if id == "" {
			continue
		}
		status, _ := contents["status"].(string)
		rows = append(rows, BoardRow{
			ID:       id,
			Pending:  status == "pending",
			Fixture:  id == "fixtures",
			contents: contents,
		})
	}
	return rows, nil
}

// sweepOnce is one reconciliation pass: diff board-vs-ledger, enqueue and
// deliver every row whose logical event key has never been seen. Returns the
// number of rows enqueued THIS pass. Failures per row are recorded through the
// handler's dead-letter bookkeeping and joined; the pass never aborts on a
// single bad row.
func sweepOnce(ctx context.Context, h *Handler, boardDir, sourceID string) (int, error) {
	rows, err := ReadBoardRows(boardDir)
	if err != nil {
		return 0, err
	}
	source, err := h.Store.Source(ctx, sourceID)
	if err != nil {
		// Visible and fail-closed (spec §6: no source ⇒ nothing delivers);
		// the sweep keeps its cadence so a late registration heals itself.
		return 0, fmt.Errorf("reconciliation sweep: source %q not registered: %w", sourceID, err)
	}
	enqueued := 0
	var failures []error
	for _, row := range rows {
		if !row.Pending || row.Fixture {
			continue
		}
		key := row.LogicalEventKey()
		seen, err := h.Store.SeenEventKey(ctx, sourceID, key)
		if err == nil && !seen {
			// Rebase dedup (§10 amendment 3): a row-id rewrite re-queues a
			// clone whose content hashes identically — absorb it.
			seen, err = h.Store.SeenContentHash(ctx, sourceID, row.ContentHash())
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("dedup probe %s: %w", row.ID, err))
			continue
		}
		if seen {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"id":            row.ID,
			"board_dir":     boardDir,
			"event":         "row_added",
			"discovered_by": "reconciliation-sweep",
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("encode row %s: %w", row.ID, err))
			continue
		}
		messageID, err := h.newID()
		if err != nil {
			failures = append(failures, fmt.Errorf("message id for row %s: %w", row.ID, err))
			continue
		}
		nonceBytes := make([]byte, 16)
		if _, err := rand.Read(nonceBytes); err != nil {
			failures = append(failures, fmt.Errorf("nonce for row %s: %w", row.ID, err))
			continue
		}
		duplicate, acceptedID, err := h.Store.Accept(ctx, sourceID, key, base64.RawURLEncoding.EncodeToString(nonceBytes), messageID, payload, h.now())
		if err != nil {
			failures = append(failures, fmt.Errorf("durable accept %s: %w", row.ID, err))
			continue
		}
		if duplicate {
			// Raced with a hook POST of the same logical key between the
			// probe and the accept: the row is queued exactly once.
			continue
		}
		if err := h.deliver(ctx, source, acceptedID, key, payload); err != nil {
			failures = append(failures, fmt.Errorf("deliver %s: %w", row.ID, err))
			continue
		}
		enqueued++
	}
	return enqueued, errors.Join(failures...)
}

// Reconciler runs the sweep on a cadence. It is the only thing main.go wires;
// the diff logic above is package-level so the tests can drive single passes.
type Reconciler struct {
	Handler  *Handler
	BoardDir string
	SourceID string
	// Interval is the sweep cadence. Zero resolves to DefaultSweepInterval
	// (spec §10: 10m); config resolves the env default before wiring.
	Interval time.Duration
}

func (r *Reconciler) interval() time.Duration {
	if r.Interval <= 0 {
		return DefaultSweepInterval
	}
	return r.Interval
}

// Run sweeps on the cadence until the context is canceled. A failed pass is
// logged and retried on the next tick — an unreadable board or an unregistered
// source must never stop the sweep from trying again within one interval.
func (r *Reconciler) Run(ctx context.Context) {
	sourceID := r.SourceID
	if sourceID == "" {
		sourceID = DefaultBoardctlSourceID
	}
	ticker := time.NewTicker(r.interval())
	defer ticker.Stop()
	slog.Info("reconciliation sweep armed", "board_dir", r.BoardDir, "source", sourceID, "interval", r.interval().String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enqueued, err := sweepOnce(ctx, r.Handler, r.BoardDir, sourceID)
			if err != nil {
				slog.Error("reconciliation sweep", "error", err)
				continue
			}
			if enqueued > 0 {
				slog.Info("reconciliation sweep enqueued rows", "count", enqueued)
			}
		}
	}
}
