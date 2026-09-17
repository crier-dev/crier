package guard

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/middleware"
)

// Filter is the guard choke point (spec §2). A nil Filter disables the
// guard (kept for tests and for CR_GUARD_ENABLED=false).
type Filter interface {
	// Check runs the guard for one inbound message. Errors are
	// misconfiguration only (spec §2.2) — provider failures are folded
	// into Result.Errored per §3.6 and resolved by the policy's
	// fail_open/fail_closed decision.
	Check(ctx context.Context, agentID string, cfg *AgentGuardConfig, in Input) (Result, error)
}

// Guard is the default Filter implementation (spec §11.1): pattern
// pre-scan → payload render → LLM call through the router → verdict
// parse/validate → deterministic escalation → sanitize quarantine. It also
// owns the §7.3 audit line and internal counters.
type Guard struct {
	router          *Router
	scanner         *PreScanner
	defaultPolicy   Policy // server-wide default (CR_GUARD_DEFAULT_POLICY, §4.2 step 3)
	maxPayloadBytes int
	renderMaxBytes  int
	logf            func(msg string, args ...any) // info-level audit
	logfWarn        func(msg string, args ...any) // warn-level audit (decision != allow || errored)
	kw              *kanbanWorker                 // CR-FEAT-014 output option; nil = kanban disabled
	closeOnce       sync.Once
	mu              sync.Mutex
	checksTotal     map[string]int64 // "decision/risk"
	errorsTotal     int64
	llmCallsTotal   map[string]int64 // "provider/model"
	sanitizeTotal   int64
	blockTotal      int64
}

// Options wires a Guard (spec §9.1 env → guard.New in cmd/server).
type Options struct {
	Timeout           time.Duration // CR_GUARD_TIMEOUT_MS (default 10s)
	MaxConcurrent     int           // CR_GUARD_MAX_CONCURRENT (default 8)
	CircuitThreshold  int           // CR_GUARD_CIRCUIT_THRESHOLD (default 10)
	CircuitCooldown   time.Duration // CR_GUARD_CIRCUIT_COOLDOWN_S (default 300s)
	MaxPayloadBytes   int           // CR_GUARD_MAX_PAYLOAD_BYTES (default 65536)
	RenderMaxBytes    int           // CR_GUARD_RENDER_MAX_BYTES (default 32768)
	DeepSeekBaseURL   string        // CR_GUARD_DEEPSEEK_BASE_URL override
	DefaultModel      string        // CR_GUARD_MODEL override for the deepseek preset default model
	ExtraPatterns     string        // CR_GUARD_PATTERNS_EXTRA JSON
	DefaultPolicyJSON string        // CR_GUARD_DEFAULT_POLICY — server-wide default policy (JSON); invalid fails fast
	KanbanWriter      CardWriter    // CR-FEAT-014: nil = kanban output disabled (no-op writer)
	KanbanQueueSize   int           // CR_GUARD_KANBAN_QUEUE (default 100, spec §8.2)
	LookupEnv         func(string) string
	Logf              func(msg string, args ...any)
	LogfWarn          func(msg string, args ...any)
}

// New builds a Guard. Zero option values take the spec defaults; invalid
// CR_GUARD_PATTERNS_EXTRA or CR_GUARD_DEFAULT_POLICY fail fast (a broken
// guard must not silently degrade, spec §9.1).
func New(opts Options) (*Guard, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
	}
	if opts.MaxPayloadBytes <= 0 {
		opts.MaxPayloadBytes = 65536
	}
	if opts.RenderMaxBytes <= 0 {
		opts.RenderMaxBytes = 32768
	}
	if opts.KanbanQueueSize <= 0 {
		opts.KanbanQueueSize = 100
	}
	scanner, err := NewPreScanner(opts.ExtraPatterns)
	if err != nil {
		return nil, err
	}
	defaultPolicy := BuiltinDefaultPolicy()
	if opts.DefaultPolicyJSON != "" {
		// Spec §9.1: the server default must parse + validate as a Policy,
		// else the server fails fast (a broken default policy must not
		// silently fail open).
		if defaultPolicy, err = ParsePolicy(opts.DefaultPolicyJSON); err != nil {
			return nil, err
		}
	}
	logf, logfWarn := opts.Logf, opts.LogfWarn
	if logf == nil {
		logf = func(msg string, args ...any) { slog.Info(msg, args...) }
	}
	if logfWarn == nil {
		logfWarn = func(msg string, args ...any) { slog.Warn(msg, args...) }
	}
	g := &Guard{
		router: NewRouter(RouterOptions{
			Timeout:          opts.Timeout,
			MaxConcurrent:    opts.MaxConcurrent,
			CircuitThreshold: opts.CircuitThreshold,
			CircuitCooldown:  opts.CircuitCooldown,
			DeepSeekBaseURL:  opts.DeepSeekBaseURL,
			DefaultModel:     opts.DefaultModel,
			LookupEnv:        opts.LookupEnv,
			// DF-CRIER-149: the router audits through the same sink the
			// guard uses, so a skipped provider / failover is visible in
			// the server log next to the verdict lines.
			Logf: logf,
		}),
		scanner:         scanner,
		defaultPolicy:   defaultPolicy,
		maxPayloadBytes: opts.MaxPayloadBytes,
		renderMaxBytes:  opts.RenderMaxBytes,
		logf:            logf,
		logfWarn:        logfWarn,
		checksTotal:     make(map[string]int64),
		llmCallsTotal:   make(map[string]int64),
	}
	if opts.KanbanWriter != nil {
		// CR-FEAT-014 (spec §8.2): the kanban worker is a bounded
		// goroutine + buffered channel; enqueue never blocks. A nil
		// writer keeps kanban disabled (no-op) until CR-FEAT-009 lands.
		g.kw = newKanbanWorker(opts.KanbanWriter, opts.KanbanQueueSize, logf, logfWarn)
	}
	return g, nil
}

// Close stops the kanban worker (CR-FEAT-014). Idempotent; safe to call
// from server shutdown and test cleanup. No-op when kanban is disabled.
func (g *Guard) Close() {
	g.closeOnce.Do(func() {
		if g.kw != nil {
			g.kw.close()
		}
	})
}

// Check implements Filter (spec §2 / §11.1).
func (g *Guard) Check(ctx context.Context, agentID string, cfg *AgentGuardConfig, in Input) (Result, error) {
	start := time.Now()
	// Per-channel policy resolution (spec §4.2, CR-FEAT-011): agent
	// channel_match override → agent default → server default
	// (CR_GUARD_DEFAULT_POLICY) → built-in default.
	policy := ResolvePolicy(cfg, g.defaultPolicy, in.SessionID, in.ThreadID)

	// Deterministic pattern pre-scan over the RAW payload (spec §6.4):
	// enriches the prompt and is the only verdict source for oversize
	// payloads (§6.3). Prematch names of disabled checks are suppressed
	// (spec §3.3).
	prematch := g.scanner.Scan(in.Payload)
	prematch = filterPrematch(prematch, policy.Checks, g.scanner)

	// Oversize path (§6.3): skip the LLM entirely. Only high-confidence
	// prematch hits may block here — low-confidence shape-only evidence
	// (b64_blob on an ordinary long alphanumeric/base64-like body) is
	// reported as evidence but cannot hard-block on its own (DF-CRIER-31).
	if len(in.Payload) > g.maxPayloadBytes {
		strong := g.scanner.HighConfidence(prematch)
		var res Result
		switch {
		case len(strong) > 0:
			res = Result{
				Decision:  DecisionBlock,
				RiskLevel: RiskHigh,
				Reason:    "payload_exceeds_guard_cap: prematch hit",
				Patterns:  prematch,
				PolicyID:  policy.ID,
			}
		case len(prematch) > 0:
			res = Result{
				Decision:  DecisionAllow,
				RiskLevel: RiskMedium,
				Reason:    "payload_exceeds_guard_cap: low-confidence prematch only",
				Patterns:  append([]string{"oversize"}, prematch...),
				PolicyID:  policy.ID,
			}
		default:
			res = Result{
				Decision:  DecisionAllow,
				RiskLevel: RiskMedium,
				Reason:    "payload_exceeds_guard_cap",
				Patterns:  []string{"oversize"},
				PolicyID:  policy.ID,
			}
		}
		res.MessageID = in.MessageID
		res.DurationMs = time.Since(start).Milliseconds()
		g.record(res)
		g.audit(ctx, agentID, in, res, len(in.Payload))
		g.maybeEnqueueCard(agentID, in, policy, res)
		return res, nil
	}

	projection, err := Render(in.Payload, g.renderMaxBytes)
	if err != nil {
		return g.errorResult(ctx, agentID, in, policy, prematch, start, len(in.Payload), fmt.Errorf("render: %w", err)), nil
	}
	sys := SystemPrompt(policy.Checks)
	user := UserMessage(in, prematch, projection)

	provider, model, content, err := g.router.Check(ctx, policy, sys, user)
	if err != nil {
		return g.errorResult(ctx, agentID, in, policy, prematch, start, len(in.Payload), err), nil
	}
	g.llmCall(provider, model)

	verdict, err := ParseVerdict(content)
	if err != nil {
		return g.errorResult(ctx, agentID, in, policy, prematch, start, len(in.Payload), err), nil
	}

	// Union of prematch names + LLM-reported names, deduplicated, LLM names
	// first (spec §6.4).
	verdict.MatchedPatterns = mergePatterns(verdict.MatchedPatterns, prematch)

	// Deterministic escalation (§3.4) — the LLM can never under-block
	// below the policy threshold.
	verdict = escalate(verdict, policy.Thresholds.BlockRisk)

	res := Result{
		Decision:   verdict.Decision,
		RiskLevel:  verdict.RiskLevel,
		Reason:     verdict.Reason,
		Patterns:   verdict.MatchedPatterns,
		PolicyID:   policy.ID,
		Provider:   provider,
		Model:      model,
		MessageID:  in.MessageID,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if res.Decision == DecisionSanitize {
		// Sanitize = LLM rewrite + DELIVER (Bane directive 2026-08-22;
		// spec §3.5/§12.1 amended): the guard LLM rewrites the message with
		// the FIXED neutralization prompt and the rewritten payload is
		// delivered in place of the original. The verdict's own
		// sanitized_payload field stays ignored (untrusted — we generate our
		// own rewrite); the original rides in Meta.QuarantinedPayload (base64)
		// for provenance. Rewrite unavailable → never deliver the original:
		// fail-closed → block, fail-open → deterministic quarantine notice.
		rewritten, block, rerr := g.rewritePayload(ctx, in, policy, res.Reason)
		switch {
		case block:
			res.Decision = DecisionBlock
			res.RiskLevel = RiskHigh
			res.Reason = "no benign content to preserve; blocked instead of rewritten"
			res.Patterns = append(res.Patterns, "no_benign_content")
		case rerr == nil:
			res.Sanitized = true
			res.DeliveredPayload = rewritten
			res.QuarantinedPayload = base64.StdEncoding.EncodeToString(in.Payload)
			res.Reason = "sanitized: " + res.Reason + " (payload rewritten)"
		case policy.FailClosed:
			res.Decision = DecisionBlock
			res.RiskLevel = RiskHigh
			res.Reason = "sanitize rewrite failed; fail_closed: " + rerr.Error()
		default:
			// The fail-open fallback still never delivers the original, but it
			// DEGRADES the message to a quarantine notice — which loses the
			// benign content the rewrite was supposed to preserve. Name the
			// cause on its own warn line: the reason string only says
			// "(rewrite unavailable)", which made this class invisible
			// (DF-CRIER-147: the real cause was the rewrite shape).
			g.logfWarn("guard sanitize rewrite unavailable",
				"msg", in.MessageID, "fallback", "quarantine_notice", "err", rerr.Error())
			res.Quarantined = true
			res.DeliveredPayload, res.QuarantinedPayload = quarantinePayload(in, res.Reason+" (rewrite unavailable)")
		}
	}
	g.record(res)
	g.audit(ctx, agentID, in, res, len(in.Payload))
	g.maybeEnqueueCard(agentID, in, policy, res)
	return res, nil
}

// errorResult resolves a guard error per §3.6: fail-open default
// (deliver, decision allow, risk medium), fail-closed per policy (apply
// policy.action — default block — with risk high). Errored is always set
// (record() counts it in errorsTotal).
//
// DF-CRIER-158: the deterministic pre-scan is provider-independent, so a
// guard error must never discard it. `prematch` is the already-filtered
// §6.4 evidence for this payload and is ALWAYS carried on Result.Patterns
// (an LLM outage must not erase the record of what the payload matched).
// When that evidence contains a high-confidence hit — the same subset that
// hard-blocks the §6.3 oversize path without any LLM call — the fail-open
// outcome escalates to block/high: an unreachable LLM is not a reason to
// deliver a textbook injection. A payload with no high-confidence evidence
// still fails open exactly as before.
func (g *Guard) errorResult(ctx context.Context, agentID string, in Input, policy Policy, prematch []string, start time.Time, payloadBytes int, cause error) Result {
	reason := "guard_error: " + strings.TrimPrefix(cause.Error(), "guard: ")
	strong := g.scanner.HighConfidence(prematch)
	res := Result{
		RiskLevel:  RiskMedium,
		Reason:     reason,
		Patterns:   prematch,
		Errored:    true,
		PolicyID:   policy.ID,
		MessageID:  in.MessageID,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if policy.FailClosed {
		res.RiskLevel = RiskHigh
		res.Decision = errorAction(policy.Action)
		if res.Decision == DecisionSanitize {
			res.Quarantined = true
			res.DeliveredPayload, res.QuarantinedPayload = quarantinePayload(in, reason)
		}
	} else if len(strong) > 0 {
		res.Decision = DecisionBlock
		res.RiskLevel = RiskHigh
		res.Reason = reason + "; deterministic prematch block: " + strings.Join(strong, ", ")
	} else {
		res.Decision = DecisionAllow
	}
	g.record(res)
	g.audit(ctx, agentID, in, res, payloadBytes)
	g.maybeEnqueueCard(agentID, in, policy, res)
	return res
}

// errorAction maps policy.action to the fail-closed error path decision
// (spec §3.6: allow | block | sanitize, default block).
func errorAction(a Decision) Decision {
	if a == DecisionAllow || a == DecisionSanitize {
		return a
	}
	return DecisionBlock
}

// filterPrematch drops prematch names whose attack class is disabled in
// the policy (spec §3.3: that class's prematch patterns are suppressed).
func filterPrematch(prematch []string, checks Checks, sc *PreScanner) []string {
	on := func(p *bool) bool { return p == nil || *p }
	out := prematch[:0]
	for _, name := range prematch {
		enabled := true
		switch sc.ClassOf(name) {
		case "instruction_injection":
			enabled = on(checks.InstructionInjection)
		case "jailbreak":
			enabled = on(checks.Jailbreak)
		case "masquerade":
			enabled = on(checks.Masquerade)
		case "structured_object":
			enabled = on(checks.StructuredObject)
		}
		if enabled {
			out = append(out, name)
		}
	}
	return out
}

// mergePatterns unions LLM-reported + prematch names, deduplicated, LLM
// names first (spec §6.4).
func mergePatterns(llm, prematch []string) []string {
	seen := make(map[string]bool, len(llm)+len(prematch))
	out := make([]string, 0, len(llm)+len(prematch))
	for _, p := range llm {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range prematch {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// audit writes the spec §7.3 line (level=info; level=warn when decision
// != allow or errored) plus the blocked-delivery warn line with
// event=guard_blocked. The line carries the request's correlation id when the
// delivery came over HTTP (DF-CRIER-141) — the guard's decision line is the
// choke-point record the relay/inbox/webhook lines are joined to, so it is
// annotated rather than duplicated.
func (g *Guard) audit(ctx context.Context, agentID string, in Input, r Result, payloadBytes int) {
	args := []any{
		"msg", in.MessageID,
		"target", agentID,
		"policy", r.PolicyID,
		"chan", in.SessionID + "/" + in.ThreadID,
		"kind", in.Kind,
		"decision", r.Decision,
		"risk", r.RiskLevel,
		"request_id", middleware.RequestIDFromContext(ctx),
		"provider", r.Provider,
		"model", r.Model,
		"patterns", strings.Join(r.Patterns, ","),
		"errored", r.Errored,
		"quarantined", r.Quarantined,
		"sanitized", r.Sanitized,
		"reason", r.Reason,
		"ms", r.DurationMs,
		"payload_bytes", payloadBytes,
	}
	if r.Decision != DecisionAllow || r.Errored {
		g.logfWarn("guard", args...)
	} else {
		g.logf("guard", args...)
	}
	if r.Decision == DecisionBlock {
		g.logfWarn("guard blocked", append([]any{"event", "guard_blocked"}, args...)...)
	}
}

func (g *Guard) record(r Result) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checksTotal[string(r.Decision)+"/"+string(r.RiskLevel)]++
	if r.Errored {
		g.errorsTotal++
	}
	if r.Decision == DecisionSanitize {
		g.sanitizeTotal++
	}
	if r.Decision == DecisionBlock {
		g.blockTotal++
	}
}

func (g *Guard) llmCall(provider, model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.llmCallsTotal[provider+"/"+model]++
}

// Counters is a snapshot of the §7.3 internal counters (exposed for tests).
type Counters struct {
	ChecksTotal   map[string]int64 // key "decision/risk"
	ErrorsTotal   int64
	LLMCallsTotal map[string]int64 // key "provider/model"
	SanitizeTotal int64
	BlockTotal    int64
	// KanbanWritten/Dropped/Failed are CR-FEAT-014 fire-and-forget card
	// stats (spec §8.2: full queue → drop + counter; failures counted).
	KanbanWritten int64
	KanbanDropped int64
	KanbanFailed  int64
}

// Snapshot returns the current counters.
func (g *Guard) Snapshot() Counters {
	g.mu.Lock()
	defer g.mu.Unlock()
	checks := make(map[string]int64, len(g.checksTotal))
	for k, v := range g.checksTotal {
		checks[k] = v
	}
	calls := make(map[string]int64, len(g.llmCallsTotal))
	for k, v := range g.llmCallsTotal {
		calls[k] = v
	}
	snap := Counters{
		ChecksTotal:   checks,
		ErrorsTotal:   g.errorsTotal,
		LLMCallsTotal: calls,
		SanitizeTotal: g.sanitizeTotal,
		BlockTotal:    g.blockTotal,
	}
	if g.kw != nil {
		snap.KanbanWritten, snap.KanbanDropped, snap.KanbanFailed = g.kw.stats()
	}
	return snap
}
