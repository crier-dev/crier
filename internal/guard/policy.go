package guard

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// AgentGuardConfig is the guard configuration carried on an agent
// registration (Agent.Guard, spec §4.1). At least one policy is required
// when the object is present.
type AgentGuardConfig struct {
	Policies []Policy `json:"policies"`
}

// Policy is one channel-scoped guard policy (spec §4.1).
//
// v1 (CR-FEAT-010) resolves the FIRST policy in list order and accepts an
// inline minimal policy ({model, base_url, api_key_ref, fail_closed} via
// Providers + FailClosed) or a bare policy id naming an entry in
// NamedPolicies. channel_match glob resolution, checks on/off prompt
// pruning and the CR_GUARD_DEFAULT_POLICY env land with CR-FEAT-011; the
// kanban output option lands with CR-FEAT-014 (the wire field is already
// part of the spec's §4.1 shape).
type Policy struct {
	ID           string         `json:"id"`
	ChannelMatch string         `json:"channel_match,omitempty"` // glob; "" or "*" = agent default
	Checks       Checks         `json:"checks,omitempty"`
	Thresholds   Thresholds     `json:"thresholds,omitempty"`
	Action       Decision       `json:"action,omitempty"` // error-path action; default block
	FailClosed   bool           `json:"fail_closed,omitempty"`
	Providers    []ProviderSpec `json:"providers,omitempty"` // failover order; empty = [deepseek preset]
	Kanban       *KanbanConfig  `json:"kanban,omitempty"`
}

// Checks toggles which attack classes are evaluated (false = class removed
// from the prompt and its prematch patterns suppressed, spec §3.3/§4.2).
// All default true.
type Checks struct {
	InstructionInjection *bool `json:"instruction_injection,omitempty"`
	Jailbreak            *bool `json:"jailbreak,omitempty"`
	Masquerade           *bool `json:"masquerade,omitempty"`
	StructuredObject     *bool `json:"structured_object,omitempty"`
}

// Thresholds caps permissiveness. block_risk default "high" (spec §3.4).
type Thresholds struct {
	BlockRisk RiskLevel `json:"block_risk,omitempty"`
}

// ProviderSpec is one entry in a policy's failover chain (spec §5.1).
// api_key_ref uses the env:VAR convention (resolved at call time via the
// lookupEnv indirection; never stored inline).
type ProviderSpec struct {
	Provider        string `json:"provider"` // "deepseek" | "groq" | "nvidia" | "custom"
	Model           string `json:"model,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	APIKeyRef       string `json:"api_key_ref,omitempty"` // "env:VAR" — never plaintext
	ThinkingEnabled bool   `json:"thinking_enabled,omitempty"`
}

// KanbanConfig is the CR-FEAT-014 output option (opt-in, OFF by default).
// Accepted on the wire in v1; consumed by CR-FEAT-014.
type KanbanConfig struct {
	Enabled  bool   `json:"enabled,omitempty"`
	On       string `json:"on,omitempty"` // "block" (default) | "all"
	Assignee string `json:"assignee,omitempty"`
	BoardURL string `json:"board_url,omitempty"` // originating board link for the card
}

// Validate checks an agent guard config at registration time (spec §4.1:
// 400 on violation). The checks/kanban fields are accepted but not yet
// consumed by the v1 resolution path (CR-FEAT-011/014).
func (c *AgentGuardConfig) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Policies) == 0 {
		return fmt.Errorf("guard.policies: at least one policy is required")
	}
	seen := make(map[string]bool, len(c.Policies))
	for i, p := range c.Policies {
		if err := p.validate(); err != nil {
			return fmt.Errorf("guard.policies[%d]: %w", i, err)
		}
		if seen[p.ID] {
			return fmt.Errorf("guard.policies[%d]: duplicate policy id %q", i, p.ID)
		}
		seen[p.ID] = true
	}
	return nil
}

func (p Policy) validate() error {
	if n := utf8.RuneCountInString(p.ID); n < 1 || n > 64 {
		return fmt.Errorf("policy id required, 1..64 runes (got %d)", n)
	}
	if n := utf8.RuneCountInString(p.ChannelMatch); n > 256 {
		return fmt.Errorf("policy channel_match too long (%d runes, max 256)", n)
	}
	switch p.Action {
	case "", DecisionAllow, DecisionBlock, DecisionSanitize:
	default:
		return fmt.Errorf("policy action must be allow|block|sanitize (got %q)", p.Action)
	}
	switch p.Thresholds.BlockRisk {
	case "", RiskLow, RiskMedium, RiskHigh:
	default:
		return fmt.Errorf("policy thresholds.block_risk must be low|medium|high (got %q)", p.Thresholds.BlockRisk)
	}
	if p.Kanban != nil && p.Kanban.On != "" && p.Kanban.On != "block" && p.Kanban.On != "all" {
		return fmt.Errorf("policy kanban.on must be block|all (got %q)", p.Kanban.On)
	}
	if len(p.Providers) > 5 {
		return fmt.Errorf("policy providers: at most 5 entries (got %d)", len(p.Providers))
	}
	for i, ps := range p.Providers {
		if err := ps.validate(); err != nil {
			return fmt.Errorf("policy providers[%d]: %w", i, err)
		}
	}
	return nil
}

func (ps ProviderSpec) validate() error {
	if ps.Provider == "" {
		return fmt.Errorf("provider name is required")
	}
	switch ps.Provider {
	case "custom":
		if ps.BaseURL == "" || ps.APIKeyRef == "" {
			return fmt.Errorf("custom provider requires base_url and api_key_ref")
		}
	case "deepseek":
		// Spec §5.2: the deepseek preset's contract is thinking-free; the
		// client hard-rejects thinking_enabled at config-validation time.
		if ps.ThinkingEnabled {
			return fmt.Errorf("deepseek preset forbids thinking_enabled (spec §5.2)")
		}
	default:
		if _, known := Presets[ps.Provider]; !known {
			return fmt.Errorf("unknown provider %q (want deepseek|groq|nvidia|custom)", ps.Provider)
		}
	}
	return nil
}

// BuiltinDefaultPolicy is the server-wide default policy when the target
// agent has no guard config (spec §4.2 step 3): all checks on,
// block_risk high, fail_closed false, action block, providers
// [deepseek preset].
func BuiltinDefaultPolicy() Policy {
	return Policy{
		ID:         "default",
		Thresholds: Thresholds{BlockRisk: RiskHigh},
		Action:     DecisionBlock,
		Providers:  []ProviderSpec{{Provider: "deepseek"}},
	}
}

// NamedPolicies is the in-repo named policy map (v1). A policy whose id
// names an entry here and carries no inline behavior resolves to the named
// policy — the brief's "named policy from a simple in-repo policy map".
// CR_FEAT-011 extends this with channel_match resolution and the
// CR_GUARD_DEFAULT_POLICY env server default.
var NamedPolicies = map[string]Policy{
	"default": BuiltinDefaultPolicy(),
}

// ResolvePolicy picks the policy for one delivery (spec §4.2):
//
//  1. First agent policy in list order whose channel_match matches the
//     envelope's session/thread keys wins ("" or "*" = the agent default;
//     a bare id resolves through NamedPolicies).
//  2. No agent policy matched → serverDefault (the CR_GUARD_DEFAULT_POLICY
//     env policy, or the built-in default when that is also unset).
//
// Match rule: channel_match is a Go path.Match glob (`*`, `?`, `[class]`),
// matched against `session:<session_id>` when session_id is non-empty and
// against `thread:<thread_id>` when thread_id is non-empty; a policy
// matches if the glob matches EITHER key. A bare glob (no session:/thread:
// prefix) matches session_id only. A message with no session/thread context
// matches only default policies ("", "*") — a channel-glob-only policy
// never matches an unchanneled message.
func ResolvePolicy(agentGuard *AgentGuardConfig, serverDefault Policy, sessionID, threadID string) Policy {
	if agentGuard != nil && len(agentGuard.Policies) > 0 {
		for _, p := range agentGuard.Policies {
			if !channelMatches(p.ChannelMatch, sessionID, threadID) {
				continue
			}
			if named, ok := NamedPolicies[p.ID]; ok && p.isBare() {
				return named
			}
			return p
		}
	}
	if serverDefault.ID != "" {
		return serverDefault
	}
	return BuiltinDefaultPolicy()
}

// channelMatches applies the spec §4.2 match rule to one policy glob.
func channelMatches(match, sessionID, threadID string) bool {
	if match == "" || match == "*" {
		return true // agent default policy — matches every channel
	}
	if strings.HasPrefix(match, "session:") || strings.HasPrefix(match, "thread:") {
		if sessionID != "" && globMatch(match, "session:"+sessionID) {
			return true
		}
		if threadID != "" && globMatch(match, "thread:"+threadID) {
			return true
		}
		return false
	}
	// Bare glob (no session:/thread: prefix) matches session_id only.
	return sessionID != "" && globMatch(match, sessionID)
}

// globMatch applies path.Match; a malformed pattern never matches.
func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// ParsePolicy parses and validates a JSON Policy (the
// CR_GUARD_DEFAULT_POLICY env, spec §9.1). A bare id resolves through
// NamedPolicies. A broken policy fails fast — a broken server default must
// not silently fail open.
func ParsePolicy(raw string) (Policy, error) {
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Policy{}, fmt.Errorf("guard default policy: parse: %w", err)
	}
	if err := p.validate(); err != nil {
		return Policy{}, fmt.Errorf("guard default policy: %w", err)
	}
	if named, ok := NamedPolicies[p.ID]; ok && p.isBare() {
		return named, nil
	}
	return p, nil
}

// isBare reports whether a policy carries only an id (no inline behavior).
func (p Policy) isBare() bool {
	return p.ChannelMatch == "" &&
		p.Thresholds.BlockRisk == "" &&
		p.Action == "" &&
		!p.FailClosed &&
		len(p.Providers) == 0 &&
		p.Kanban == nil &&
		p.Checks == (Checks{})
}
