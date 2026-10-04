// Package addressing implements the crier tagging grammar and its parser —
// specs/CHAT-ADDRESSING.md §1 (CR-CHAT-004).
//
// A tag addresses something; it never does anything. §2.5 (D12) is the rule
// this package is shaped by: parsing an address is MECHANICAL. The package
// therefore exposes exactly three verbs — Parse, ParseList, ParseWrite — and
// produces only address tokens plus their kinds. It does not resolve an
// address, does not look anything up, does not fan out and does not deliver;
// resolution, authorization and dispatch live in their own packages (the
// shipped deliver path and the permission model).
//
// The second rule is that no failure is silent. Every malformed address is an
// *Error carrying the offending token and the rule it violated, and the
// grammar's bounds (maxAddressLength, maxAddressesPerWrite) are refusals, not
// truncations: a silently truncated list would deliver to the WRONG subset.
//
// What this package deliberately does NOT do, and where each thing lives:
//
//   - resolve a tag to a target and authorize it (§4 steps 2..4) — that is the
//     delivery path plus internal/permissions;
//   - expand `@ns/…/*` — the wildcard is PARSED here and resolved elsewhere;
//   - apply precedence among bare tokens (§2.1.2) or refuse an ambiguous one
//     (409 AMBIGUOUS_ADDRESS) — both need the registries;
//   - know whether a named group, capability or session exists — the parser is
//     a grammar, not a directory.
//
// The remote form `@instance/agent` is parsed (§1.5) and nothing more: the
// shadow-principal resolution and the 404 UNKNOWN_REMOTE_ADDRESS refusal are
// owned by specs/CHAT-FEDERATION.md.
//
// One documented consequence of §1.1's ns_name rule: a namespace NAME is
// validated against the server's stricter ^[a-z0-9][a-z0-9_-]{0,63}$ (see
// ValidNamespaceName) rather than the wider ident charset, so `@ns/ACME/*` is
// a named refusal (INVALID_NAMESPACE_NAME) even though `.` or an upper-case
// letter is a legal ident character. The spec says the server enforces the
// stricter rule, so the parser enforces it here rather than letting an
// unservable name reach resolution.
package addressing

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The §1.1 bounds.
const (
	// MaxAddressesPerWrite bounds one write's fan-out. A longer list is
	// refused (naming the limit and the count received), never truncated.
	MaxAddressesPerWrite = 64
	// MaxAddressLength bounds one address, sigil included.
	MaxAddressLength = 256
	// MaxResolvedTargets bounds a wildcard expansion. It is declared here
	// because §1.1 declares it, but the parser does not expand anything — the
	// bound is enforced where `@ns/…/*` is resolved.
	MaxResolvedTargets = 256
)

// identShape is the ident charset, spelled once so every refusal quotes the
// same rule.
const identShape = "[A-Za-z0-9_][A-Za-z0-9_.-]*"

// nsNameRE is the stricter namespace-name rule §1.1 attaches to ns_name. It is
// the SAME rule internal/namespace applies to a declared namespace name
// (namespace.nameRE), restated here so the parser can refuse an unservable
// name at parse time; the two must not drift.
var nsNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Kind is the closed set of address kinds a tag can be. Each is a TARGET kind
// — never an action. There is no "task" or "action" kind, because a tag is
// addressing and never a command (§2.5, D12).
type Kind string

const (
	// KindAgent — `@agent`, ONE registered agent id.
	KindAgent Kind = "agent"
	// KindTeam — `@team:x`, a NAMED, CURATED roster (§1.4).
	KindTeam Kind = "team"
	// KindCapability — `@cap:y`, a dynamic capability pool resolved to one
	// live holder (§1.4, §3).
	KindCapability Kind = "capability"
	// KindNamespace — `@ns/*`, `@ns/<name>`, `@ns/<name>/*`,
	// `@ns/<name>/<agent>` (§1.2). `@ns/` is reserved and wins over the
	// remote form (§2.1.1).
	KindNamespace Kind = "namespace"
	// KindRemote — `@<instance>/<agent>`, ONE agent on a federated peer
	// (§1.5).
	KindRemote Kind = "remote"
	// KindSession — `#session`, a ROOM rather than a recipient (§1.2).
	KindSession Kind = "session"
)

// Kinds is the closed kind vocabulary in a stable order.
var Kinds = []Kind{KindAgent, KindTeam, KindCapability, KindNamespace, KindRemote, KindSession}

// ValidKind reports whether k is a member of the closed kind vocabulary.
func ValidKind(k Kind) bool {
	for _, want := range Kinds {
		if k == want {
			return true
		}
	}
	return false
}

// ValidNamespaceName reports whether name satisfies the server's namespace-name
// rule (§1.1): ^[a-z0-9][a-z0-9_-]{0,63}$.
func ValidNamespaceName(name string) bool { return nsNameRE.MatchString(name) }

// Address is ONE parsed tag: its kind plus the parts that kind names. The
// zero Address is not a valid tag (Kind is empty), which is what makes "did
// this parse?" answerable without a second boolean.
type Address struct {
	// Kind is the address kind.
	Kind Kind
	// Ref is the primary identifier the address names: the agent id
	// (`@agent`), the group name (`@team:x`), the capability name (`@cap:y`),
	// the session id (`#session`), the namespace name (`@ns/<name>`; empty for
	// the bare `@ns/*`) or the AGENT id of a remote target (`@instance/agent`).
	Ref string
	// Instance is the peer instance name of a remote address; empty for every
	// other kind.
	Instance string
	// Member is a namespace address's trailing element: "*" for a wildcard
	// (`@ns/*`, `@ns/<name>/*`), an agent ident (`@ns/<name>/<agent>`), or
	// empty for a bare namespace reference (`@ns/<name>`). Empty for every
	// other kind.
	Member string
}

// String renders the CANONICAL form of the address (§2.2's prefixed wire
// form): the re-render of a parsed address is stable, which is what makes an
// address list round-trip.
func (a Address) String() string {
	switch a.Kind {
	case KindAgent:
		return "@" + a.Ref
	case KindTeam:
		return "@team:" + a.Ref
	case KindCapability:
		return "@cap:" + a.Ref
	case KindSession:
		return "#" + a.Ref
	case KindRemote:
		return "@" + a.Instance + "/" + a.Ref
	case KindNamespace:
		if a.Ref == "" {
			// The only Ref-less namespace form is the top-level wildcard.
			return "@ns/*"
		}
		if a.Member == "" {
			return "@ns/" + a.Ref
		}
		return "@ns/" + a.Ref + "/" + a.Member
	}
	return ""
}

// Wildcard reports whether the address is a namespace wildcard (`@ns/*` or
// `@ns/<name>/*`) — the ONE form that expands (§1.2). The parser reports it;
// the expansion itself happens where the namespace registry is reachable.
func (a Address) Wildcard() bool { return a.Kind == KindNamespace && a.Member == "*" }

// List is an ordered address list (a "to" line). MaxAddressesPerWrite bounds
// its length.
type List []Address

// String renders the canonical list: addresses joined with ", ".
func (l List) String() string {
	parts := make([]string, len(l))
	for i, a := range l {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// Equal reports whether two lists carry the same addresses in the same order.
// It is the round-trip witness.
func (l List) Equal(other List) bool {
	if len(l) != len(other) {
		return false
	}
	for i := range l {
		if l[i] != other[i] {
			return false
		}
	}
	return true
}

// Write is an ADDRESSED WRITE (§1.1): a room plus recipients, or recipients
// only. It carries at most one Session and at most MaxAddressesPerWrite
// addresses.
type Write struct {
	// Session is the room the write appends to, or nil when the write names no
	// room. `#session` is one axis; the addresses are the other (§2.3).
	Session *Address
	// Addresses are the recipients, in the order they were written.
	Addresses List
}

// String renders the canonical write: the session (if any) followed by the
// addresses, space-separated (§1.3's `#build-plan @atlas @cap:research`).
func (w Write) String() string {
	parts := make([]string, 0, len(w.Addresses)+1)
	if w.Session != nil {
		parts = append(parts, w.Session.String())
	}
	for _, a := range w.Addresses {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, " ")
}

// Parse parses ONE address. Surrounding whitespace is insignificant; anything
// else that is not exactly one of the §1.1 forms is a named *Error.
func Parse(s string) (Address, error) {
	return parseToken(strings.TrimSpace(s))
}

// ParseList parses an address LIST (§1.1): addresses separated by "," or ";"
// or whitespace, at most MaxAddressesPerWrite of them. It accepts every kind —
// including `#session`, which a caller wanting the §1.3 one-room rule should
// parse with ParseWrite instead.
func ParseList(s string) (List, error) {
	toks, err := splitList(s)
	if err != nil {
		return nil, err
	}
	if len(toks) > MaxAddressesPerWrite {
		return nil, tooMany(len(toks), strings.TrimSpace(s))
	}
	list := make(List, 0, len(toks))
	for _, tok := range toks {
		a, err := parseToken(tok)
		if err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, nil
}

// ParseWrite parses an ADDRESSED WRITE (§1.1, §1.3): an optional single
// `#session` room plus an address list, or an address list alone. A second
// `#` in one write is refused (MULTIPLE_SESSIONS) — one write has at most one
// room, because a message in two rooms is two messages.
func ParseWrite(s string) (Write, error) {
	toks, err := splitList(s)
	if err != nil {
		return Write{}, err
	}
	var w Write
	for _, tok := range toks {
		a, err := parseToken(tok)
		if err != nil {
			return Write{}, err
		}
		if a.Kind == KindSession {
			if w.Session != nil {
				return Write{}, newError(RuleMultipleSessions, tok, shapeDetail(fmt.Sprintf(
					"the write already names the room %s; one write has at most one room (§1.3)", w.Session)))
			}
			session := a
			w.Session = &session
			continue
		}
		w.Addresses = append(w.Addresses, a)
	}
	if len(w.Addresses) > MaxAddressesPerWrite {
		return Write{}, tooMany(len(w.Addresses), w.Addresses.String())
	}
	return w, nil
}

// tooMany builds the §1.1 TOO_MANY_ADDRESSES refusal, naming the limit AND the
// count received so the caller can see the list was refused, not truncated.
func tooMany(count int, token string) *Error {
	return newError(RuleTooManyAddresses, clip(token, MaxAddressLength), fmt.Sprintf(
		"the list carries %d addresses; maxAddressesPerWrite is %d (§1.1) — the list is refused, never truncated",
		count, MaxAddressesPerWrite))
}

// splitList splits a list into its address tokens. A run of separators
// separates two addresses; a LEADING or TRAILING separator has no address to
// separate and is refused, as is a list that names nothing at all.
func splitList(s string) ([]string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, newError(RuleEmptyAddressList, s, shapeDetail("the address list names nothing"))
	}
	first, _ := utf8.DecodeRuneInString(trimmed)
	if first == ',' || first == ';' {
		return nil, newError(RuleLeadingSeparator, trimmed, shapeDetail("the address list starts with a separator and no address"))
	}
	last, _ := utf8.DecodeRuneInString(trimmed[len(trimmed)-1:])
	if last == ',' || last == ';' {
		return nil, newError(RuleTrailingSeparator, trimmed, shapeDetail("the address list ends with a separator and no address"))
	}
	return strings.FieldsFunc(trimmed, isSeparator), nil
}

// isSeparator reports whether r separates two addresses in a list.
func isSeparator(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) }

// parseToken classifies ONE token into an address or a named refusal.
func parseToken(tok string) (Address, error) {
	if tok == "" {
		return Address{}, newError(RuleEmptyToken, tok, shapeDetail("the token names nothing"))
	}
	if len(tok) > MaxAddressLength {
		return Address{}, newError(RuleAddressTooLong, clip(tok, MaxAddressLength), fmt.Sprintf(
			"the address is %d characters; maxAddressLength is %d (§1.1)", len(tok), MaxAddressLength))
	}
	switch tok[0] {
	case '#':
		return parseSession(tok)
	case '@':
		return parseAt(tok)
	default:
		return Address{}, newError(RuleNotAnAddress, tok, shapeDetail("an address starts with @ (a target) or # (a session)"))
	}
}

// parseSession parses `#session` (§1.1 session_ref).
func parseSession(tok string) (Address, error) {
	rest := tok[1:]
	if rest == "" {
		return Address{}, newError(RuleEmptyToken, tok, shapeDetail("`#` with no session id"))
	}
	if !isIdent(rest) {
		return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("a session id must match "+identShape))
	}
	return Address{Kind: KindSession, Ref: rest}, nil
}

// parseAt parses every `@`-prefixed form. The reserved prefixes are matched in
// the order §2.1.1 fixes — `@ns/` is tested BEFORE the remote form, so the
// reserved namespace prefix always wins.
func parseAt(tok string) (Address, error) {
	body := tok[1:]
	if body == "" {
		return Address{}, newError(RuleEmptyToken, tok, shapeDetail("`@` with no target"))
	}
	switch {
	case strings.HasPrefix(body, "team:"):
		name := strings.TrimPrefix(body, "team:")
		if name == "" {
			return Address{}, newError(RuleEmptyGroupName, tok, shapeDetail("`@team:` names no group"))
		}
		if !isIdent(name) {
			return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("a group name must match "+identShape))
		}
		return Address{Kind: KindTeam, Ref: name}, nil
	case strings.HasPrefix(body, "cap:"):
		name := strings.TrimPrefix(body, "cap:")
		if name == "" {
			return Address{}, newError(RuleEmptyCapabilityName, tok, shapeDetail("`@cap:` names no capability"))
		}
		if !isIdent(name) {
			return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("a capability name must match "+identShape))
		}
		return Address{Kind: KindCapability, Ref: name}, nil
	case strings.HasPrefix(body, "ns/"):
		return parseNamespace(tok, strings.TrimPrefix(body, "ns/"))
	default:
		return parseAgentOrRemote(tok, body)
	}
}

// parseNamespace parses the `@ns/` forms — `@ns/*`, `@ns/<name>`,
// `@ns/<name>/*`, `@ns/<name>/<agent>` (§1.1 ns_ref, §1.2). rest is everything
// after the reserved `@ns/` prefix.
func parseNamespace(tok, rest string) (Address, error) {
	if rest == "*" {
		// @ns/* — every addressable thing in the sender's own namespace.
		return Address{Kind: KindNamespace, Member: "*"}, nil
	}
	if rest == "" {
		return Address{}, newError(RuleEmptyNamespace, tok, shapeDetail("`@ns/` names no namespace"))
	}
	name, member, hasMember := strings.Cut(rest, "/")
	if !ValidNamespaceName(name) {
		return Address{}, newError(RuleInvalidNamespaceName, tok, fmt.Sprintf(
			"namespace name %q must match %s (§1.1)", name, nsNameRE))
	}
	if !hasMember {
		// @ns/<name> — one named namespace, every addressable thing in it.
		return Address{Kind: KindNamespace, Ref: name}, nil
	}
	if member == "" {
		return Address{}, newError(RuleEmptyNamespaceTarget, tok, shapeDetail(fmt.Sprintf(
			"`@ns/%s/` names no wildcard or agent", name)))
	}
	if member != "*" && !isIdent(member) {
		return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("a namespace target must be * or an ident"))
	}
	return Address{Kind: KindNamespace, Ref: name, Member: member}, nil
}

// parseAgentOrRemote parses the two forms that share the bare `@instance` or
// `@instance/agent` shape: an agent_ref when there is no `/`, a remote_ref
// when there is one. body is everything after the `@`.
func parseAgentOrRemote(tok, body string) (Address, error) {
	if inst, agent, ok := strings.Cut(body, "/"); ok {
		if inst == "" {
			return Address{}, newError(RuleEmptyRemoteInstance, tok, shapeDetail("`@/<agent>` names no instance"))
		}
		if agent == "" {
			return Address{}, newError(RuleEmptyRemoteAgent, tok, shapeDetail(fmt.Sprintf("`@%s/` names no agent", inst)))
		}
		if !isIdent(inst) {
			return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("an instance name must match "+identShape))
		}
		if !isIdent(agent) {
			return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("an agent id must match "+identShape))
		}
		return Address{Kind: KindRemote, Instance: inst, Ref: agent}, nil
	}
	if !isIdent(body) {
		return Address{}, newError(RuleBadTokenCharset, tok, shapeDetail("an agent id must match "+identShape))
	}
	return Address{Kind: KindAgent, Ref: body}, nil
}

// isIdent reports whether s matches §1.1's ident: an ident_start followed by
// ident_char*.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isIdentStart(r) {
				return false
			}
			continue
		}
		if !isIdentChar(r) {
			return false
		}
	}
	return true
}

func isIdentStart(r rune) bool {
	return r == '_' ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9')
}

func isIdentChar(r rune) bool { return isIdentStart(r) || r == '-' || r == '.' }

// clip shortens s to at most max runes, appending an ellipsis when it cuts, so
// a refusal echoing an over-long token stays bounded and valid UTF-8.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
