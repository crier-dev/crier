package guard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPKanbanWriter_ValidatesURL(t *testing.T) {
	// CR-FEAT-009: http/https absolute URLs construct; anything else
	// fails at construction (fail fast, spec §9.1) — the caller must not
	// discover a misconfigured sink on the first blocked message.
	valid := []string{
		"http://localhost:18774/kanban",
		"https://kanban.example.com/cards",
		"http://127.0.0.1:8080",
		"http://localhost",
	}
	for _, u := range valid {
		if w := NewHTTPKanbanWriter(u); w == nil {
			t.Errorf("NewHTTPKanbanWriter(%q) = nil, want writer", u)
		}
	}
	invalid := []string{
		"", "ftp://cards.example.com/x", "not-a-url", "://bad", "http://",
		"https://", "file:///tmp/cards", "mailto:ops@example.com",
	}
	for _, u := range invalid {
		if w := NewHTTPKanbanWriter(u); w != nil {
			t.Errorf("NewHTTPKanbanWriter(%q) = %v, want nil", u, w)
		}
	}
}

func TestHTTPKanbanWriter_PostsCard(t *testing.T) {
	// Happy path (spec §8.2): 2xx → nil error; the sink received a POST
	// with Content-Type application/json whose body decodes to the card
	// JSON — exact fields, not a wrapper.
	var gotMethod, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewHTTPKanbanWriter(srv.URL)
	if w == nil {
		t.Fatal("NewHTTPKanbanWriter: nil")
	}
	card := Card{
		Title:    "[crier-guard] agent-1 block: injection",
		Assignee: "Bane",
		BoardURL: "https://crier/board",
		Verdict: Meta{
			MessageID: "m1",
			Decision:  DecisionBlock,
			RiskLevel: RiskHigh,
			Reason:    "injection",
			Patterns:  []string{"jailbreak"},
		},
		Sender:  "s1",
		Excerpt: `{"text":"bad"}`,
	}
	if err := w.WriteCard(context.Background(), card); err != nil {
		t.Fatalf("WriteCard: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var got Card
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("body is not card JSON: %v (%q)", err, gotBody)
	}
	if got.Title != card.Title || got.Assignee != "Bane" || got.BoardURL != card.BoardURL ||
		got.Sender != "s1" || got.Excerpt != card.Excerpt {
		t.Errorf("card = %+v", got)
	}
	v := got.Verdict
	if v.MessageID != "m1" || v.Decision != DecisionBlock || v.RiskLevel != RiskHigh ||
		v.Reason != "injection" || len(v.Patterns) != 1 || v.Patterns[0] != "jailbreak" {
		t.Errorf("verdict = %+v", v)
	}
}

func TestHTTPKanbanWriter_Non2xx(t *testing.T) {
	// A 500 from the sink is an error (fire-and-forget callers log + count
	// it) — never a silent success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "sink exploded")
	}))
	defer srv.Close()
	w := NewHTTPKanbanWriter(srv.URL)
	err := w.WriteCard(context.Background(), Card{Title: "t", Verdict: Meta{Decision: DecisionBlock}})
	if err == nil {
		t.Fatal("WriteCard against 500 sink: want error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "sink exploded") {
		t.Errorf("error = %q, want status + body excerpt", err)
	}
}

func TestHTTPKanbanWriter_NetworkError(t *testing.T) {
	// A closed server → connection refused → error, not a hang or a
	// silently dropped card.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	w := NewHTTPKanbanWriter(url)
	if err := w.WriteCard(context.Background(), Card{Title: "t", Verdict: Meta{Decision: DecisionBlock}}); err == nil {
		t.Fatal("WriteCard against closed sink: want error")
	}
}

func TestHTTPKanbanWriter_ContextDeadline(t *testing.T) {
	// A sink that never responds: the ctx deadline (the guard worker's
	// kanbanWriteTimeout is the per-card budget) must cut the request off
	// promptly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	w := NewHTTPKanbanWriter(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := w.WriteCard(ctx, Card{Title: "t", Verdict: Meta{Decision: DecisionBlock}}); err == nil {
		t.Fatal("WriteCard over slow sink: want ctx deadline error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WriteCard took %v, want prompt ctx deadline", elapsed)
	}
}

func TestHTTPKanbanWriter_ThroughWorker(t *testing.T) {
	// The HTTP writer is drop-in through the CardWriter interface: the
	// guard worker writes a card to the sink for a blocked message, and a
	// write failure is counted, not surfaced to the delivery path.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, llmSrv := newMockLLM(t, 0, verdictBlockJSON)
	g := newKanbanGuard(t, envMap{"K": "k"}, m, llmSrv, NewHTTPKanbanWriter(srv.URL), 100)
	kc := &KanbanConfig{Enabled: true, On: "block"}
	res, err := g.Check(context.Background(), "agent-1", kanbanPolicy(llmSrv.URL, "env:K", kc),
		Input{MessageID: "m1", Sender: "s1", Payload: []byte(`{"text":"bad"}`)})
	if err != nil || res.Decision != DecisionBlock {
		t.Fatalf("Check: %v %+v", err, res)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(gotBody) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var card Card
	if err := json.Unmarshal(gotBody, &card); err != nil {
		t.Fatalf("worker body is not card JSON: %v (%q)", err, gotBody)
	}
	if card.Title != "[crier-guard] agent-1 block: injection" || card.Verdict.MessageID != "m1" {
		t.Errorf("worker card = %+v", card)
	}
}
