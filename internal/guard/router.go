package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Preset is a built-in provider configuration (spec §5.2).
type Preset struct {
	Name      string
	BaseURL   string
	APIKeyRef string
	Models    []string // default model = Models[0]
}

// Presets is the built-in provider table (spec §5.2). The deepseek preset
// is the default for every policy that omits providers and is the fleet's
// low-latency, thinking-disabled lane (CR-FEAT-012: free-limit lanes
// groq/nvidia are usable per-policy exactly as any other provider).
//
// Model ids are the providers' own qualified ids, vendor prefix included
// (DF-CRIER-149). Both free-limit lanes serve chat ONLY under the qualified
// id: measured against the live APIs on 2026-09-17, the SAME ids WITHOUT
// their vendor prefix answer 404 from /chat/completions — groq with
// `model_not_found`, nvidia with a bare "404 page not found" — while the
// prefixed ids below answer 200. The unprefixed spellings that shipped before
// this fix made both presets 100% dead (see the commit body for the verbatim
// request/response pairs).
var Presets = map[string]Preset{
	"deepseek": {
		Name:      "deepseek",
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyRef: "env:DEEPSEEK_API_KEY",
		Models:    []string{"deepseek-v4-flash"},
	},
	"groq": {
		Name:      "groq",
		BaseURL:   "https://api.groq.com/openai/v1",
		APIKeyRef: "env:GROQ_API_KEY",
		// Models[0] is the preset default (proven live end-to-end).
		// qwen/qwen3.8-27b is a real live id but the on-demand tier caps
		// output tokens/min at 1000 while the model's default output
		// budget is ~1359, so it answers 429 for an uncapped request —
		// usable only with an explicit lower max_tokens.
		Models: []string{"openai/gpt-oss-120b", "openai/gpt-oss-20b", "qwen/qwen3.8-27b"},
	},
	"nvidia": {
		Name:      "nvidia",
		BaseURL:   "https://integrate.api.nvidia.com/v1",
		APIKeyRef: "env:NVIDIA_API_KEY",
		// deepseek-ai/deepseek-v4-flash-0731 IS in the live /v1/models
		// list but did not answer a probe request within 60s from this
		// host (curl http=000), so its serviceability is unproven and it
		// is NOT shipped as a fallback slot (DF-CRIER-149).
		Models: []string{"google/gemma-4-31b-it"},
	},
}

// RouterOptions wires the router (spec §5.4 / §9.1 env).
type RouterOptions struct {
	Timeout          time.Duration       // CR_GUARD_TIMEOUT_MS (default 10s)
	MaxConcurrent    int                 // CR_GUARD_MAX_CONCURRENT (default 8)
	CircuitThreshold int                 // CR_GUARD_CIRCUIT_THRESHOLD (default 10)
	CircuitCooldown  time.Duration       // CR_GUARD_CIRCUIT_COOLDOWN_S (default 300s)
	DeepSeekBaseURL  string              // CR_GUARD_DEEPSEEK_BASE_URL override
	DefaultModel     string              // CR_GUARD_MODEL override for the deepseek preset default model
	LookupEnv        func(string) string // env:VAR resolver (test seam)
	// Logf is the router's audit sink (DF-CRIER-149): one INFO line per
	// provider that is skipped or that exhausts its attempts, plus one
	// summary line when the chain lands on a later provider. nil = silent
	// (existing behaviour). Never receives payloads or key material.
	Logf func(msg string, args ...any)
}

// Router picks the first healthy provider in a policy's chain (spec §5.4):
// failover order = p.Providers (implicit [deepseek] when empty), one retry
// (250ms backoff) on 429/5xx/network per provider, a circuit breaker per
// base_url+model, a concurrency semaphore, and a per-message time budget
// across the whole chain.
type Router struct {
	presets   map[string]Preset
	lookupEnv func(string) string
	circuit   *Circuit
	sem       chan struct{}
	timeout   time.Duration
	logf      func(msg string, args ...any)
}

// NewRouter builds a router from options (zero values → spec defaults).
func NewRouter(opts RouterOptions) *Router {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {} // silent: no sink configured
	}
	presets := make(map[string]Preset, len(Presets))
	for name, p := range Presets {
		cp := p
		cp.Models = append([]string(nil), p.Models...)
		if name == "deepseek" {
			if opts.DeepSeekBaseURL != "" {
				cp.BaseURL = opts.DeepSeekBaseURL
			}
			if opts.DefaultModel != "" {
				cp.Models = []string{opts.DefaultModel}
			}
		}
		presets[name] = cp
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	return &Router{
		presets:   presets,
		lookupEnv: lookup,
		circuit:   NewCircuit(opts.CircuitThreshold, opts.CircuitCooldown),
		sem:       make(chan struct{}, opts.MaxConcurrent),
		timeout:   opts.Timeout,
		logf:      opts.Logf,
	}
}

// Check runs the policy chain: for each ProviderSpec in order, resolve
// preset defaults + env key, skip if the circuit is open, call Complete.
// First success wins. All failed → ErrAllProvidersFailed (the guard-error
// result is assembled by the caller per §3.6).
//
// The per-message budget (CR_GUARD_TIMEOUT_MS) covers the entire chain,
// retries included; the caller's context is threaded through so a client
// disconnect cancels the LLM call (spec §7.4).
func (r *Router) Check(ctx context.Context, p Policy, sysPrompt, userMsg string) (provider, model, content string, err error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Concurrency cap: acquire waits up to 2s, then guard error
	// (spec §5.4 — never unbounded goroutines).
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-time.After(2 * time.Second):
		return "", "", "", ErrConcurrencySaturated
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	}

	specs := p.Providers
	if len(specs) == 0 {
		specs = []ProviderSpec{{Provider: "deepseek"}}
	}

	messages := []Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userMsg},
	}

	anyKey := false
	var lastErr error
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			return "", "", "", err
		}
		preset, known := r.presets[spec.Provider]
		model := spec.Model
		if known && model == "" && len(preset.Models) > 0 {
			model = preset.Models[0]
		}
		// DF-CRIER-149: a degraded lane must be visible in the log. Exactly
		// ONE line per provider that is skipped or that exhausts its
		// attempts (reason + the env:VAR name for a missing key — never the
		// key value, never payload text), plus ONE summary line when the
		// chain lands on a later provider.
		skip := func(reason string, extra ...any) {
			args := append([]any{"provider", spec.Provider, "model", model, "reason", reason}, extra...)
			r.logf("guard router: provider skipped", args...)
		}
		failed := func(err error) {
			extra := []any{}
			if status, code, msg := providerSignal(err); status != "" || code != "" || msg != "" {
				if status != "" {
					extra = append(extra, "status", status)
				}
				if code != "" {
					extra = append(extra, "error_code", code)
				}
				if msg != "" {
					extra = append(extra, "error_message", msg)
				}
			}
			args := append([]any{"provider", spec.Provider, "model", model, "reason", "provider error"}, extra...)
			r.logf("guard router: provider failed", args...)
		}
		landed := func() {
			if i > 0 {
				r.logf("guard router: failover landed on a later provider",
					"from", specs[0].Provider, "to", spec.Provider, "index", i)
			}
		}
		if spec.Provider != "custom" && !known {
			lastErr = fmt.Errorf("%w: unknown provider %q", ErrProvider, spec.Provider)
			skip("unknown provider")
			continue
		}
		baseURL, apiKeyRef := spec.BaseURL, spec.APIKeyRef
		if spec.Provider == "custom" {
			if baseURL == "" || apiKeyRef == "" {
				lastErr = fmt.Errorf("%w: custom provider requires base_url and api_key_ref", ErrProvider)
				skip("custom provider requires base_url and api_key_ref")
				continue
			}
		} else {
			if baseURL == "" {
				baseURL = preset.BaseURL
			}
			if apiKeyRef == "" {
				apiKeyRef = preset.APIKeyRef
			}
		}

		key := baseURL + " " + model
		apiKey := ""
		if strings.HasPrefix(apiKeyRef, "env:") {
			apiKey = r.lookupEnv(strings.TrimPrefix(apiKeyRef, "env:"))
		}
		if apiKey == "" {
			// Missing key: skip (counts as a failure for the circuit),
			// failover continues (spec §5.4).
			r.circuit.recordFailure(key)
			lastErr = fmt.Errorf("%w: missing api key for provider %q", ErrProvider, spec.Provider)
			skip("no api key", "key_ref", apiKeyRef)
			continue
		}
		anyKey = true

		if r.circuit.open(key) {
			// Circuit open: skip the provider (cheap failover, no wasted
			// calls); the first call after cooldown expiry is the probe.
			lastErr = fmt.Errorf("%w: circuit open for %s %s", ErrProvider, spec.Provider, model)
			skip("circuit open")
			continue
		}

		// Spec §5.2: the deepseek preset's contract is thinking-free; the
		// policy validator hard-rejects it, this is defensive.
		if spec.Provider == "deepseek" && spec.ThinkingEnabled {
			lastErr = fmt.Errorf("%w: deepseek preset forbids thinking", ErrProvider)
			skip("deepseek preset forbids thinking")
			continue
		}

		client := NewClient(baseURL, apiKey, model, spec.ThinkingEnabled, r.timeout)
		content, cerr := client.Complete(ctx, messages)
		if cerr == nil {
			r.circuit.recordSuccess(key)
			landed()
			return spec.Provider, model, content, nil
		}
		r.circuit.recordFailure(key)
		lastErr = cerr

		if !retryable(cerr) {
			// Permanent (e.g. 400 response_format rejection): failover
			// immediately, no retry (spec §5.3: one retry on 429/5xx/network
			// only).
			failed(cerr)
			continue
		}
		// One retry with 250ms backoff (spec §5.4).
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		}
		content, cerr2 := client.Complete(ctx, messages)
		if cerr2 == nil {
			r.circuit.recordSuccess(key)
			landed()
			return spec.Provider, model, content, nil
		}
		r.circuit.recordFailure(key)
		lastErr = cerr2
		failed(cerr2)
	}

	if !anyKey {
		return "", "", "", fmt.Errorf("%w: no provider api key", ErrAllProvidersFailed)
	}
	return "", "", "", fmt.Errorf("%w: %v", ErrAllProvidersFailed, lastErr)
}

// retryable reports whether an error deserves the one 250ms-backoff retry:
// 429/5xx/network (ErrProvider), never a model rejection (ErrModelRejected).
func retryable(err error) bool {
	return errors.Is(err, ErrProvider) && !errors.Is(err, ErrModelRejected)
}

// providerStatusRe pulls the HTTP status out of the client's error text
// ("…: status 404: {…}", internal/guard/client.go).
var providerStatusRe = regexp.MustCompile(`status (\d{3})`)

// providerSignal extracts the provider's OWN failure signal from a client
// error so the router's audit line can name what the provider said
// (DF-CRIER-149) — e.g. Groq's `model_not_found` for an id that does not
// exist. The client packs the status and the response body into the error
// text and exposes them nowhere else, so they are read back out here rather
// than re-implementing the transport. Any field that cannot be recovered is
// left empty; a truncated body simply yields no code/message.
func providerSignal(err error) (status, code, message string) {
	if err == nil {
		return "", "", ""
	}
	s := err.Error()
	if m := providerStatusRe.FindStringSubmatch(s); m != nil {
		status = m[1]
	}
	if i := strings.Index(s, "{"); i >= 0 {
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(s[i:]), &body) == nil {
			return status, body.Error.Code, body.Error.Message
		}
	}
	return status, "", ""
}
