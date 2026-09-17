package guard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const verdictAllowJSON = `{"choices":[{"message":{"content":"{\"decision\":\"allow\",\"risk_level\":\"low\",\"reason\":\"clean\",\"matched_patterns\":[]}"}}]}`

// countingHandler serves a fixed response and counts requests.
type countingHandler struct {
	mu       sync.Mutex
	n        int
	status   int
	response string
	block    chan struct{} // when non-nil, requests block until closed
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	if h.block != nil {
		<-h.block
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		w.Write([]byte(`{"error":{"message":"mock"}}`))
		return
	}
	w.Write([]byte(h.response))
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func newCountingServer(status int, response string) (*countingHandler, *httptest.Server) {
	h := &countingHandler{status: status, response: response}
	return h, httptest.NewServer(h)
}

// envMap is a lookupEnv test seam.
type envMap map[string]string

func (e envMap) get(k string) string { return e[k] }

// policyWith returns a policy with the given provider chain.
func policyWith(providers ...ProviderSpec) Policy {
	return Policy{ID: "test", Providers: providers}
}

func TestRouter_FailoverToSecondProvider(t *testing.T) {
	h1, s1 := newCountingServer(http.StatusInternalServerError, "")
	h2, s2 := newCountingServer(0, verdictAllowJSON)
	env := envMap{"KEY1": "k1", "KEY2": "k2"}

	r := NewRouter(RouterOptions{
		Timeout:       5 * time.Second,
		MaxConcurrent: 8,
		LookupEnv:     env.get,
	})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: s1.URL, APIKeyRef: "env:KEY1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: s2.URL, APIKeyRef: "env:KEY2", Model: "m2"},
	)
	provider, model, content, err := r.Check(context.Background(), p, "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if provider != "custom" || model != "m2" {
		t.Errorf("provider/model = %s/%s, want custom/m2", provider, model)
	}
	if !strings.Contains(content, `"decision":"allow"`) {
		t.Errorf("content = %q", content)
	}
	// 500 on provider 1 → one retry (250ms backoff) → 2 requests, then failover.
	if n := h1.count(); n != 2 {
		t.Errorf("provider 1 request count = %d, want 2 (attempt + retry)", n)
	}
	if n := h2.count(); n != 1 {
		t.Errorf("provider 2 request count = %d, want 1", n)
	}
}

func TestRouter_MissingKeySkipsProvider(t *testing.T) {
	h2, s2 := newCountingServer(0, verdictAllowJSON)
	env := envMap{"KEY1": "", "KEY2": "k2"} // KEY1 present but empty = missing

	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: "http://127.0.0.1:1", APIKeyRef: "env:KEY1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: s2.URL, APIKeyRef: "env:KEY2", Model: "m2"},
	)
	provider, _, _, err := r.Check(context.Background(), p, "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if provider != "custom" {
		t.Errorf("provider = %q", provider)
	}
	if n := h2.count(); n != 1 {
		t.Errorf("provider 2 count = %d, want 1", n)
	}
}

func TestRouter_NoKeyAnywhere(t *testing.T) {
	r := NewRouter(RouterOptions{Timeout: 2 * time.Second, LookupEnv: envMap{}.get})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: "http://127.0.0.1:1", APIKeyRef: "env:NOPE", Model: "m"})
	_, _, _, err := r.Check(context.Background(), p, "sys", "user")
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("err = %v, want ErrAllProvidersFailed", err)
	}
	if !strings.Contains(err.Error(), "no provider api key") {
		t.Errorf("err = %v, want 'no provider api key'", err)
	}
}

func TestRouter_ModelRejected400FailsOver(t *testing.T) {
	h1, s1 := newCountingServer(http.StatusBadRequest, "")
	_, s2 := newCountingServer(0, verdictAllowJSON)
	env := envMap{"K1": "k1", "K2": "k2"}
	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: s1.URL, APIKeyRef: "env:K1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: s2.URL, APIKeyRef: "env:K2", Model: "m2"},
	)
	provider, _, _, err := r.Check(context.Background(), p, "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if provider != "custom" || h1.count() != 1 {
		t.Errorf("400 must NOT retry (retry is for 429/5xx/network only): provider=%s h1=%d", provider, h1.count())
	}
}

func TestRouter_CircuitOpenSkipsProvider(t *testing.T) {
	h, srv := newCountingServer(http.StatusInternalServerError, "")
	env := envMap{"K": "k"}
	r := NewRouter(RouterOptions{
		Timeout:          2 * time.Second,
		CircuitThreshold: 2,
		CircuitCooldown:  50 * time.Millisecond,
		LookupEnv:        env.get,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	// Two failures (each with a retry) trip the circuit.
	for i := 0; i < 2; i++ {
		if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
			t.Fatalf("iteration %d: want error", i)
		}
	}
	// Circuit is now open: the provider is skipped — NO HTTP calls, error
	// comes back without touching the server.
	before := h.count()
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("want error while circuit open")
	}
	if n := h.count(); n != before {
		t.Fatalf("circuit open must skip HTTP calls: count %d → %d", before, n)
	}

	// After the cooldown, the next call is the probe: it hits the server
	// (and fails, since the server still 500s → circuit reopens).
	time.Sleep(80 * time.Millisecond)
	before = h.count()
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("want error from probe")
	}
	if n := h.count(); n <= before {
		t.Fatalf("probe after cooldown must hit the server: count %d → %d", before, n)
	}
}

func TestRouter_ProbeClosesCircuit(t *testing.T) {
	var status atomic.Int32
	status.Store(int32(http.StatusInternalServerError))
	h := &countingHandler{}
	h.status = int(http.StatusInternalServerError)
	// Use a handler whose status flips to 200 on the probe.
	h.response = verdictAllowJSON
	h.status = http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
		if status.Load() == http.StatusOK {
			w.Write([]byte(verdictAllowJSON))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	env := envMap{"K": "k"}
	r := NewRouter(RouterOptions{
		Timeout:          2 * time.Second,
		CircuitThreshold: 2,
		CircuitCooldown:  50 * time.Millisecond,
		LookupEnv:        env.get,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})
	for i := 0; i < 2; i++ {
		r.Check(context.Background(), p, "s", "u")
	}
	// Flip healthy, wait out the cooldown, probe succeeds → circuit closes.
	status.Store(int32(http.StatusOK))
	time.Sleep(80 * time.Millisecond)
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err != nil {
		t.Fatalf("probe should succeed: %v", err)
	}
	// Circuit closed: next call goes straight through.
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err != nil {
		t.Fatalf("call after closed circuit: %v", err)
	}
}

func TestRouter_ConcurrencyCap(t *testing.T) {
	block := make(chan struct{})
	h := &countingHandler{response: verdictAllowJSON, block: block}
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := envMap{"K": "k"}
	r := NewRouter(RouterOptions{
		Timeout:       10 * time.Second,
		MaxConcurrent: 1,
		LookupEnv:     env.get,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r.Check(context.Background(), p, "s", "u")
	}()
	// Give the first call time to grab the semaphore, then a second call
	// must hit the 2s acquire cap and fail with ErrConcurrencySaturated.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_, _, _, err := r.Check(context.Background(), p, "s", "u")
	if !errors.Is(err, ErrConcurrencySaturated) {
		t.Fatalf("err = %v, want ErrConcurrencySaturated", err)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("acquire released too early (%s)", elapsed)
	}
	// Release the first call and let it finish.
	close(block)
	<-firstDone
}

func TestRouter_TimeoutBudgetAcrossChain(t *testing.T) {
	slow := &countingHandler{response: verdictAllowJSON}
	slow.block = make(chan struct{})
	srv := httptest.NewServer(slow)
	defer srv.Close()
	// Registered AFTER srv.Close so it runs BEFORE it (LIFO): Close waits
	// for outstanding requests, which need the block released first.
	defer close(slow.block)
	env := envMap{"K": "k"}
	r := NewRouter(RouterOptions{Timeout: 150 * time.Millisecond, LookupEnv: env.get})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})
	_, _, _, err := r.Check(context.Background(), p, "s", "u")
	if err == nil {
		t.Fatal("want timeout error")
	}
}

func TestRouter_DeepSeekPresetDefaults(t *testing.T) {
	// No providers → implicit [deepseek preset]; missing DEEPSEEK_API_KEY
	// → guard error "no provider api key".
	r := NewRouter(RouterOptions{Timeout: 2 * time.Second, LookupEnv: envMap{}.get})
	_, _, _, err := r.Check(context.Background(), BuiltinDefaultPolicy(), "sys", "user")
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("err = %v, want ErrAllProvidersFailed (missing key)", err)
	}
	if !strings.Contains(err.Error(), "no provider api key") {
		t.Errorf("err = %v", err)
	}
}

// ── CR-FEAT-012: request building per provider preset ───────────────────

// TestRouter_PresetRequestBuilding proves the preset resolution contract
// (spec §5.2/§5.4): a policy naming a preset with empty base_url /
// api_key_ref / model resolves the preset's defaults — URL, model, and the
// env:VAR key ref — and thinking stays OFF unless the policy opts in. The
// preset base URL itself is overridden to a local mock (the router's
// base_url override path is the same one operators use for mirrors), so
// the assertions are: Authorization header from the env ref, model from
// the preset table, /chat/completions path, no thinking field.
func TestRouter_PresetRequestBuilding(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string // want model (preset default or explicit)
		envKey   string // env var the preset's api_key_ref resolves
		envVal   string
	}{
		{"deepseek", "deepseek", "deepseek-v4-flash", "DEEPSEEK_API_KEY", "ds-key"},
		{"groq default model", "groq", "openai/gpt-oss-120b", "GROQ_API_KEY", "gq-key"},
		{"nvidia default model", "nvidia", "google/gemma-4-31b-it", "NVIDIA_API_KEY", "nv-key"},
		{"groq explicit model", "groq", "openai/gpt-oss-20b", "GROQ_API_KEY", "gq-key"},
		{"nvidia explicit model", "nvidia", "google/gemma-3-12b-it", "NVIDIA_API_KEY", "nv-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, srv := newMockLLM(t, 0, verdictAllowJSON)
			env := envMap{tc.envKey: tc.envVal}
			r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get})
			spec := ProviderSpec{Provider: tc.provider, BaseURL: srv.URL} // URL override to local mock; model+key from preset
			if tc.name == "groq explicit model" || tc.name == "nvidia explicit model" {
				spec.Model = tc.model
			}
			_, model, _, err := r.Check(context.Background(), policyWith(spec), "sys", "user")
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if model != tc.model {
				t.Fatalf("model = %q, want %q (preset default)", model, tc.model)
			}
			// Auth header from the preset's env:VAR key ref.
			if m.lastAuth != "Bearer "+tc.envVal {
				t.Fatalf("auth = %q, want Bearer %s (from env:%s)", m.lastAuth, tc.envVal, tc.envKey)
			}
			// OpenAI-compatible chat-completions path.
			if m.lastPath != "/chat/completions" {
				t.Fatalf("path = %q, want /chat/completions", m.lastPath)
			}
			// thinking_enabled=false → NO thinking field in the body.
			if _, has := m.body()["thinking"]; has {
				t.Fatal("thinking field present with thinking disabled")
			}
		})
	}
}

// TestRouter_DeepSeekBaseURLOption proves CR_GUARD_DEEPSEEK_BASE_URL
// (RouterOptions.DeepSeekBaseURL) overrides the deepseek preset's default
// URL (spec §5.2) — used for mirrors/proxies.
func TestRouter_DeepSeekBaseURLOption(t *testing.T) {
	m, srv := newMockLLM(t, 0, verdictAllowJSON)
	env := envMap{"DEEPSEEK_API_KEY": "ds-key"}
	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, DeepSeekBaseURL: srv.URL, LookupEnv: env.get})
	// Implicit [deepseek preset] — the option-rewritten base URL is used.
	_, model, _, err := r.Check(context.Background(), BuiltinDefaultPolicy(), "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if model != "deepseek-v4-flash" {
		t.Fatalf("model = %q", model)
	}
	if m.lastAuth != "Bearer ds-key" {
		t.Fatalf("auth = %q", m.lastAuth)
	}
	if !strings.HasSuffix(m.lastPath, "/chat/completions") {
		t.Fatalf("path = %q", m.lastPath)
	}
}

// TestRouter_Unauthorized401FailsOver: a 401 on the primary provider is a
// retryable provider failure (spec §5.4) — one retry, then failover to the
// fallback; the fallback's verdict wins.
func TestRouter_Unauthorized401FailsOver(t *testing.T) {
	h1, s1 := newCountingServer(http.StatusUnauthorized, "")
	h2, s2 := newCountingServer(0, verdictAllowJSON)
	env := envMap{"K1": "k1", "K2": "k2"}
	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: s1.URL, APIKeyRef: "env:K1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: s2.URL, APIKeyRef: "env:K2", Model: "m2"},
	)
	provider, model, _, err := r.Check(context.Background(), p, "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if provider != "custom" || model != "m2" {
		t.Fatalf("provider/model = %s/%s, want custom/m2 (fallback)", provider, model)
	}
	// 401 → one retry (250ms backoff) → failover.
	if n := h1.count(); n != 2 {
		t.Errorf("provider 1 request count = %d, want 2 (attempt + retry)", n)
	}
	if n := h2.count(); n != 1 {
		t.Errorf("provider 2 request count = %d, want 1", n)
	}
}
