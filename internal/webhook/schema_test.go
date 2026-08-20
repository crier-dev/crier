package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- schema templates (CR-FEAT-003) ----------

func TestResolveTemplate_DefaultsAndCustom(t *testing.T) {
	// Default = generic-custom passthrough.
	if got := ResolveTemplate(&Config{URL: "http://x"}); got.Name != "generic-custom" {
		t.Fatalf("default template = %s", got.Name)
	}
	// Named.
	if got := ResolveTemplate(&Config{URL: "http://x", SchemaTemplate: "openai-compatible"}); got.Name != "openai-compatible" {
		t.Fatalf("named template = %s", got.Name)
	}
	// Custom wins.
	custom := &CustomSchema{ResponseMap: "reply.text", RequestShape: &RequestShape{Body: json.RawMessage(`{"x":"{{payload.text}}"}`)}}
	if got := ResolveTemplate(&Config{URL: "http://x", CustomSchema: custom}); got.ResponseMap != "reply.text" {
		t.Fatalf("custom response map lost")
	}
	// Unknown name falls back to generic-custom.
	if got := ResolveTemplate(&Config{URL: "http://x", SchemaTemplate: "nope"}); got.Name != "generic-custom" {
		t.Fatalf("unknown template = %s", got.Name)
	}
}

func TestTemplate_BuildBody_Expansion(t *testing.T) {
	tpl := openAICompatible
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m1", SessionID: "s1", Sender: "a"},
		Payload: json.RawMessage(`{"text":"hello","n":42}`),
	}
	body, err := tpl.BuildBody(&Config{URL: "http://x", SchemaTemplate: "openai-compatible"}, env)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	msgs := v["messages"].([]any)
	content := msgs[0].(map[string]any)["content"]
	if content != "hello" {
		t.Fatalf("content = %v", content)
	}
	if v["stream"] != false {
		t.Fatalf("stream = %v", v["stream"])
	}
	if _, ok := v["model"]; !ok {
		t.Fatal("model missing (default should apply)")
	}
}

func TestTemplate_BuildBody_Passthrough(t *testing.T) {
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message"}, Payload: json.RawMessage(`{"a":1}`)}
	body, err := genericCustom.BuildBody(&Config{URL: "http://x"}, env)
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Crier.MessageID != "m1" {
		t.Fatalf("passthrough lost envelope: %s", body)
	}
}

func TestTemplate_ExtractReply(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"the answer"}}]}`)
	got, err := openAICompatible.ExtractReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `"the answer"` {
		t.Fatalf("reply = %q (want quoted JSON string)", got)
	}
	// raw passthrough of JSON bodies
	raw, _ := genericCustom.ExtractReply(body)
	if string(raw) != string(body) {
		t.Fatalf("raw reply changed")
	}
	// raw wraps non-JSON text in a JSON string
	wrapped, _ := genericCustom.ExtractReply([]byte("plain text"))
	if string(wrapped) != `"plain text"` {
		t.Fatalf("wrapped = %q", wrapped)
	}
	// missing path errors
	if _, err := openAICompatible.ExtractReply([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("expected error for missing path")
	}
}

// ---------- blocking delivery (CR-FEAT-002) ----------

func TestDriver_DeliverBlocking_RoundTrip(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"plot B is pepper"}}]}`))
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, SchemaTemplate: "openai-compatible", DeliveryMode: "blocking"}
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "req-1", Kind: "message", SessionID: "sess-x"},
		Payload: json.RawMessage(`{"text":"what is in plot B?"}`),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := d.DeliverBlocking(ctx, "agent-b", cfg, env, 5*time.Second)
	if err != nil {
		t.Fatalf("blocking deliver: %v", err)
	}
	if string(reply) != `"plot B is pepper"` {
		t.Fatalf("reply = %q (want quoted JSON string)", reply)
	}
	// The request body went through the schema template, not the envelope.
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["messages"]; !ok {
		t.Fatalf("request body did not use openai template: %s", gotBody)
	}
	if _, ok := sent["session_id"]; ok { // openai-compatible has no session map
		t.Fatalf("unexpected session in body")
	}
}

func TestDriver_DeliverBlocking_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second) // hang past the budget
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking"}
	d := NewDriver(NewClient(500*time.Millisecond, nil), NewMemoryQueue(), DefaultDriverConfig())
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m", Kind: "message"}, Payload: json.RawMessage(`{}`)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := d.DeliverBlocking(ctx, "agent-b", cfg, env, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDriver_DeliverBlocking_RetriesWithinBudget(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("finally"))
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking"}
	d := NewDriver(NewClient(500*time.Millisecond, nil), NewMemoryQueue(), DefaultDriverConfig())
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m", Kind: "message"}, Payload: json.RawMessage(`{}`)}

	reply, err := d.DeliverBlocking(context.Background(), "agent-b", cfg, env, 5*time.Second)
	if err != nil {
		t.Fatalf("blocking deliver: %v", err)
	}
	if string(reply) != `"finally"` {
		t.Fatalf("reply = %q", reply)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", n.Load())
	}
}

// ---------- session FIFO (CR-FEAT-004) ----------

func TestSessionGate_SerializesSameSession(t *testing.T) {
	g := newSessionGate()
	// Two acquires on the same session must not both proceed.
	r1 := g.acquire("s1")
	done := make(chan struct{})
	go func() {
		r2 := g.acquire("s1")
		r2()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second acquire passed while first held")
	case <-time.After(100 * time.Millisecond):
	}
	r1()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire never released after first")
	}
}

func TestSessionGate_AllowsDifferentSessions(t *testing.T) {
	g := newSessionGate()
	r1 := g.acquire("s1")
	r2 := g.acquire("s2")
	r1()
	r2()
}

func TestDriver_DeliverBlocking_SessionFIFO(t *testing.T) {
	// Two concurrent blocking deliveries in the SAME session must be
	// serialized: the endpoint sees non-overlapping requests.
	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			m := maxConcurrent.Load()
			if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking"}
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(n int) {
			env := &Envelope{
				Crier:   EnvelopeMeta{Version: 1, MessageID: "m", Kind: "message", SessionID: "same-session"},
				Payload: json.RawMessage(`{}`),
			}
			_, err := d.DeliverBlocking(context.Background(), "agent-b", cfg, env, 5*time.Second)
			done <- err
		}(i)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if maxConcurrent.Load() > 1 {
		t.Fatalf("max concurrent in same session = %d, want 1", maxConcurrent.Load())
	}
}
