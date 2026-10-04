// Package permissions implements the crier permission model — principals,
// bindings, the four namespace roles, grants and the DELIVERY ACL — per
// specs/CHAT-PERMISSIONS.md (CR-CHAT-003).
//
// The single rule the package exists to make executable is §3.2: "the
// permission model must let me talk to MY agents but NOT to YOURS.
// Cross-owner is DEFAULT-DENY." Everything else here is machinery around it.
//
// Three statements are load-bearing and are stated in the code, not implied:
//
//   - Default-deny in the AUTHORIZATION sense: if no live grant, no ownership
//     rule and no scope reach set yields allow, the delivery is refused
//     (§6.4). The refusal is a NAMED, machine-readable error
//     (DELIVERY_FORBIDDEN + a reason, §6.7), never a silent drop.
//   - Default-DENY is a statement about a GRANT; the absence of an ACL
//     DEPLOYMENT is a different statement and leaves legacy behaviour
//     unchanged (§6.4 rule 2, §8.1). A deployment with no Checker wired is
//     byte-identical to the shipped trust-by-reach bus; a deployment with a
//     Checker wired and an unclassed target row is likewise unchanged,
//     because an unclassed agent is reachable by anyone who can reach the
//     port (the residual risk §8.1 states rather than hides).
//   - Grants are evaluated PER REQUEST and a revoked grant is TOMBSTONED,
//     never edited away (§6.6): the record stays, revoked_at/revoked_by are
//     set, and the effective-permission computation treats it as absent.
//
// The package is self-contained, like internal/session: it does not extend or
// reinterpret internal/registry. The registry handler consumes it through the
// small Authorizer surface and is otherwise unchanged.
package permissions

import (
	"fmt"

	"strings"
	"time"
)

// RecordFormatVersion is the envelope version this package writes and the only
// version it reads. A line whose `v` is unknown is refused loudly rather than
// skipped.
const RecordFormatVersion = 1

// Action is a member of the closed action set (§6.1, §6.2). The vocabulary is
// taken from what the approved options DRAW (§6.2): send/read/invite/admin/mint
// from Option B's matrix and invoke from Option D's legend. An unknown action
// is refused, naming the accepted set.
type Action string

const (
	ActionSend   Action = "send"
	ActionRead   Action = "read"
	ActionInvite Action = "invite"
	ActionAdmin  Action = "admin"
	ActionMint   Action = "mint"
	ActionInvoke Action = "invoke"
)

// Actions is the closed action set in a stable order. It is the list every
// refusal names and every validation loops over.
var Actions = []Action{ActionSend, ActionRead, ActionInvite, ActionAdmin, ActionMint, ActionInvoke}

// ValidAction reports whether a is a member of the closed action set.
func ValidAction(a Action) bool {
	for _, want := range Actions {
		if a == want {
			return true
		}
	}
	return false
}

// ActionSet is the accepted-set string every unknown-action refusal names.
func ActionSet() string {
	parts := make([]string, len(Actions))
	for i, a := range Actions {
		parts[i] = string(a)
	}
	return strings.Join(parts, ", ")
}

// Role is a namespace role (§5.1). The four roles are action BUNDLES: a role
// is not a reach badge (§5.2, T2 closed).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// Roles is the closed role vocabulary in descending privilege order.
var Roles = []Role{RoleOwner, RoleAdmin, RoleMember, RoleViewer}

// ValidRole reports whether r is one of the four roles. The empty role is NOT
// a valid role: it is handled separately (a principal may hold no role).
func ValidRole(r Role) bool {
	for _, want := range Roles {
		if r == want {
			return true
		}
	}
	return false
}

// roleBundles is §5.2's may/may-not matrix as data. Reading the table row by
// row:
//
//	read    owner yes  admin yes  member yes  viewer YES
//	send    owner yes  admin yes  member yes  viewer no
//	invite  owner yes  admin yes  member yes  viewer no
//	admin   owner yes  admin yes  member no   viewer no
//	mint    owner yes  admin yes  member no   viewer no
//	invoke  owner yes  admin yes  member yes  viewer no
//
// `read` never implies `send`: a viewer is a first-class participant with
// exactly one action, and a viewer's send is refused with the same
// DELIVERY_FORBIDDEN a grantless stranger gets (§6.5).
var roleBundles = map[Role]map[Action]bool{
	RoleOwner: {
		ActionSend: true, ActionRead: true, ActionInvite: true,
		ActionAdmin: true, ActionMint: true, ActionInvoke: true,
	},
	RoleAdmin: {
		ActionSend: true, ActionRead: true, ActionInvite: true,
		ActionAdmin: true, ActionMint: true, ActionInvoke: true,
	},
	RoleMember: {
		ActionSend: true, ActionRead: true, ActionInvite: true,
		ActionInvoke: true,
	},
	RoleViewer: {
		ActionRead: true,
	},
}

// RoleAllows reports whether a role's bundle contains an action (§5.2). An
// unset or unknown role allows nothing.
func RoleAllows(r Role, a Action) bool {
	return roleBundles[r][a]
}

// RoleChangesNamespacePolicy reports the §5.1/§5.2 owner-only axis: only
// `owner` may change a namespace's auth posture, rate limit, guard settings or
// retention. It is separate from RoleAllows because it is a property of the
// namespace, not an action on a subject.
func RoleChangesNamespacePolicy(r Role) bool { return r == RoleOwner }

// PrincipalStatus is the principal lifecycle vocabulary (§2.2). `suspended`
// and `revoked` are separate states because "suspended" is reversible and
// "revoked" is a decision about a person that the transcript must be able to
// state years later.
type PrincipalStatus string

const (
	PrincipalInvited   PrincipalStatus = "invited"
	PrincipalActive    PrincipalStatus = "active"
	PrincipalSuspended PrincipalStatus = "suspended"
	PrincipalRevoked   PrincipalStatus = "revoked"
)

// ValidPrincipalStatus reports whether s is a member of the lifecycle
// vocabulary.
func ValidPrincipalStatus(s PrincipalStatus) bool {
	switch s {
	case PrincipalInvited, PrincipalActive, PrincipalSuspended, PrincipalRevoked:
		return true
	}
	return false
}

// Principal is a HUMAN user (§2.1). It is a different kind of thing from an
// agent, and the distinction is the point: a human does not hold ed25519 keys
// to chat and does not appear anonymous (CR-CHAT-007). A principal never
// appears as a bus identity — it is the answer to "which human spoke as which
// agent" (§6.3).
type Principal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// DisplayName is the human-facing name.
	DisplayName string `json:"display_name,omitempty"`
	// Namespace is the realm the principal belongs to. Empty is the default
	// realm, the same `omitempty` discipline the registry row uses.
	Namespace string `json:"namespace,omitempty"`
	// Role is the principal's namespace role (§5.1). Absent means the
	// principal holds no role bundle; it may still hold explicit grants.
	Role Role `json:"role,omitempty"`
	// Status is the lifecycle state (§2.2). A suspended principal's grants
	// are inert but visible; a revoked principal's grants are tombstoned.
	Status PrincipalStatus `json:"status,omitempty"`
	// Remote marks a LOCAL shadow principal for a remote participant (§2.5,
	// D14). It is otherwise an ordinary principal row, and every
	// authorization decision runs against it unchanged.
	Remote         bool       `json:"remote,omitempty"`
	RemoteInstance string     `json:"remote_instance,omitempty"`
	RemoteRef      string     `json:"remote_ref,omitempty"`
	CreatedAt      time.Time  `json:"created_at,omitempty"`
	LastLoginAt    *time.Time `json:"last_login_at,omitempty"`
}

// Active reports whether the principal may currently act. `active` is the only
// state in which grants apply and deliveries may be made (§2.2); an `invited`
// principal cannot log in, and suspended/revoked principals are refused.
func (p *Principal) Active() bool { return p != nil && p.Status == PrincipalActive }

// Binding is a principal's speech right onto an agent (§2.3): "this human may
// speak AS this agent". It does not say "may read its inbox" and it does not
// admit a send — a binding is a speech right exercised through an authorized
// session or delivery, never a bypass of the action check (§6.5).
type Binding struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Principal string    `json:"principal"`
	Agent     string    `json:"agent"`
	AsAgent   bool      `json:"as_agent"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// SubjectType is a member of the closed subject set (§6.1).
type SubjectType string

const (
	SubjectAgent      SubjectType = "agent"
	SubjectGroup      SubjectType = "group"
	SubjectCapability SubjectType = "capability"
	SubjectSession    SubjectType = "session"
	SubjectNamespace  SubjectType = "namespace"
)

// SubjectTypes is the closed subject vocabulary in a stable order.
var SubjectTypes = []SubjectType{SubjectAgent, SubjectGroup, SubjectCapability, SubjectSession, SubjectNamespace}

// ValidSubjectType reports whether t is a member of the closed subject set.
func ValidSubjectType(t SubjectType) bool {
	for _, want := range SubjectTypes {
		if t == want {
			return true
		}
	}
	return false
}

// SubjectSet is the accepted-set string every unknown-subject refusal names.
func SubjectSet() string {
	parts := make([]string, len(SubjectTypes))
	for i, t := range SubjectTypes {
		parts[i] = string(t)
	}
	return strings.Join(parts, ", ")
}

// Subject is the thing a grant binds a principal to (§6.1).
type Subject struct {
	Type SubjectType `json:"type"`
	Ref  string      `json:"ref"`
}

// Validate refuses an unknown subject type or an empty ref.
func (s Subject) Validate() error {
	if !ValidSubjectType(s.Type) {
		return fmt.Errorf("%w: unknown subject type %q (want one of %s)", ErrInvalidRecord, s.Type, SubjectSet())
	}
	if strings.TrimSpace(s.Ref) == "" {
		return fmt.Errorf("%w: subject ref is empty", ErrInvalidRecord)
	}
	return nil
}

// Grant is an ACL entry binding a principal to a subject with an action set
// (§6.1). It is ADDITIVE to a role, and never exceeds what the granting party
// holds ("you cannot grant admin without admin").
type Grant struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Principal is a principal id, NEVER an agent id (§6.1): an agent reaches
	// things by being bound or by being owned, and a second reach path for
	// machines would be a second truth.
	Principal string    `json:"principal"`
	Subject   Subject   `json:"subject"`
	Actions   []Action  `json:"actions"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	// ExpiresAt nil = no expiry; a timestamp = the grant is inert after it and
	// the effective-permission computation treats it as absent. It is NOT
	// deleted — an expired grant is evidence (§6.1).
	ExpiresAt *time.Time `json:"expires_at"`
	// RevokedAt/RevokedBy are the tombstone (§6.6). A revoked grant is never
	// edited away: deleting the record would erase the audit trail of who held
	// what when (T3).
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	RevokedBy string     `json:"revoked_by,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// Validate checks the §6.1 shape: known subject, non-empty known action set,
// a grantor.
func (g *Grant) Validate() error {
	if strings.TrimSpace(g.ID) == "" {
		return fmt.Errorf("%w: grant without id", ErrInvalidRecord)
	}
	if strings.TrimSpace(g.Principal) == "" {
		return fmt.Errorf("%w: grant %q without a principal", ErrInvalidRecord, g.ID)
	}
	if err := g.Subject.Validate(); err != nil {
		return err
	}
	if len(g.Actions) == 0 {
		return fmt.Errorf("%w: grant %q has an empty action set (want a non-empty subset of %s)", ErrInvalidRecord, g.ID, ActionSet())
	}
	for _, a := range g.Actions {
		if !ValidAction(a) {
			return fmt.Errorf("%w: grant %q carries unknown action %q (want one of %s)", ErrInvalidRecord, g.ID, a, ActionSet())
		}
	}
	if strings.TrimSpace(g.GrantedBy) == "" {
		return fmt.Errorf("%w: grant %q without granted_by", ErrInvalidRecord, g.ID)
	}
	return nil
}

// HasAction reports whether the grant's action set contains a.
func (g *Grant) HasAction(a Action) bool {
	for _, have := range g.Actions {
		if have == a {
			return true
		}
	}
	return false
}

// Live reports whether the grant is in force at instant t: not tombstoned and
// not expired (§6.1, §6.6). A revoked or expired grant is treated as absent by
// the effective-permission computation, never as a deny-marker to be surfaced
// as a distinct code.
func (g *Grant) Live(t time.Time) bool {
	if g == nil {
		return false
	}
	if g.RevokedAt != nil {
		return false
	}
	if g.ExpiresAt != nil && !t.Before(*g.ExpiresAt) {
		return false
	}
	return true
}

// AgentClass is the two-value class of an agent row (§3.1). The empty class is
// deliberately NOT a third value: absent means "unchanged shipped behaviour"
// (§8.1), and the ACL has nothing to say about an unclassed row.
type AgentClass string

const (
	ClassPersonal AgentClass = "personal"
	ClassService  AgentClass = "service"
)

// ValidClass reports whether c is one of the two classes. The empty class is
// handled separately (unclassed = legacy posture).
func ValidClass(c AgentClass) bool { return c == ClassPersonal || c == ClassService }

// Reach is the set of principals, groups and capabilities that may reach a
// service agent BECAUSE of a scope (§4.1). It is the only thing that makes a
// service agent reachable (§3.2).
//
// The full Scope object — resource kind/ref, declared_by, the scope.* audit
// events and GET /agents/{id}/scopes — is owned by §4 and CR-CHAT-012 and is
// NOT BUILT here. This package carries only the reach set, because §3.2's rule
// needs it; it adds no scope route and no scope audit event.
type Reach struct {
	Principals   []string `json:"principals,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// ContainsPrincipal reports whether a principal id is in the reach set.
func (r Reach) ContainsPrincipal(id string) bool {
	for _, p := range r.Principals {
		if p == id {
			return true
		}
	}
	return false
}

// Scope is the reach-set half of §4.1's scope object: an agent's declaration
// of who may reach it because of a resource it holds. See Reach for what is
// deliberately out of scope here.
type Scope struct {
	ID    string `json:"id"`
	Reach Reach  `json:"reach"`
}

// AgentInfo is what the ACL needs about a target agent: its class, its owner
// and its scope reach sets (§3.2, §4.1).
//
// The spec fixes `class` and `owner` as properties of the agent ROW (§3.1).
// The shipped registry row has neither (§8.1 item 3), and growing it is the
// agent-class row's change (registration, both stores, migration, OpenAPI),
// not this one. Until then the class data lives with the permission model it
// is read by, as `permission.agent` records, and this struct is the single
// shape the ACL reads. When the registry row grows `class`, the lookup source
// moves and this struct does not change.
type AgentInfo struct {
	ID           string     `json:"id"`
	Class        AgentClass `json:"class,omitempty"`
	Owner        string     `json:"owner,omitempty"`
	Namespace    string     `json:"namespace,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
	Scopes       []Scope    `json:"scopes,omitempty"`
}

// Validate checks the class/owner coherence §3.1 fixes: a `personal` row has
// exactly one owner, a `service` row has none. A class that cannot answer "who
// is accountable" is not a class.
func (a *AgentInfo) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("%w: agent class record without id", ErrInvalidRecord)
	}
	switch a.Class {
	case "":
		// Unclassed: the legacy posture. Owner is not meaningful but is not
		// refused — an operator clearing a class must not have to clear the
		// owner in the same record.
		return nil
	case ClassPersonal:
		if strings.TrimSpace(a.Owner) == "" {
			return fmt.Errorf("%w: personal agent %q has no owner (§3.2)", ErrInvalidRecord, a.ID)
		}
	case ClassService:
		if strings.TrimSpace(a.Owner) != "" {
			return fmt.Errorf("%w: service agent %q carries an owner %q (a service agent has no single human owner)", ErrInvalidRecord, a.ID, a.Owner)
		}
	default:
		return fmt.Errorf("%w: agent %q has unknown class %q (want personal or service)", ErrInvalidRecord, a.ID, a.Class)
	}
	return nil
}

// reachableByPrincipal reports whether a service agent's scope reach sets
// include a principal (§3.2). Groups and capabilities are grant subjects
// (§6.2) and their membership is resolved by CR-CHAT-013; until then a
// principal-directory reach entry is the only resolvable one, which is stated
// here rather than silently assumed.
func (a *AgentInfo) reachableByPrincipal(id string) bool {
	for i := range a.Scopes {
		if a.Scopes[i].Reach.ContainsPrincipal(id) {
			return true
		}
	}
	return false
}

// sameRealm compares two namespace spellings the way namespace.Canonical does
// ("" is the default realm) without importing internal/namespace into a
// package that does not otherwise need it.
func sameRealm(a, b string) bool {
	canon := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return "default"
		}
		return s
	}
	return canon(a) == canon(b)
}
