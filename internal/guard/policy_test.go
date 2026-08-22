package guard

import (
	"strings"
	"testing"
)

// ── channel_match glob matching (spec §4.2) ─────────────────────────────

func TestChannelMatches_GlobForms(t *testing.T) {
	cases := []struct {
		name      string
		match     string
		sessionID string
		threadID  string
		want      bool
	}{
		// Exact prefixed globs.
		{"exact session", "session:ops-42", "ops-42", "", true},
		{"exact session mismatch", "session:ops-42", "ops-43", "", false},
		{"exact thread", "thread:thr-7", "", "thr-7", true},
		// Wildcard / prefix.
		{"session wildcard", "session:ops-*", "ops-42", "", true},
		{"session wildcard suffix", "session:ops-*", "ops-42", "thr-7", true},
		{"session prefix no match", "session:ops-*", "other-1", "", false},
		{"session question", "session:op?-42", "ops-42", "", true},
		{"session char class", "session:op[sx]-42", "ops-42", "", true},
		{"session char class no", "session:op[sx]-42", "opa-42", "", false},
		// Either key matches (spec §4.2: matches session: OR thread:).
		{"either key thread hit", "session:ops-*", "", "ops-9", false}, // session glob vs thread key: no
		{"thread glob vs session key", "thread:thr-*", "thr-1", "", false},
		{"thread wildcard", "thread:thr-*", "", "thr-9", true},
		{"both keys session match", "session:ops-*", "ops-1", "thr-2", true},
		{"both keys thread match", "thread:thr-*", "ops-1", "thr-2", true},
		// Bare glob (no prefix) matches session_id only (spec §4.2).
		{"bare glob session", "chat-*", "chat-42", "", true},
		{"bare glob no session", "chat-*", "", "chat-42", false},
		{"bare glob no match", "chat-*", "other-1", "", false},
		{"bare exact", "chat-42", "chat-42", "", true},
		// Default policies match everything, including unchanneled.
		{"empty match any", "", "x", "", true},
		{"empty match unchanneled", "", "", "", true},
		{"star match any", "*", "x", "", true},
		{"star match unchanneled", "*", "", "", true},
		// Unchanneled message: channel-glob-only policies never match.
		{"unchanneled session glob", "session:ops-*", "", "", false},
		{"unchanneled bare glob", "chat-*", "", "", false},
		// Malformed pattern: never matches, never panics.
		{"malformed glob", "session:[", "ops-1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelMatches(tc.match, tc.sessionID, tc.threadID); got != tc.want {
				t.Fatalf("channelMatches(%q, %q, %q) = %v, want %v",
					tc.match, tc.sessionID, tc.threadID, got, tc.want)
			}
		})
	}
}

// ── resolution order (spec §4.2, tests #13-16) ──────────────────────────

func TestResolvePolicy_ExactBeatsGlobBeatsDefault(t *testing.T) {
	cfg := &AgentGuardConfig{Policies: []Policy{
		{ID: "strict", ChannelMatch: "session:ops-*", FailClosed: true},
		{ID: "default", ChannelMatch: "*"},
	}}
	// session ops-42 → strict (channel override, listed first).
	if p := ResolvePolicy(cfg, Policy{}, "ops-42", ""); p.ID != "strict" {
		t.Fatalf("ops-42: policy = %q, want strict", p.ID)
	}
	// session other-1 → default (agent default).
	if p := ResolvePolicy(cfg, Policy{}, "other-1", ""); p.ID != "default" {
		t.Fatalf("other-1: policy = %q, want default", p.ID)
	}
	// no session → default.
	if p := ResolvePolicy(cfg, Policy{}, "", ""); p.ID != "default" {
		t.Fatalf("no session: policy = %q, want default", p.ID)
	}
	// thread key with no session → strict does NOT match (session glob),
	// default does.
	if p := ResolvePolicy(cfg, Policy{}, "", "ops-42"); p.ID != "default" {
		t.Fatalf("thread-only: policy = %q, want default", p.ID)
	}
}

func TestResolvePolicy_ThreadKeyWhenSessionEmpty(t *testing.T) {
	// Spec test #14: a thread: glob matches when session_id is empty but
	// thread_id is present.
	cfg := &AgentGuardConfig{Policies: []Policy{
		{ID: "t-strict", ChannelMatch: "thread:ops-*", FailClosed: true},
		{ID: "t-default", ChannelMatch: "*"},
	}}
	if p := ResolvePolicy(cfg, Policy{}, "", "ops-9"); p.ID != "t-strict" {
		t.Fatalf("thread ops-9: policy = %q, want t-strict", p.ID)
	}
	if p := ResolvePolicy(cfg, Policy{}, "", "other-1"); p.ID != "t-default" {
		t.Fatalf("thread other-1: policy = %q, want t-default", p.ID)
	}
}

func TestResolvePolicy_NoAgentPolicyFallsThrough(t *testing.T) {
	// Spec test #15: no agent policy matched → server default → built-in.
	serverDefault := Policy{ID: "server-default", FailClosed: true}
	// Agent with only channel policies that do not match.
	cfg := &AgentGuardConfig{Policies: []Policy{
		{ID: "only-chan", ChannelMatch: "session:ops-*"},
	}}
	if p := ResolvePolicy(cfg, serverDefault, "other-1", ""); p.ID != "server-default" {
		t.Fatalf("unmatched channel: policy = %q, want server-default", p.ID)
	}
	// No agent config at all → server default.
	if p := ResolvePolicy(nil, serverDefault, "", ""); p.ID != "server-default" {
		t.Fatalf("nil agent: policy = %q, want server-default", p.ID)
	}
	// Zero server default → built-in default.
	builtin := ResolvePolicy(nil, Policy{}, "ops-1", "")
	if builtin.ID != "default" {
		t.Fatalf("zero server default: policy = %q, want built-in default", builtin.ID)
	}
}

func TestResolvePolicy_NoSessionNoThreadChannelOnly(t *testing.T) {
	// Spec test #16: unchanneled message + channel-glob-only policies →
	// only default policies apply; here none exists → server default.
	cfg := &AgentGuardConfig{Policies: []Policy{
		{ID: "chan-1", ChannelMatch: "session:ops-*"},
		{ID: "chan-2", ChannelMatch: "thread:thr-*"},
	}}
	if p := ResolvePolicy(cfg, Policy{ID: "sd"}, "", ""); p.ID != "sd" {
		t.Fatalf("unchanneled: policy = %q, want server default", p.ID)
	}
	// With an agent default present it wins for unchanneled messages.
	cfg2 := &AgentGuardConfig{Policies: []Policy{
		{ID: "chan-1", ChannelMatch: "session:ops-*"},
		{ID: "agent-default", ChannelMatch: "*"},
	}}
	if p := ResolvePolicy(cfg2, Policy{ID: "sd"}, "", ""); p.ID != "agent-default" {
		t.Fatalf("unchanneled with default: policy = %q, want agent-default", p.ID)
	}
}

func TestResolvePolicy_BareIDResolvesNamed(t *testing.T) {
	cfg := &AgentGuardConfig{Policies: []Policy{{ID: "default"}}}
	p := ResolvePolicy(cfg, Policy{}, "any", "")
	if p.ID != "default" || p.FailClosed || p.Providers[0].Provider != "deepseek" {
		t.Fatalf("bare id resolution wrong: %+v", p)
	}
}

// ── CR_GUARD_DEFAULT_POLICY parsing (spec §9.1) ─────────────────────────

func TestParsePolicy(t *testing.T) {
	// Valid inline policy.
	p, err := ParsePolicy(`{"id":"env-default","fail_closed":true,"providers":[{"provider":"groq"}]}`)
	if err != nil {
		t.Fatalf("ParsePolicy valid: %v", err)
	}
	if p.ID != "env-default" || !p.FailClosed || p.Providers[0].Provider != "groq" {
		t.Fatalf("parsed policy wrong: %+v", p)
	}
	// Bare id resolves through NamedPolicies.
	p, err = ParsePolicy(`{"id":"default"}`)
	if err != nil {
		t.Fatalf("ParsePolicy bare id: %v", err)
	}
	if p.ID != "default" || p.Providers[0].Provider != "deepseek" {
		t.Fatalf("bare id policy wrong: %+v", p)
	}
	// Invalid JSON → error.
	if _, err := ParsePolicy(`{not json`); err == nil {
		t.Fatal("ParsePolicy garbage: want error")
	}
	// Invalid policy → error (bad action).
	if _, err := ParsePolicy(`{"id":"x","action":"nuke"}`); err == nil {
		t.Fatal("ParsePolicy bad action: want error")
	}
	// deepseek preset + thinking → hard error.
	if _, err := ParsePolicy(`{"id":"x","providers":[{"provider":"deepseek","thinking_enabled":true}]}`); err == nil {
		t.Fatal("ParsePolicy deepseek thinking: want error")
	}
}

// ── thresholds mapping to verdict decisions (spec §3.4) ─────────────────

func TestResolvePolicy_ThresholdsFlowToEscalation(t *testing.T) {
	// block_risk: medium caps permissiveness — an allow/medium LLM verdict
	// is escalated to block (spec §3.4 row 1 with block_risk=medium).
	cfg := &AgentGuardConfig{Policies: []Policy{
		{ID: "med", ChannelMatch: "*", Thresholds: Thresholds{BlockRisk: RiskMedium}},
	}}
	p := ResolvePolicy(cfg, Policy{}, "sess-1", "")
	if p.Thresholds.BlockRisk != RiskMedium {
		t.Fatalf("thresholds not carried: %+v", p.Thresholds)
	}
	v := escalate(Verdict{Decision: DecisionAllow, RiskLevel: RiskMedium}, p.Thresholds.BlockRisk)
	if v.Decision != DecisionBlock {
		t.Fatalf("allow/medium with block_risk=medium = %s, want block", v.Decision)
	}
	if !strings.HasPrefix(v.Reason, "escalated:") {
		t.Fatalf("reason = %q, want escalated prefix", v.Reason)
	}
}
