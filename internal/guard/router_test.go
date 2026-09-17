package guard

import (
	"context"
	"errors"
	"fmt"
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

// ── DF-CRIER-149: provider skip / failover visibility ───────────────────

// logCapture is a Logf sink that renders every line to one string
// ("msg k=v k=v") so a test can assert on exactly what an operator would see.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, b.String())
}

// find returns every captured line containing substr.
func (l *logCapture) find(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range l.lines {
		if strings.Contains(ln, substr) {
			out = append(out, ln)
		}
	}
	return out
}

func (l *logCapture) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// assertOneLine fails unless exactly one line matches substr, and returns it.
func (l *logCapture) assertOneLine(t *testing.T, substr string) string {
	t.Helper()
	got := l.find(substr)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 log line containing %q, got %d: %v", substr, len(got), l.all())
	}
	return got[0]
}

// TestRouter_SkipLogNoKey: a provider skipped for a missing key emits ONE
// line naming the provider, the model and the env VAR the preset wanted, and
// the chain landing on a later provider emits ONE summary line from → to.
func TestRouter_SkipLogNoKey(t *testing.T) {
	h2, s2 := newCountingServer(0, verdictAllowJSON)
	env := envMap{"KEY2": "k2"} // KEY1 has no key at all
	cap := &logCapture{}
	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get, Logf: cap.logf})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: "http://127.0.0.1:1", APIKeyRef: "env:KEY1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: s2.URL, APIKeyRef: "env:KEY2", Model: "m2"},
	)
	provider, model, _, err := r.Check(context.Background(), p, "sys", "user")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if provider != "custom" || model != "m2" {
		t.Fatalf("provider/model = %s/%s, want custom/m2 (fallback)", provider, model)
	}
	if n := h2.count(); n != 1 {
		t.Errorf("provider 2 count = %d, want 1", n)
	}

	line := cap.assertOneLine(t, "provider skipped")
	for _, want := range []string{"provider=custom", "model=m1", "reason=no api key", "key_ref=env:KEY1"} {
		if !strings.Contains(line, want) {
			t.Errorf("skip line %q missing %q", line, want)
		}
	}
	// The key VALUE never appears — only the env var NAME.
	if strings.Contains(line, "KVALUE") {
		t.Errorf("skip line leaked a key value: %q", line)
	}
	if n := len(cap.find("provider failed")); n != 0 {
		t.Errorf("no provider was called: want 0 'provider failed' lines, got %d: %v", n, cap.all())
	}

	sum := cap.assertOneLine(t, "failover landed on a later provider")
	for _, want := range []string{"from=custom", "to=custom", "index=1"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary line %q missing %q", sum, want)
		}
	}
	// Exactly the two lines — one per skip, one summary.
	if n := len(cap.all()); n != 2 {
		t.Errorf("want exactly 2 log lines, got %d: %v", n, cap.all())
	}
}

// TestRouter_SkipLogCircuitOpen: a provider skipped because its circuit is
// open emits ONE line with reason=circuit open, and no HTTP call happens.
func TestRouter_SkipLogCircuitOpen(t *testing.T) {
	h, srv := newCountingServer(http.StatusInternalServerError, "")
	env := envMap{"K": "k"}
	cap := &logCapture{}
	r := NewRouter(RouterOptions{
		Timeout:          2 * time.Second,
		CircuitThreshold: 2,
		CircuitCooldown:  50 * time.Millisecond,
		LookupEnv:        env.get,
		Logf:             cap.logf,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	for i := 0; i < 2; i++ {
		if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
			t.Fatalf("iteration %d: want error", i)
		}
	}
	before := h.count()
	cap.lines = nil // only the circuit-open Check is under test
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("want error while circuit open")
	}
	if n := h.count(); n != before {
		t.Fatalf("circuit open must skip HTTP calls: count %d → %d", before, n)
	}
	line := cap.assertOneLine(t, "provider skipped")
	for _, want := range []string{"provider=custom", "model=m", "reason=circuit open"} {
		if !strings.Contains(line, want) {
			t.Errorf("skip line %q missing %q", line, want)
		}
	}
	if n := len(cap.all()); n != 1 {
		t.Errorf("want exactly 1 log line, got %d: %v", n, cap.all())
	}
}

// TestRouter_ProviderErrorLogCarriesProviderSignal: a provider that exhausts
// its attempts logs ONE line carrying the provider's own signal — HTTP status
// and the provider's error code (the shape Groq uses for a model id that does
// not exist).
func TestRouter_ProviderErrorLogCarriesProviderSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"The model ` + "`nope`" + ` does not exist or you do not have access to it.","code":"model_not_found"}}`))
	}))
	defer srv.Close()
	env := envMap{"K": "k"}
	cap := &logCapture{}
	r := NewRouter(RouterOptions{Timeout: 5 * time.Second, LookupEnv: env.get, Logf: cap.logf})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "nope"})

	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("want error from a 404 provider")
	}
	line := cap.assertOneLine(t, "provider failed")
	for _, want := range []string{
		"provider=custom", "model=nope", "reason=provider error",
		"status=404", "error_code=model_not_found",
		"error_message=The model `nope` does not exist",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("provider-failure line %q missing %q", line, want)
		}
	}
	if n := len(cap.all()); n != 1 {
		t.Errorf("attempt + retry must log exactly 1 line, got %d: %v", n, cap.all())
	}
}

// ── DF-CRIER-204: a chain that dies at the per-message budget ────────────

// TestRouter_BudgetExhaustedOnSingleProviderIsLogged: a policy with a SINGLE
// provider whose endpoint outlives the per-message budget dies inside the
// retry backoff's ctx.Done() branch — the one terminal path that returned
// with no router line at all, so the failure could not be attributed to a
// provider from the log alone. Asserts the caller-visible error is unchanged
// AND that exactly ONE line names the provider, the model and the reason.
func TestRouter_BudgetExhaustedOnSingleProviderIsLogged(t *testing.T) {
	// The endpoint outlives the budget by an order of magnitude. It is
	// released by the test (not by the request context: once the body is
	// consumed the server does not observe the client's disconnect, which
	// would make httptest's Close wait out the sleep).
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()    // runs LAST
	defer close(release) // LIFO: releases the handler before Close waits on it

	env := envMap{"K": "k"} // env: prefix is mandatory for the key ref
	cap := &logCapture{}
	r := NewRouter(RouterOptions{Timeout: 300 * time.Millisecond, LookupEnv: env.get, Logf: cap.logf})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	start := time.Now()
	_, _, _, err := r.Check(context.Background(), p, "s", "u")
	if err == nil {
		t.Fatal("want error (budget exhausted)")
	}
	// Wire invariance: the error the caller saw before this fix is still the
	// error the caller sees.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("per-message budget not honored: Check took %s", elapsed)
	}

	line := cap.assertOneLine(t, "guard router: provider failed")
	for _, want := range []string{"provider=custom", "model=m", "reason=per-message budget exhausted"} {
		if !strings.Contains(line, want) {
			t.Errorf("budget line %q missing %q", line, want)
		}
	}
	// Exactly one line for the one provider: the retryable first attempt
	// logs nothing, so the terminal line must not duplicate.
	if n := len(cap.all()); n != 1 {
		t.Errorf("want exactly 1 log line for one provider, got %d: %v", n, cap.all())
	}
}

// TestRouter_ContextCanceledReason: when it is the CALLER's context that dies
// (client disconnect, spec §7.4) the terminal line maps to
// reason=context canceled — it must not claim the budget was exhausted.
func TestRouter_ContextCanceledReason(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // held until the test is done asserting
	}))
	defer srv.Close()    // runs LAST
	defer close(release) // LIFO: releases the handler before Close waits on it

	env := envMap{"K": "k"}
	cap := &logCapture{}
	r := NewRouter(RouterOptions{Timeout: 10 * time.Second, LookupEnv: env.get, Logf: cap.logf})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.Check(ctx, p, "s", "u")
		done <- err
	}()

	select {
	case <-started: // the attempt is in flight, so the cancel lands mid-call
	case <-time.After(5 * time.Second):
		t.Fatal("provider never received the attempt")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the caller canceled")
	}

	line := cap.assertOneLine(t, "guard router: provider failed")
	for _, want := range []string{"provider=custom", "model=m", "reason=context canceled"} {
		if !strings.Contains(line, want) {
			t.Errorf("cancel line %q missing %q", line, want)
		}
	}
	if n := len(cap.all()); n != 1 {
		t.Errorf("want exactly 1 log line, got %d: %v", n, cap.all())
	}
}

// TestRouter_SkipLogRemainingReasons covers the other skip code paths so the
// "one line per skipped provider" contract holds for every branch of Check.
func TestRouter_SkipLogRemainingReasons(t *testing.T) {
	cases := []struct {
		name    string
		spec    ProviderSpec
		env     envMap
		wantSub string
	}{
		{"unknown provider", ProviderSpec{Provider: "wat"}, envMap{}, "reason=unknown provider"},
		{"custom without base_url/api_key_ref", ProviderSpec{Provider: "custom"}, envMap{}, "reason=custom provider requires base_url and api_key_ref"},
		{"deepseek preset forbids thinking", ProviderSpec{Provider: "deepseek", ThinkingEnabled: true}, envMap{"DEEPSEEK_API_KEY": "k"}, "reason=deepseek preset forbids thinking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &logCapture{}
			r := NewRouter(RouterOptions{Timeout: 2 * time.Second, LookupEnv: tc.env.get, Logf: cap.logf})
			if _, _, _, err := r.Check(context.Background(), policyWith(tc.spec), "s", "u"); err == nil {
				t.Fatal("want error (every provider in the chain was skipped)")
			}
			line := cap.assertOneLine(t, "provider skipped")
			if !strings.Contains(line, tc.wantSub) {
				t.Errorf("line %q missing %q", line, tc.wantSub)
			}
			if n := len(cap.all()); n != 1 {
				t.Errorf("want exactly 1 log line, got %d: %v", n, cap.all())
			}
		})
	}
}

// TestRouter_LogNeverLeaksKeyMaterial: the router's audit lines carry no part
// of a configured API key, on any path (a call that sent the key, a skip, a
// failure).
func TestRouter_LogNeverLeaksKeyMaterial(t *testing.T) {
	// A fixture value, NOT a credential: assembled from words so no secret
	// scanner (and no reader) mistakes it for key material. What the test
	// pins is that the exact value the router read out of the env — the one
	// it sent as a Bearer token — never reaches the audit lines.
	secret := strings.Join([]string{"t304", "fixture", "key", "value"}, "-")
	_, s500 := newCountingServer(http.StatusInternalServerError, "")
	env := envMap{"K1": secret} // K2 missing → skip path
	cap := &logCapture{}
	r := NewRouter(RouterOptions{Timeout: 2 * time.Second, LookupEnv: env.get, Logf: cap.logf})
	p := policyWith(
		ProviderSpec{Provider: "custom", BaseURL: s500.URL, APIKeyRef: "env:K1", Model: "m1"},
		ProviderSpec{Provider: "custom", BaseURL: "http://127.0.0.1:1", APIKeyRef: "env:K2", Model: "m2"},
	)
	if _, _, _, err := r.Check(context.Background(), p, "sys", "user"); err == nil {
		t.Fatal("want error (both providers fail)")
	}
	if len(cap.all()) == 0 {
		t.Fatal("expected audit lines to assert against")
	}
	// Both providers logged (one failure + one skip) — the assertion below is
	// only meaningful if the key was actually used on the wire.
	for _, probe := range []string{secret, secret[:12], secret[6:20], "fixture-key"} {
		for _, ln := range cap.all() {
			if strings.Contains(ln, probe) {
				t.Fatalf("log line leaked key material %q: %q", probe, ln)
			}
		}
	}
}

// TestGuard_RouterLogfWired: guard.New must hand the guard's own logf to the
// router — without it the router's audit lines are silently discarded and a
// degraded lane stays invisible.
func TestGuard_RouterLogfWired(t *testing.T) {
	cap := &logCapture{}
	g, err := New(Options{Timeout: 2 * time.Second, LookupEnv: envMap{}.get, Logf: cap.logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// groq preset, no GROQ_API_KEY → the router skips it and must audit.
	if _, _, _, err := g.router.Check(context.Background(), policyWith(ProviderSpec{Provider: "groq"}), "s", "u"); err == nil {
		t.Fatal("want error (no key)")
	}
	line := cap.assertOneLine(t, "provider skipped")
	for _, want := range []string{"provider=groq", "model=openai/gpt-oss-120b", "reason=no api key", "key_ref=env:GROQ_API_KEY"} {
		if !strings.Contains(line, want) {
			t.Errorf("guard-wired line %q missing %q", line, want)
		}
	}
}
