package chat

import (
	"regexp"
	"strings"
)

// addresseePattern matches the address forms a body may carry (CR-CHAT-004,
// specs/CHAT-ADDRESSING.md §1.1): the RESERVED prefixed forms first —
// `@team:x`, a NAMED group (D8), and `@cap:y`, a capability target — then the
// bare `@agent` form. The reserved prefixes are matched BEFORE the bare form
// for the same reason §2.1.1 fixes the order: `@ns/` wins over the remote
// form there, and `@team:`/`@cap:` win over "an agent literally named team"
// here. The kind of a match is read from which alternative matched.
var addresseePattern = regexp.MustCompile(
	`@(?:(team|cap):([A-Za-z0-9_][A-Za-z0-9_.-]*)|([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*))`)

// Mention kinds — the alternative a match resolved to. An agent mention is
// bare; a team mention is the NAMED-group form `@team:x` (a curated roster,
// CR-CHAT-022); a capability mention is `@cap:y` (the dynamic pool, D8).
const (
	MentionAgent      = "agent"
	MentionTeam       = "team"
	MentionCapability = "capability"
)

// Mention is ONE address token parsed out of a body: its kind (agent, team or
// capability) and the ref (the agent id, the group name or the capability
// name). A tag is ADDRESSING, never an action (D12) — parsing resolves who a
// body NAMES and nothing else.
type Mention struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// ParseMentions extracts every address token from a body, in order of
// appearance, without duplicates (deduplicated by kind+ref). Empty for a body
// with no tags.
func ParseMentions(body string) []Mention {
	matches := addresseePattern.FindAllStringSubmatch(body, -1)
	var out []Mention
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		kind, ref := MentionAgent, m[3]
		switch m[1] {
		case "team":
			kind, ref = MentionTeam, m[2]
		case "cap":
			kind, ref = MentionCapability, m[2]
		}
		if ref == "" || seen[kind+"\x00"+ref] {
			continue
		}
		seen[kind+"\x00"+ref] = true
		out = append(out, Mention{Kind: kind, Ref: ref})
	}
	return out
}

// ParseAddressees extracts the @agent tokens from a body, in order of
// appearance, without duplicates. The RESERVED forms (`@team:x`, `@cap:y`)
// are NOT agent addressees — a group name and a capability name are not agent
// ids (D8) — and are returned by ParseMentions instead. Empty for a body with
// no agent tags.
func ParseAddressees(body string) []string {
	var out []string
	for _, m := range ParseMentions(body) {
		if m.Kind == MentionAgent {
			out = append(out, m.Ref)
		}
	}
	return out
}

// Classify derives the effective kind of a message. An explicit Kind of task
// always wins (a TASK is only ever created deliberately — tagging alone is
// never enough); otherwise tags in the body make it ADDRESSED; otherwise it
// is PLAIN.
//
// The rule encoded here — and tested — is that an ADDRESSED message whose
// body reads like an instruction ("please fix X") is STILL just an addressed
// message. Nothing in this package executes anything; only a TASK may create
// work, and only via an explicit kind.
func Classify(body string, explicit Kind) (Kind, error) {
	switch explicit {
	case KindTask, KindPlain, KindAddressed:
		return explicit, nil
	case "":
		// No explicit kind: derive from the body.
	default:
		return "", ErrInvalidKind
	}
	if len(ParseAddressees(body)) > 0 {
		return KindAddressed, nil
	}
	return KindPlain, nil
}

// EnsureUnique returns the addressee list with duplicates removed and
// whitespace trimmed; empty tokens are dropped. Helper for callers that
// collect addressees from free-form input.
func EnsureUnique(addressees []string) []string {
	var out []string
	seen := make(map[string]bool, len(addressees))
	for _, a := range addressees {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}
