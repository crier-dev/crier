package addressing

import "fmt"

// ErrorInvalidAddress is the ONE code every parse failure carries
// (specs/CHAT-ADDRESSING.md §4). A malformed address is never reinterpreted
// and never dropped; it is this refusal, naming the offending token and the
// rule it violated.
const ErrorInvalidAddress = "INVALID_ADDRESS"

// AcceptedForms is the spelling list every shape refusal quotes (§4): the
// forms the grammar accepts, so a caller reading the 400 knows what to type.
const AcceptedForms = "@agent, @team:x, @cap:y, @ns/<name>/*, #session"

// The rule vocabulary. Every *Error names exactly one member; the set is
// closed and ValidRule is the predicate the tests hold it to.
const (
	// RuleEmptyToken — a bare sigil: `@` or `#` with no token after it. §1.3:
	// an address that names nothing is not an address.
	RuleEmptyToken = "EMPTY_TOKEN"
	// RuleNotAnAddress — a token that starts with neither `@` nor `#`.
	RuleNotAnAddress = "NOT_AN_ADDRESS"
	// RuleBadTokenCharset — an ident outside [A-Za-z0-9_][A-Za-z0-9_.-]*.
	RuleBadTokenCharset = "BAD_TOKEN_CHARSET"
	// RuleAddressTooLong — over maxAddressLength (256, sigil included).
	RuleAddressTooLong = "ADDRESS_TOO_LONG"
	// RuleEmptyAddressList — a list that names nothing at all.
	RuleEmptyAddressList = "EMPTY_ADDRESS_LIST"
	// RuleLeadingSeparator — a list beginning with `,` or `;`.
	RuleLeadingSeparator = "LEADING_SEPARATOR"
	// RuleTrailingSeparator — a list ending with `,` or `;` (trailing junk).
	RuleTrailingSeparator = "TRAILING_SEPARATOR"
	// RuleTooManyAddresses — over maxAddressesPerWrite (64). The refusal names
	// the limit AND the count received; the list is never truncated.
	RuleTooManyAddresses = "TOO_MANY_ADDRESSES"
	// RuleMultipleSessions — two `#` rooms in one write. §1.3: one write has at
	// most one room.
	RuleMultipleSessions = "MULTIPLE_SESSIONS"
	// RuleEmptyGroupName — `@team:` with no name.
	RuleEmptyGroupName = "EMPTY_GROUP_NAME"
	// RuleEmptyCapabilityName — `@cap:` with no name.
	RuleEmptyCapabilityName = "EMPTY_CAPABILITY_NAME"
	// RuleEmptyNamespace — `@ns/` with no wildcard and no name.
	RuleEmptyNamespace = "EMPTY_NAMESPACE"
	// RuleInvalidNamespaceName — a namespace name the server refuses: the
	// grammar's ident charset is wider than the server's
	// ^[a-z0-9][a-z0-9_-]{0,63}$ (§1.1).
	RuleInvalidNamespaceName = "INVALID_NAMESPACE_NAME"
	// RuleEmptyNamespaceTarget — `@ns/<name>/` with no wildcard or ident.
	RuleEmptyNamespaceTarget = "EMPTY_NAMESPACE_TARGET"
	// RuleEmptyRemoteInstance — `@/<agent>`: a remote form with no instance.
	RuleEmptyRemoteInstance = "EMPTY_REMOTE_INSTANCE"
	// RuleEmptyRemoteAgent — `@<instance>/`: a remote form with no agent.
	RuleEmptyRemoteAgent = "EMPTY_REMOTE_AGENT"
)

// Rules is the closed rule vocabulary in a stable order. It is what
// ValidRule reads and what every refusal can be checked against.
var Rules = []string{
	RuleEmptyToken,
	RuleNotAnAddress,
	RuleBadTokenCharset,
	RuleAddressTooLong,
	RuleEmptyAddressList,
	RuleLeadingSeparator,
	RuleTrailingSeparator,
	RuleTooManyAddresses,
	RuleMultipleSessions,
	RuleEmptyGroupName,
	RuleEmptyCapabilityName,
	RuleEmptyNamespace,
	RuleInvalidNamespaceName,
	RuleEmptyNamespaceTarget,
	RuleEmptyRemoteInstance,
	RuleEmptyRemoteAgent,
}

// ValidRule reports whether r is a member of the closed rule vocabulary.
func ValidRule(r string) bool {
	for _, want := range Rules {
		if r == want {
			return true
		}
	}
	return false
}

// Error is the typed parse failure: the INVALID_ADDRESS code, the offending
// token verbatim, and the rule it violated. It is deliberately NOT a bare
// string — a caller branches on Rule without parsing prose, the same way the
// shipped NO_CAPABLE_AGENT refusal is branched on.
type Error struct {
	// Code is always ErrorInvalidAddress. It is a field so the §4 body can be
	// built from the error alone (Refusal).
	Code string
	// Rule is the violated rule, one of Rules.
	Rule string
	// Token is the offending address or address-list token, verbatim.
	Token string
	// Detail explains the rule to a human; it is never empty.
	Detail string
}

// newError builds a refusal. It is the ONLY constructor: no site may return a
// fmt.Errorf from the parser.
func newError(rule, token, detail string) *Error {
	return &Error{Code: ErrorInvalidAddress, Rule: rule, Token: token, Detail: detail}
}

// Error implements error: the code, the rule, the token and the detail, so a
// log line alone says which rule refused what.
func (e *Error) Error() string {
	return fmt.Sprintf("addressing: %s: %q violates %s: %s", e.Code, e.Token, e.Rule, e.Detail)
}

// Refusal is the §4 machine-readable refusal body. Field order is the order
// the spec prints, and encoding/json preserves a struct's field order.
type Refusal struct {
	Error   string `json:"error"`
	Address string `json:"address"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// Refusal renders the Error as the wire body of §4:
//
//	{"error":"INVALID_ADDRESS","address":"@team:","reason":"EMPTY_GROUP_NAME","detail":"…"}
//
// It is the ONE place the body is built, so the handler cannot invent a second
// shape.
func (e *Error) Refusal() Refusal {
	return Refusal{
		Error:   ErrorInvalidAddress,
		Address: e.Token,
		Reason:  e.Rule,
		Detail:  e.Detail,
	}
}

// shapeDetail appends the accepted forms to a rule explanation — the §4 body's
// `detail` for every shape refusal.
func shapeDetail(explain string) string {
	return explain + "; accepted forms: " + AcceptedForms
}
