package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockLLMServer is an OpenAI-compatible chat-completions endpoint that
// records the last request (for body/auth assertions) and serves a canned
// completion. Handlers may override the response.
type mockLLMServer struct {
	mu          sync.Mutex
	requests    int
	lastBody    map[string]any
	lastAuth    string
	lastPath    string
	status      int
	response    string
	responseSeq []string // optional: replies served in call order, last one repeats
	delay       time.Duration
}

// nextReply returns the reply for call n (1-based), honouring responseSeq.
func (m *mockLLMServer) nextReply() string {
	if len(m.responseSeq) == 0 {
		return m.response
	}
	i := m.requests - 1
	if i >= len(m.responseSeq) {
		i = len(m.responseSeq) - 1
	}
	return m.responseSeq[i]
}

func newMockLLM(t *testing.T, status int, response string) (*mockLLMServer, *httptest.Server) {
	t.Helper()
	m := &mockLLMServer{status: status, response: response}
	return m, startMockLLM(t, m)
}

// newMockLLMSeq serves one reply per LLM call in order (the last reply
// repeats). Needed for the sanitize path, which makes TWO calls: the
// classifier verdict, then the §3.5 rewrite.
func newMockLLMSeq(t *testing.T, replies ...string) (*mockLLMServer, *httptest.Server) {
	t.Helper()
	if len(replies) == 0 {
		t.Fatal("newMockLLMSeq needs at least one reply")
	}
	m := &mockLLMServer{responseSeq: replies}
	return m, startMockLLM(t, m)
}

func startMockLLM(t *testing.T, m *mockLLMServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests++
		m.lastPath = r.URL.Path
		m.lastAuth = r.Header.Get("Authorization")
		m.lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&m.lastBody)
		reply := m.nextReply()
		m.mu.Unlock()
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		if m.status != 0 {
			w.WriteHeader(m.status)
			w.Write([]byte(`{"error":{"message":"mock"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *mockLLMServer) body() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBody
}

func (m *mockLLMServer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

func TestClientComplete_RequestShape(t *testing.T) {
	m, srv := newMockLLM(t, 0, `{"choices":[{"message":{"content":"{\"decision\":\"allow\"}"}}]}`)
	c := NewClient(srv.URL, "sk-test", "deepseek-v4-flash", false, 5*time.Second)

	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"decision":"allow"}` {
		t.Fatalf("content = %q", out)
	}
	b := m.body()
	if b["model"] != "deepseek-v4-flash" {
		t.Errorf("model = %v, want deepseek-v4-flash", b["model"])
	}
	if b["temperature"] != float64(0) {
		t.Errorf("temperature = %v, want 0", b["temperature"])
	}
	if b["stream"] != false {
		t.Errorf("stream = %v, want false", b["stream"])
	}
	rf, ok := b["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want {type: json_object}", b["response_format"])
	}
	if _, has := b["thinking"]; has {
		t.Errorf("thinking field present with thinking disabled")
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if m.lastAuth != "Bearer sk-test" {
		t.Errorf("auth = %q, want Bearer sk-test", m.lastAuth)
	}
	if m.lastPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", m.lastPath)
	}
}

func TestClientComplete_ThinkingEnabled(t *testing.T) {
	m, srv := newMockLLM(t, 0, `{"choices":[{"message":{"content":"{}"}}]}`)
	c := NewClient(srv.URL, "k", "model-x", true, 5*time.Second)
	if _, err := c.Complete(context.Background(), nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	th, ok := m.body()["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Errorf("thinking = %v, want {type: enabled}", m.body()["thinking"])
	}
}

func TestClientComplete_ModelRejectedOn400(t *testing.T) {
	_, srv := newMockLLM(t, http.StatusBadRequest, `{"error":{"message":"response_format unsupported"}}`)
	c := NewClient(srv.URL, "k", "m", false, 5*time.Second)
	_, err := c.Complete(context.Background(), nil)
	if !errors.Is(err, ErrModelRejected) {
		t.Fatalf("err = %v, want ErrModelRejected", err)
	}
}

func TestClientComplete_ProviderErrors(t *testing.T) {
	for name, status := range map[string]int{
		"500": http.StatusInternalServerError,
		"401": http.StatusUnauthorized,
		"429": http.StatusTooManyRequests,
		"503": http.StatusServiceUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			_, srv := newMockLLM(t, status, `{}`)
			c := NewClient(srv.URL, "k", "m", false, 5*time.Second)
			_, err := c.Complete(context.Background(), nil)
			if !errors.Is(err, ErrProvider) {
				t.Fatalf("err = %v, want ErrProvider", err)
			}
		})
	}
}

func TestClientComplete_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // dead endpoint
	c := NewClient(url, "k", "m", false, 5*time.Second)
	_, err := c.Complete(context.Background(), nil)
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
}

func TestClientComplete_MissingOrNonStringContent(t *testing.T) {
	for name, resp := range map[string]string{
		"no choices":   `{}`,
		"null content": `{"choices":[{"message":{}}]}`,
		"non-string":   `{"choices":[{"message":{"content":42}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, srv := newMockLLM(t, 0, resp)
			c := NewClient(srv.URL, "k", "m", false, 5*time.Second)
			_, err := c.Complete(context.Background(), nil)
			if !errors.Is(err, ErrProvider) {
				t.Fatalf("err = %v, want ErrProvider", err)
			}
		})
	}
}

func TestClientComplete_Timeout(t *testing.T) {
	m, srv := newMockLLM(t, 0, `{"choices":[{"message":{"content":"x"}}]}`)
	m.delay = 300 * time.Millisecond
	c := NewClient(srv.URL, "k", "m", false, 50*time.Millisecond)
	start := time.Now()
	_, err := c.Complete(context.Background(), nil)
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("timeout not honored: took %s", elapsed)
	}
}

func TestClientComplete_BaseURLJoin(t *testing.T) {
	m, srv := newMockLLM(t, 0, `{"choices":[{"message":{"content":"ok"}}]}`)
	c := NewClient(srv.URL+"/", "k", "m", false, 5*time.Second)
	if _, err := c.Complete(context.Background(), nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.HasSuffix(m.lastPath, "/chat/completions") {
		t.Errorf("path = %q", m.lastPath)
	}
}
