package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// Card is the kanban card payload (spec §8.2, CR-FEAT-014). The guard
// builds it from the verdict + policy kanban config; a CardWriter (the
// CR-FEAT-009 kanban-write capability) persists it.
type Card struct {
	// Title is "[crier-guard] <target_agent> <decision>: <reason[:80]>".
	Title string `json:"title"`
	// Assignee is policy.kanban.assignee — falls back to "Bane" (the
	// operator's default) when the policy leaves it empty.
	Assignee string `json:"assignee,omitempty"`
	// BoardURL is policy.kanban.board_url — the originating project board
	// link (e.g. the crier board path) for the card.
	BoardURL string `json:"board_url,omitempty"`
	// Verdict is the full guard metadata for the message (message id via
	// Meta.MessageID, spec §8.2).
	Verdict Meta `json:"verdict"`
	// Sender is the delivering agent.
	Sender string `json:"sender,omitempty"`
	// Excerpt is the payload excerpt (truncated) — ticket CR-FEAT-014 card
	// content. Bounded by kanbanExcerptBytes; never the full payload.
	Excerpt string `json:"payload_excerpt,omitempty"`
}

// CardWriter is the CR-FEAT-009 kanban-write interface the guard depends
// on (spec §8.2). Implementations: the Hermes kanban card writer (shipped
// with CR-FEAT-014, writes via the `hermes kanban create` CLI); a no-op /
// HTTP writer can be dropped in by CR-FEAT-009 without touching the
// guard. The guard treats a nil writer as kanban disabled.
type CardWriter interface {
	WriteCard(ctx context.Context, c Card) error
}

// HermesKanbanWriter writes cards through the Hermes kanban CLI — the
// working kanban write path on this machine (`hermes kanban create`).
// command is the executable path (default "hermes"); the per-card
// deadline comes from ctx (the guard worker applies a 10s timeout, spec
// §8.2). Write failures are returned to the caller, which logs + counts
// them — they never fail the message delivery (fire-and-forget, §8.1).
//
// CR-FEAT-009 integration point: replace this writer with the real
// kanban-write capability (or an HTTP writer POSTing card JSON to
// CR_GUARD_KANBAN_URL) via the same CardWriter interface — nothing else
// in the guard changes.
type HermesKanbanWriter struct {
	command string
}

// NewHermesKanbanWriter builds a CLI-backed CardWriter. An empty command
// defaults to "hermes" (resolved via PATH at call time).
func NewHermesKanbanWriter(command string) *HermesKanbanWriter {
	if command == "" {
		command = "hermes"
	}
	return &HermesKanbanWriter{command: command}
}

// WriteCard runs:
//
//	<command> kanban create <title> --body <card JSON> --assignee <assignee> --json
//
// The full card object rides in --body so the card is self-describing
// (verdict, board link, excerpt); the CLI's own title field gets the card
// title. Non-zero exit / spawn failure / ctx deadline → error.
func (w *HermesKanbanWriter) WriteCard(ctx context.Context, c Card) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("kanban: marshal card: %w", err)
	}
	args := []string{"kanban", "create", c.Title, "--body", string(body)}
	if c.Assignee != "" {
		args = append(args, "--assignee", c.Assignee)
	}
	args = append(args, "--json")
	cmd := exec.CommandContext(ctx, w.command, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kanban: %w: %s", err, truncateBody(out))
	}
	return nil
}

// kanbanWriteTimeout bounds each WriteCard call (spec §8.2: 10s per card).
const kanbanWriteTimeout = 10 * time.Second

// kanbanExcerptBytes caps the payload excerpt on a card (ticket: payload
// excerpt, truncated).
const kanbanExcerptBytes = 512

// kanbanWorker is the bounded fire-and-forget card writer (spec §8.2): a
// buffered queue + one draining goroutine. enqueue never blocks — a full
// queue drops the card (log + counter). WriteCard runs with a 10s per-card
// timeout; failures are logged + counted, never surfaced to the delivery
// path. close() stops the loop after the in-flight write; the queue
// channel is never closed, so enqueue can never panic.
type kanbanWorker struct {
	writer    CardWriter
	queue     chan Card
	quit      chan struct{}
	done      chan struct{}
	logf      func(msg string, args ...any)
	logfWarn  func(msg string, args ...any)
	closeOnce sync.Once

	mu      sync.Mutex
	written int64
	dropped int64
	failed  int64
}

func newKanbanWorker(writer CardWriter, queueSize int, logf, logfWarn func(msg string, args ...any)) *kanbanWorker {
	if queueSize <= 0 {
		queueSize = 100
	}
	w := &kanbanWorker{
		writer:   writer,
		queue:    make(chan Card, queueSize),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		logf:     logf,
		logfWarn: logfWarn,
	}
	go w.run()
	return w
}

// enqueue hands a card to the worker without blocking (spec §8.1/§8.2).
// Returns false when the queue is full (card dropped + counted) or the
// worker has been stopped. Never blocks, never panics.
func (w *kanbanWorker) enqueue(c Card) bool {
	select {
	case w.queue <- c:
		return true
	default:
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
		if w.logfWarn != nil {
			w.logfWarn("guard kanban", "event", "kanban_queue_full", "title", c.Title)
		}
		return false
	}
}

// run drains the queue until quit is closed. The in-flight write always
// completes (and respects kanbanWriteTimeout) before the loop exits.
func (w *kanbanWorker) run() {
	defer close(w.done)
	for {
		select {
		case c := <-w.queue:
			w.write(c)
		case <-w.quit:
			return
		}
	}
}

func (w *kanbanWorker) write(c Card) {
	ctx, cancel := context.WithTimeout(context.Background(), kanbanWriteTimeout)
	defer cancel()
	if err := w.writer.WriteCard(ctx, c); err != nil {
		w.mu.Lock()
		w.failed++
		w.mu.Unlock()
		if w.logfWarn != nil {
			w.logfWarn("guard kanban", "event", "kanban_write_failed", "title", c.Title, "error", err)
		}
		return
	}
	w.mu.Lock()
	w.written++
	w.mu.Unlock()
	if w.logf != nil {
		w.logf("guard kanban", "event", "kanban_card_written", "title", c.Title)
	}
}

// close stops the worker. Idempotent; safe to call from server shutdown
// and test cleanup. Queued cards behind the in-flight write may be
// dropped (fire-and-forget — the process is stopping anyway).
func (w *kanbanWorker) close() {
	w.closeOnce.Do(func() {
		close(w.quit)
		<-w.done
	})
}

func (w *kanbanWorker) stats() (written, dropped, failed int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written, w.dropped, w.failed
}

// maybeEnqueueCard implements the spec §8.1 output option: a card is
// enqueued when the matched policy enables kanban and the verdict is in
// scope (kanban.on "block" → block/sanitize/guard-error-block; "all" →
// every guarded message). Enqueue is async and never blocks delivery.
func (g *Guard) maybeEnqueueCard(agentID string, in Input, policy Policy, res Result) {
	if g.kw == nil || policy.Kanban == nil || !policy.Kanban.Enabled {
		return
	}
	on := policy.Kanban.On
	if on == "" {
		on = "block" // kanban.on default (spec §8.1)
	}
	fire := on == "all"
	if !fire {
		switch {
		case res.Decision == DecisionBlock, res.Decision == DecisionSanitize:
			fire = true
		case res.Errored && res.Decision == DecisionBlock:
			// Guard-error whose error-path decision is block (§8.1).
			fire = true
		}
	}
	if !fire {
		return
	}
	assignee := policy.Kanban.Assignee
	if assignee == "" {
		assignee = "Bane" // ticket: assignee default
	}
	meta := res.Meta()
	g.kw.enqueue(Card{
		Title:    kanbanTitle(agentID, res),
		Assignee: assignee,
		BoardURL: policy.Kanban.BoardURL,
		Verdict:  meta,
		Sender:   in.Sender,
		Excerpt:  payloadExcerpt(in.Payload),
	})
}

// kanbanTitle builds the spec §8.2 title:
// "[crier-guard] <target_agent> <decision>: <reason[:80]>".
func kanbanTitle(agentID string, res Result) string {
	return fmt.Sprintf("[crier-guard] %s %s: %s", agentID, res.Decision, truncateRunes(res.Reason, 80))
}

// truncateRunes truncates s to at most n runes, appending "…" when cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// payloadExcerpt bounds the card's payload excerpt (ticket: truncated).
func payloadExcerpt(p []byte) string {
	if len(p) <= kanbanExcerptBytes {
		return string(p)
	}
	return string(p[:kanbanExcerptBytes]) + "…(truncated)"
}
