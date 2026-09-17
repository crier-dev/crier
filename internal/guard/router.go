package guard

import (
	"context"
	"errors"
	"fmt"
	"os"
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
// (DF-CRIER-149). Both free-limit lanes serve chat only under the fully
// qualified id — measured against the live APIs on 2026-09-17:
//
//	groq:   gpt-oss-120b -> 404 model_not_found; openai/gpt-oss-120b -> 200
//	nvidia: gemma-4-31b  -> 404 page not found;  google/gemma-4-31b-it -> 200
//
// The bare ids shipped before this fix made both presets 100% dead.
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
}

// NewRouter builds a router from options (zero values → spec defaults).
func NewRouter(opts RouterOptions) *Router {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
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
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return "", "", "", err
		}
		preset, known := r.presets[spec.Provider]
		if spec.Provider != "custom" && !known {
			lastErr = fmt.Errorf("%w: unknown provider %q", ErrProvider, spec.Provider)
			continue
		}
		baseURL, apiKeyRef := spec.BaseURL, spec.APIKeyRef
		if spec.Provider == "custom" {
			if baseURL == "" || apiKeyRef == "" {
				lastErr = fmt.Errorf("%w: custom provider requires base_url and api_key_ref", ErrProvider)
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
		model := spec.Model
		if model == "" && len(preset.Models) > 0 {
			model = preset.Models[0]
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
			continue
		}
		anyKey = true

		if r.circuit.open(key) {
			// Circuit open: skip the provider (cheap failover, no wasted
			// calls); the first call after cooldown expiry is the probe.
			lastErr = fmt.Errorf("%w: circuit open for %s %s", ErrProvider, spec.Provider, model)
			continue
		}

		// Spec §5.2: the deepseek preset's contract is thinking-free; the
		// policy validator hard-rejects it, this is defensive.
		if spec.Provider == "deepseek" && spec.ThinkingEnabled {
			lastErr = fmt.Errorf("%w: deepseek preset forbids thinking", ErrProvider)
			continue
		}

		client := NewClient(baseURL, apiKey, model, spec.ThinkingEnabled, r.timeout)
		content, cerr := client.Complete(ctx, messages)
		if cerr == nil {
			r.circuit.recordSuccess(key)
			return spec.Provider, model, content, nil
		}
		r.circuit.recordFailure(key)
		lastErr = cerr

		if !retryable(cerr) {
			// Permanent (e.g. 400 response_format rejection): failover
			// immediately, no retry (spec §5.3: one retry on 429/5xx/network
			// only).
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
			return spec.Provider, model, content, nil
		}
		r.circuit.recordFailure(key)
		lastErr = cerr2
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
