package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestTemplate_BuildBody_CrierSessionThreadExpansion(t *testing.T) {
	// CR-GAP-037: {{crier.session_id}} / {{crier.thread_id}} rendered EMPTY
	// because resolvePath only walked map[string]any / []any nodes while
	// ctx.Crier is the struct-typed EnvelopeMeta. The envelope's session,
	// thread and message ids must reach the rendered body.
	tpl := Template{
		Name:        "test-crier",
		ResponseMap: "raw",
		RequestShape: RequestShape{
			Method: "POST",
			Body: json.RawMessage(`{
				"session_id": "{{crier.session_id}}",
				"thread_id": "{{crier.thread_id}}",
				"message_id": "{{crier.message_id}}",
				"content": "{{payload.text}}",
				"agent_id": "{{agent.id}}"
			}`),
		},
	}
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-1", SessionID: "sess-x", ThreadID: "thread-y", Sender: "agent-a"},
		Payload: json.RawMessage(`{"text":"hello"}`),
	}
	body, err := tpl.BuildBody(&Config{URL: "http://x"}, env)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if v["session_id"] != "sess-x" {
		t.Fatalf("session_id = %v (want sess-x)", v["session_id"])
	}
	if v["thread_id"] != "thread-y" {
		t.Fatalf("thread_id = %v (want thread-y)", v["thread_id"])
	}
	if v["message_id"] != "m-1" {
		t.Fatalf("message_id = %v (want m-1)", v["message_id"])
	}
	// Regression: existing {{payload.*}} / {{agent.*}} paths still expand.
	if v["content"] != "hello" {
		t.Fatalf("content = %v (want hello)", v["content"])
	}
	if v["agent_id"] != "agent-a" {
		t.Fatalf("agent_id = %v (want agent-a)", v["agent_id"])
	}
	// The half that used to live here asserted a SILENT empty-string
	// substitution for an absent path ("Empty session/thread render as empty
	// strings (omitempty drops the key)"). DF-CRIER-279 makes that path a hard
	// error, so the contract asserted now is the error itself — naming every
	// placeholder the context cannot fill — and the |default: form below is
	// what still renders "" on purpose.
	emptyEnv := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-2", Sender: "agent-a"},
		Payload: json.RawMessage(`{}`),
	}
	_, err = tpl.BuildBody(&Config{URL: "http://x"}, emptyEnv)
	if err == nil {
		t.Fatal("BuildBody = nil error, want a loud failure naming the absent placeholders")
	}
	for _, want := range []string{"crier.session_id", "crier.thread_id", "payload.text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %q", err, want)
		}
	}
	// Explicit defaults are the escape hatch: an absent value renders the
	// declared default (here: empty) with no error.
	defaultedTpl := Template{
		Name:        "test-crier-defaulted",
		ResponseMap: "raw",
		RequestShape: RequestShape{
			Method: "POST",
			Body: json.RawMessage(`{
				"session_id": "{{crier.session_id|default:}}",
				"thread_id": "{{crier.thread_id|default:}}",
				"content": "{{payload.text|default:no text}}"
			}`),
		},
	}
	defaultedBody, err := defaultedTpl.BuildBody(&Config{URL: "http://x"}, emptyEnv)
	if err != nil {
		t.Fatalf("explicit defaults must render, got error: %v", err)
	}
	var v2 map[string]any
	if err := json.Unmarshal(defaultedBody, &v2); err != nil {
		t.Fatalf("defaulted body not json: %v", err)
	}
	if v2["session_id"] != "" || v2["thread_id"] != "" {
		t.Errorf("session_id/thread_id = %v/%v, want empty (explicit |default:)", v2["session_id"], v2["thread_id"])
	}
	if v2["content"] != "no text" {
		t.Errorf("content = %v, want the declared default", v2["content"])
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

// ---------- DF-CRIER-279: a missing template path is a LOUD failure ----------

// TestTemplate_BuildBody_MissingPathFailsLoud: a payload with no `text` key
// used to render `"content": ""` — the POST went out, the endpoint answered
// 200, and the delivery was reported as delivered with the message body gone.
// It is now an error naming the path that could not be filled.
func TestTemplate_BuildBody_MissingPathFailsLoud(t *testing.T) {
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-1", Sender: "agent-a"},
		Payload: json.RawMessage(`{"task":"wrong shape"}`),
	}
	body, err := openAICompatible.BuildBody(&Config{URL: "http://x", SchemaTemplate: "openai-compatible"}, env)
	if err == nil {
		t.Fatalf("BuildBody = %s, nil error — a payload without text must not render", body)
	}
	if body != nil {
		t.Errorf("body = %s, want nil alongside the error", body)
	}
	if !strings.Contains(err.Error(), "payload.text") {
		t.Errorf("error %q does not name the missing path payload.text", err)
	}
	// The template's shape did not change: with the text key present the SAME
	// template expands (the failure is the payload mismatch, not the template).
	env.Payload = json.RawMessage(`{"text":"hello"}`)
	if _, err := openAICompatible.BuildBody(&Config{URL: "http://x"}, env); err != nil {
		t.Errorf("same template with text present: %v", err)
	}
}

// TestTemplate_BuildBody_HappyPathBytesUnchanged pins the EXACT bytes the
// openai-compatible template renders for a text payload: the fail-loud change
// must not move the happy path (DF-CRIER-279).
func TestTemplate_BuildBody_HappyPathBytesUnchanged(t *testing.T) {
	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-1", Sender: "a"},
		Payload: json.RawMessage(`{"text":"hello"}`),
	}
	body, err := openAICompatible.BuildBody(&Config{URL: "http://x"}, env)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"messages":[{"content":"hello","role":"user"}],"model":"deepseek-v4-flash","stream":false}`
	if string(body) != want {
		t.Errorf("body = %s\nwant      %s", body, want)
	}
}

// TestTemplate_BuildBody_ExplicitDefaultStillRenders: `|default:` is how a
// template declares "this value may be absent" — it must keep working, including
// an explicitly EMPTY default.
func TestTemplate_BuildBody_ExplicitDefaultStillRenders(t *testing.T) {
	tpl := Template{
		Name:        "test-defaults",
		ResponseMap: "raw",
		RequestShape: RequestShape{
			Method: "POST",
			Body: json.RawMessage(`{
				"content": "{{payload.text|default:hello}}",
				"optional": "{{payload.nope|default:}}",
				"session_id": "{{crier.session_id|default:}}"
			}`),
		},
	}
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m-1"}, Payload: json.RawMessage(`{}`)}
	body, err := tpl.BuildBody(&Config{URL: "http://x"}, env)
	if err != nil {
		t.Fatalf("explicit defaults must render, got error: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if v["content"] != "hello" {
		t.Errorf("content = %v, want the declared default", v["content"])
	}
	if v["optional"] != "" {
		t.Errorf("optional = %v, want an explicitly empty default", v["optional"])
	}
	if v["session_id"] != "" {
		t.Errorf("session_id = %v, want an explicitly empty default", v["session_id"])
	}
}

// TestTemplate_BuildBody_HermesGatewayMissingTextFailsLoud: the session-aware
// template shares the `{{payload.text}}` shape, so a payload without text fails
// the same way (DF-CRIER-279) — while its session/thread slots, which spec §3
// makes OPTIONAL envelope fields, keep rendering empty through explicit
// `|default:`s instead of failing a legitimate thread-less delivery.
func TestTemplate_BuildBody_HermesGatewayMissingTextFailsLoud(t *testing.T) {
	cfg := &Config{URL: "http://x", SchemaTemplate: "hermes-http-gateway"}
	noText := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-1", Sender: "a", SessionID: "sess-x", ThreadID: "thread-y"},
		Payload: json.RawMessage(`{"task":"wrong shape"}`),
	}
	if _, err := hermesGateway.BuildBody(cfg, noText); err == nil {
		t.Fatal("BuildBody = nil error — hermes-http-gateway must fail loudly without payload.text")
	} else if !strings.Contains(err.Error(), "payload.text") {
		t.Errorf("error %q does not name the missing path payload.text", err)
	}

	noThread := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m-2", Sender: "a", SessionID: "sess-x"},
		Payload: json.RawMessage(`{"text":"17 times 23?"}`),
	}
	body, err := hermesGateway.BuildBody(cfg, noThread)
	if err != nil {
		t.Fatalf("a gateway delivery without a thread must still render: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	msgs, ok := v["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", v["messages"])
	}
	if c := msgs[0].(map[string]any)["content"]; c != "17 times 23?" {
		t.Errorf("content = %v, want the payload text", c)
	}
	if v["session_id"] != "sess-x" || v["thread_id"] != "" {
		t.Errorf("session_id/thread_id = %v/%v, want sess-x and empty", v["session_id"], v["thread_id"])
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
