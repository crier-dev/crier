package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingWriter captures cards for assertions. delay slows WriteCard
// (fire-and-forget / queue tests); err makes every write fail
// (write-failure tolerance); started closes on the FIRST WriteCard entry
// (deterministic queue-full setup).
type recordingWriter struct {
	mu      sync.Mutex
	cards   []Card
	err     error
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{started: make(chan struct{})}
}

func (w *recordingWriter) WriteCard(ctx context.Context, c Card) error {
	w.once.Do(func() { close(w.started) })
	if w.delay > 0 {
		select {
		case <-time.After(w.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if w.err != nil {
		return w.err
	}
	w.mu.Lock()
	w.cards = append(w.cards, c)
	w.mu.Unlock()
	return nil
}

func (w *recordingWriter) all() []Card {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Card(nil), w.cards...)
}

func (w *recordingWriter) waitFor(t *testing.T, n int) []Card {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cards := w.all(); len(cards) >= n {
			return cards
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d cards (have %d)", n, len(w.all()))
	return nil
}

// newKanbanGuard wires a Guard with a CardWriter + worker (CR-FEAT-014).
func newKanbanGuard(t *testing.T, env envMap, m *mockLLMServer, srv *httptest.Server, w CardWriter, queueSize int) *Guard {
	t.Helper()
	capture := &auditCapture{}
	g, err := New(Options{
		Timeout:          5 * time.Second,
		MaxConcurrent:    8,
		CircuitThreshold: 10,
		MaxPayloadBytes:  65536,
		RenderMaxBytes:   32768,
		KanbanWriter:     w,
		KanbanQueueSize:  queueSize,
		LookupEnv:        env.get,
		Logf:             capture.logf,
		LogfWarn:         capture.logfWarn,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}

// kanbanPolicy wraps the shared custom provider policy with a kanban
// output config.
func kanbanPolicy(url, keyRef string, kc *KanbanConfig) *AgentGuardConfig {
	cfg := customPolicy(false, url, keyRef)
	cfg.Policies[0].Kanban = kc
	return cfg
}

const verdictBlockJSON = `{"choices":[{"message":{"content":"{\"decision\":\"block\",\"risk_level\":\"high\",\"reason\":\"injection\",\"matched_patterns\":[\"jailbreak\"]}"}}]}`
const verdictSanitizeJSON = `{"choices":[{"message":{"content":"{\"decision\":\"sanitize\",\"risk_level\":\"medium\",\"reason\":\"masquerade\",\"matched_patterns\":[\"b64_blob\"]}"}}]}`

func TestCardJSONShape(t *testing.T) {
	// Spec §8.2 Card contract + the ticket's payload excerpt: exact keys.
	c := Card{
		Title:    "[crier-guard] agent-1 block: injection",
		Assignee: "Bane",
		BoardURL: "https://crier/board",
		Verdict: Meta{
			MessageID: "m1",
			Decision:  DecisionBlock,
			RiskLevel: RiskHigh,
			Reason:    "injection",
			Patterns:  []string{"jailbreak"},
			Policy:    "test-policy",
		},
		Sender:  "s1",
		Excerpt: "{\"text\":\"hi\"}",
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"title", "assignee", "board_url", "verdict", "sender", "payload_excerpt"} {
		if _, ok := m[key]; !ok {
			t.Errorf("card missing key %q: %s", key, raw)
		}
	}
	if len(m) != 6 {
		t.Errorf("card has %d keys, want exactly 6: %s", len(m), raw)
	}
	v, ok := m["verdict"].(map[string]any)
	if !ok || v["message_id"] != "m1" || v["decision"] != "block" {
		t.Errorf("verdict = %v, want message_id m1 + decision block", m["verdict"])
	}
	// Optional fields are omitted when empty.
	raw2, _ := json.Marshal(Card{Title: "t", Verdict: Meta{Decision: DecisionAllow}})
	if strings.Contains(string(raw2), "assignee") || strings.Contains(string(raw2), "sender") {
		t.Errorf("empty optional fields must be omitted: %s", raw2)
	}
}

// fakeHermesScript writes an executable shim that records its arguments
// into a capture file (or sleeps / exits non-zero per the shim body).
func fakeHermesScript(t *testing.T, body string) (scriptPath, capturePath string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath = filepath.Join(dir, "hermes")
	capturePath = filepath.Join(dir, "captured.txt")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capturePath + "\n" + body
	if err := os.WriteFile(scriptPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	return scriptPath, capturePath
}

func readCapture(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestHermesKanbanWriter_InvokesCLI(t *testing.T) {
	script, capture := fakeHermesScript(t, "")
	w := NewHermesKanbanWriter(script)
	card := Card{
		Title:    "[crier-guard] agent-1 block: injection",
		Assignee: "Bane",
		BoardURL: "https://crier/board",
		Verdict:  Meta{MessageID: "m1", Decision: DecisionBlock, RiskLevel: RiskHigh, Reason: "injection"},
		Sender:   "s1",
		Excerpt:  "{\"text\":\"x\"}",
	}
	if err := w.WriteCard(context.Background(), card); err != nil {
		t.Fatalf("WriteCard: %v", err)
	}
	args := readCapture(t, capture)
	want := []string{"kanban", "create", card.Title, "--body"}
	if len(args) < len(want) || strings.Join(args[:len(want)], " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want prefix %v", args, want)
	}
	body := args[len(want)]
	var got Card
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("--body not card JSON: %v (%q)", err, body)
	}
	if got.Title != card.Title || got.Assignee != "Bane" || got.Verdict.MessageID != "m1" {
		t.Errorf("body card = %+v", got)
	}
	rest := args[len(want)+1:]
	if len(rest) != 3 || rest[0] != "--assignee" || rest[1] != "Bane" || rest[2] != "--json" {
		t.Errorf("tail args = %v, want --assignee Bane --json", rest)
	}
}

func TestHermesKanbanWriter_NoAssigneeOmitsFlag(t *testing.T) {
	script, capture := fakeHermesScript(t, "")
	w := NewHermesKanbanWriter(script)
	if err := w.WriteCard(context.Background(), Card{Title: "t", Verdict: Meta{Decision: DecisionAllow}}); err != nil {
		t.Fatalf("WriteCard: %v", err)
	}
	args := readCapture(t, capture)
	for _, a := range args {
		if a == "--assignee" {
			t.Fatalf("--assignee must be omitted when the card has no assignee: %v", args)
		}
	}
}

func TestHermesKanbanWriter_NonZeroExit(t *testing.T) {
	script, _ := fakeHermesScript(t, "exit 3\n")
	w := NewHermesKanbanWriter(script)
	if err := w.WriteCard(context.Background(), Card{Title: "t", Verdict: Meta{Decision: DecisionBlock}}); err == nil {
		t.Fatal("WriteCard with failing CLI: want error")
	}
}

func TestHermesKanbanWriter_ContextTimeout(t *testing.T) {
	// `exec sleep` replaces sh so CommandContext's ctx kill terminates the
	// sleeping process itself — without exec, the orphaned sleep child
	// holds the output pipes open and CombinedOutput blocks until it
	// exits.
	script, _ := fakeHermesScript(t, "exec sleep 5\n")
	w := NewHermesKanbanWriter(script)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := w.WriteCard(ctx, Card{Title: "t", Verdict: Meta{Decision: DecisionBlock}}); err == nil {
		t.Fatal("WriteCard over slow CLI: want ctx deadline error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WriteCard took %v, want prompt ctx deadline", elapsed)
	}
}

func TestKanban_OnBlockFiresForBlockAndSanitize(t *testing.T) {
	// Spec §10.1 #29: on=block fires for block/sanitize only, with exact
	// Card fields.
	kc := &KanbanConfig{Enabled: true, On: "block", Assignee: "Ops", BoardURL: "https://crier/board"}

	t.Run("block", func(t *testing.T) {
		m, srv := newMockLLM(t, 0, verdictBlockJSON)
		w := newRecordingWriter()
		g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
		res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
			Input{MessageID: "m1", Sender: "s1", Payload: []byte(`{"text":"bad"}`)})
		if err != nil || res.Decision != DecisionBlock {
			t.Fatalf("Check: %v %+v", err, res)
		}
		cards := w.waitFor(t, 1)
		c := cards[0]
		if c.Title != "[crier-guard] agent-1 block: injection" {
			t.Errorf("title = %q", c.Title)
		}
		if c.Assignee != "Ops" || c.BoardURL != "https://crier/board" {
			t.Errorf("assignee/board = %q/%q", c.Assignee, c.BoardURL)
		}
		if c.Sender != "s1" || c.Excerpt != `{"text":"bad"}` {
			t.Errorf("sender/excerpt = %q/%q", c.Sender, c.Excerpt)
		}
		v := c.Verdict
		if v.MessageID != "m1" || v.Decision != DecisionBlock || v.RiskLevel != RiskHigh ||
			v.Reason != "injection" || v.Policy != "test-policy" || len(v.Patterns) != 1 || v.Patterns[0] != "jailbreak" {
			t.Errorf("verdict = %+v", v)
		}
	})

	t.Run("sanitize", func(t *testing.T) {
		m, srv := newMockLLM(t, 0, verdictSanitizeJSON)
		w := newRecordingWriter()
		g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
		res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
			Input{MessageID: "m2", Payload: []byte(`{"data":"c2VjcmV0"}`)})
		if err != nil || res.Decision != DecisionSanitize {
			t.Fatalf("Check: %v %+v", err, res)
		}
		cards := w.waitFor(t, 1)
		if cards[0].Verdict.Decision != DecisionSanitize || cards[0].Verdict.MessageID != "m2" {
			t.Errorf("card = %+v", cards[0])
		}
	})

	t.Run("allow fires nothing", func(t *testing.T) {
		m, srv := newMockLLM(t, 0, verdictAllowJSON)
		w := newRecordingWriter()
		g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
		if _, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
			Input{MessageID: "m3", Payload: []byte(`{"text":"clean"}`)}); err != nil {
			t.Fatalf("Check: %v", err)
		}
		time.Sleep(100 * time.Millisecond) // give a stray worker a chance
		if cards := w.all(); len(cards) != 0 {
			t.Fatalf("on=block must not fire for allow: %+v", cards)
		}
	})
}

func TestKanban_OnAllFiresForEveryMessage(t *testing.T) {
	// Spec §10.1 #30: on=all fires for every guarded message.
	m, srv := newMockLLM(t, 0, verdictAllowJSON)
	w := newRecordingWriter()
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
	kc := &KanbanConfig{Enabled: true, On: "all"}
	for i := 0; i < 3; i++ {
		if _, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
			Input{MessageID: "m", Payload: []byte(`{"x":1}`)}); err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
	}
	cards := w.waitFor(t, 3)
	for i, c := range cards {
		if c.Verdict.Decision != DecisionAllow {
			t.Errorf("card %d verdict = %s", i, c.Verdict.Decision)
		}
	}
}

func TestKanban_DisabledNoCards(t *testing.T) {
	// Spec §10.1 #31: no kanban config (or enabled=false) → zero cards,
	// even with a live writer.
	m, srv := newMockLLM(t, 0, verdictBlockJSON)
	for name, kc := range map[string]*KanbanConfig{
		"absent":          nil,
		"off":             {Enabled: false, On: "all"},
		"on-but-disabled": {Enabled: false, On: "block"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newRecordingWriter()
			g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
			res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
				Input{MessageID: "m1", Payload: []byte(`{"text":"bad"}`)})
			if err != nil || res.Decision != DecisionBlock {
				t.Fatalf("Check: %v %+v", err, res)
			}
			time.Sleep(100 * time.Millisecond)
			if cards := w.all(); len(cards) != 0 {
				t.Fatalf("kanban disabled must write zero cards: %+v", cards)
			}
		})
	}
}

var errFakeWrite = errors.New("kanban: fake write failure")

func TestKanban_ErrorPathBlockFiresCard(t *testing.T) {
	// Spec §8.1: a guard-error whose error-path decision is block fires a
	// card (on=block), with errored=true on the verdict.
	w := newRecordingWriter()
	g, err := New(Options{
		Timeout:         5 * time.Second,
		MaxConcurrent:   8,
		KanbanWriter:    w,
		KanbanQueueSize: 100,
		LookupEnv:       envMap{"K": "k"}.get,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	// Fail-closed policy pointing at a dead provider → error-path block.
	cfg := customPolicy(true, "http://127.0.0.1:1", "env:K")
	cfg.Policies[0].Kanban = &KanbanConfig{Enabled: true, On: "block"}
	res, err := g.Check(context.Background(), "agent-1", cfg,
		Input{MessageID: "m1", Sender: "s1", Payload: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != DecisionBlock || !res.Errored {
		t.Fatalf("res = %+v, want fail-closed block + errored", res)
	}
	cards := w.waitFor(t, 1)
	c := cards[0]
	if !c.Verdict.Errored || c.Verdict.Decision != DecisionBlock {
		t.Errorf("card verdict = %+v, want errored block", c.Verdict)
	}
	if c.Verdict.MessageID != "m1" || c.Sender != "s1" {
		t.Errorf("card = %+v", c)
	}
}

func TestKanban_AllowErrorPathFiresNothing(t *testing.T) {
	// Fail-open guard error (decision allow) is NOT in scope for on=block.
	w := newRecordingWriter()
	g, err := New(Options{
		Timeout:         5 * time.Second,
		MaxConcurrent:   8,
		KanbanWriter:    w,
		KanbanQueueSize: 100,
		LookupEnv:       envMap{"K": "k"}.get,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	cfg := customPolicy(false, "http://127.0.0.1:1", "env:K") // fail-open
	cfg.Policies[0].Kanban = &KanbanConfig{Enabled: true, On: "block"}
	res, err := g.Check(context.Background(), "agent-1", cfg,
		Input{MessageID: "m1", Payload: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Decision != DecisionAllow || !res.Errored {
		t.Fatalf("res = %+v, want fail-open allow + errored", res)
	}
	time.Sleep(100 * time.Millisecond)
	if cards := w.all(); len(cards) != 0 {
		t.Fatalf("fail-open allow must not fire a card on on=block: %+v", cards)
	}
}

func TestKanban_DefaultAssigneeBane(t *testing.T) {
	m, srv := newMockLLM(t, 0, verdictBlockJSON)
	w := newRecordingWriter()
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
	kc := &KanbanConfig{Enabled: true} // no assignee, no on → defaults
	if _, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", kc),
		Input{MessageID: "m1", Payload: []byte(`{"text":"bad"}`)}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	cards := w.waitFor(t, 1)
	if cards[0].Assignee != "Bane" {
		t.Errorf("assignee = %q, want default Bane", cards[0].Assignee)
	}
}

func TestKanban_ExcerptTruncated(t *testing.T) {
	m, srv := newMockLLM(t, 0, verdictBlockJSON)
	w := newRecordingWriter()
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
	big := strings.Repeat("z", 1000)
	if _, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", &KanbanConfig{Enabled: true}),
		Input{MessageID: "m1", Payload: []byte(big)}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	cards := w.waitFor(t, 1)
	ex := cards[0].Excerpt
	if !strings.HasSuffix(ex, "…(truncated)") || len(ex) > kanbanExcerptBytes+len("…(truncated)") {
		t.Errorf("excerpt = %q (len %d), want truncation at %d", ex, len(ex), kanbanExcerptBytes)
	}
}

func TestKanban_WriteFailureTolerated(t *testing.T) {
	// A failing writer never fails the delivery (fire-and-forget §8.1).
	m, srv := newMockLLM(t, 0, verdictBlockJSON)
	w := newRecordingWriter()
	w.err = errFakeWrite
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
	res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", &KanbanConfig{Enabled: true}),
		Input{MessageID: "m1", Payload: []byte(`{"text":"bad"}`)})
	if err != nil {
		t.Fatalf("Check must not fail on kanban write error: %v", err)
	}
	if res.Decision != DecisionBlock {
		t.Fatalf("decision = %s, want block", res.Decision)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.Snapshot().KanbanFailed == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("kanban write failure not counted: %+v", g.Snapshot())
}

func TestKanban_FireAndForgetNonBlocking(t *testing.T) {
	// A slow writer (300ms) must not delay Check: delivery returns
	// promptly, the card lands later on the background worker.
	m, srv := newMockLLM(t, 0, verdictAllowJSON)
	w := newRecordingWriter()
	w.delay = 300 * time.Millisecond
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 100)
	start := time.Now()
	res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(srv.URL, "env:K", &KanbanConfig{Enabled: true, On: "all"}),
		Input{MessageID: "m1", Payload: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("Check blocked on the kanban writer (%v)", elapsed)
	}
	if res.Decision != DecisionAllow {
		t.Fatalf("decision = %s", res.Decision)
	}
	cards := w.waitFor(t, 1)
	if cards[0].Verdict.MessageID != "m1" {
		t.Errorf("card = %+v", cards[0])
	}
}

func TestKanban_QueueFullDrops(t *testing.T) {
	// Spec §10.1 #30: queue full → drop + counter, never blocks.
	m, srv := newMockLLM(t, 0, verdictAllowJSON)
	w := newRecordingWriter()
	w.delay = 300 * time.Millisecond
	g := newKanbanGuard(t, envMap{"K": "k"}, m, srv, w, 1)
	kc := &KanbanConfig{Enabled: true, On: "all"}
	// Card 1 enters the worker (in-flight, 300ms delay) — wait for the
	// worker to pick it so the queue is provably empty.
	if _, err := g.Check(context.Background(), "a", kanbanPolicy(srv.URL, "env:K", kc),
		Input{MessageID: "m1", Payload: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("Check 1: %v", err)
	}
	<-w.started
	// Card 2 fills the 1-slot queue.
	if _, err := g.Check(context.Background(), "a", kanbanPolicy(srv.URL, "env:K", kc),
		Input{MessageID: "m2", Payload: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("Check 2: %v", err)
	}
	// Card 3 finds the queue full → dropped, and Check still returns.
	if _, err := g.Check(context.Background(), "a", kanbanPolicy(srv.URL, "env:K", kc),
		Input{MessageID: "m3", Payload: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("Check 3: %v", err)
	}
	if got := g.Snapshot().KanbanDropped; got != 1 {
		t.Fatalf("KanbanDropped = %d, want 1", got)
	}
	cards := w.waitFor(t, 2)
	if cards[0].Verdict.MessageID != "m1" || cards[1].Verdict.MessageID != "m2" {
		t.Errorf("written cards = %+v, want m1, m2", cards)
	}
	if got := g.Snapshot().KanbanWritten; got != 2 {
		t.Errorf("KanbanWritten = %d, want 2", got)
	}
}
