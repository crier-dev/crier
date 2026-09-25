// Package namespace carries crier's realm dimension: the policy set an agent
// and its messages belong to (CR-FEAT-029).
//
// The external review ("DISPATCH · CRI-001", Carter, 2026-09-25) named the gap
// this package fills, and named the ancestor to steal from: WAMP's realms
// (docs.wamp.org — "Router Realms"), where a router hosts several realms, a
// session is attached to exactly one of them and messages never cross a realm
// boundary. crier had none of that: one registry, one relay topic space, one
// inbox namespace, one rate-limit counter per agent and one guard budget
// shared by every deployment — so one noisy neighbour and everyone shares its
// fate.
//
// What is deliberately NOT copied from WAMP: a realm there is also an
// authentication realm with its own auth methods and a per-realm router
// lifecycle. crier's namespaces are a POLICY dimension on the existing
// single-process relay — auth posture, rate limits, guard settings, retention
// — plus a routing partition, not a second router.
//
// The load-bearing property is the DEFAULT: a deployment that declares no
// namespace at all is byte-for-byte the server it was before this package
// existed. The implicit namespace is named "default" here and is spelled "" on
// every wire and storage surface (see Canonical), so an unconfigured
// deployment's agent rows, inbox entries, relay frames, rate-limit keys and
// delivery bodies are exactly what they were (CR-FEAT-029 acceptance: "single
// namespace behaviour is byte-identical to today").
package namespace

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// DefaultName is the operator-facing name of the implicit namespace every
// deployment had before CR-FEAT-029. It is the only name allowed to spell the
// default namespace, and it may be DECLARED (to carry policy for the default
// realm) but never duplicated.
const DefaultName = "default"

// HeaderNamespace names the namespace a request acts in (relay publish /
// subscribe). Absent means the default namespace.
const HeaderNamespace = "X-Crier-Namespace"

// HeaderNamespaceToken carries the namespace-scoped credential a namespace
// with AuthToken posture requires.
const HeaderNamespaceToken = "X-Crier-Namespace-Token"

// AuthPosture is a namespace's authentication posture. A namespace can only
// ever make the deployment's gate STRICTER or leave it exactly as it was;
// nothing here weakens the shared Bearer token or the per-agent signature
// requirement, both of which still apply unchanged to every request.
type AuthPosture string

const (
	// AuthShared (the default, and the zero value) adds no credential of its
	// own: the deployment's CR_AUTH_TOKEN bearer (and per-agent ed25519
	// signatures where CR_REQUIRE_AGENT_SIG is on) is the whole gate for this
	// namespace's surfaces. An unconfigured deployment is exactly this.
	AuthShared AuthPosture = "shared"
	// AuthToken requires an additional namespace-scoped credential on the
	// surfaces where the namespace is known: registering an agent INTO the
	// namespace, delivering to an agent IN it, and publishing/subscribing in
	// it on the relay. The credential is never stored inline — token_ref is an
	// "env:VAR" reference, the same convention the guard uses for provider
	// keys (specs/LLM-MESSAGE-GUARD.md §4.1) — and it is compared in constant
	// time. A namespace that declares this posture but resolves to an empty
	// secret is refused at load: a gate that silently admits everyone is
	// worse than no gate.
	AuthToken AuthPosture = "token"
)

// EnvRefPrefix is the only accepted token_ref form (guard §4.1 precedent).
const EnvRefPrefix = "env:"

// maxRetentionSeconds bounds a declared retention. It is the largest value the
// delivery path's int-typed ttl_seconds can carry on every platform crier
// builds for (~68 years), which is far past any retention an operator means.
const maxRetentionSeconds = int64(math.MaxInt32)

// Policy is one namespace's resolved policy. Every pointer field is
// "inherited when unset": nil means the deployment-wide setting applies,
// which is what keeps an undeclared namespace identical to the pre-CR-FEAT-029
// server.
type Policy struct {
	// Name is the canonical name: "" for the default namespace, otherwise the
	// declared name.
	Name string `json:"name"`
	// Display is the operator-facing name: "default" for the default
	// namespace, otherwise the same as Name.
	Display string `json:"display"`
	// Auth is the namespace's authentication posture (AuthShared when unset).
	Auth AuthPosture `json:"auth"`
	// TokenRef is the "env:VAR" reference to the namespace's credential. It is
	// a REFERENCE, so it may be reported; the secret it resolves to is never
	// serialized, logged or echoed.
	TokenRef string `json:"token_ref,omitempty"`
	// RateLimitPerMinute caps relay publishes per agent WITHIN this namespace
	// (0 = no per-namespace cap; nil = the deployment's
	// CR_RATE_LIMIT_PER_MINUTE).
	RateLimitPerMinute *int `json:"rate_limit_per_minute,omitempty"`
	// GuardEnabled turns the LLM message guard on or off for deliveries INTO
	// this namespace. nil = the deployment's guard posture (wired or not).
	GuardEnabled *bool `json:"guard_enabled,omitempty"`
	// GuardPolicy is the namespace's default guard policy — the §4.2 step 3
	// server default for deliveries in this namespace, which an agent's own
	// guard.policies still outrank. nil = the deployment's
	// CR_GUARD_DEFAULT_POLICY.
	GuardPolicy *guard.Policy `json:"guard_policy,omitempty"`
	// RetentionSeconds is the default message lifetime for deliveries into
	// this namespace when the request states no ttl_seconds. nil = the store's
	// DefaultMessageTTL.
	RetentionSeconds *int64 `json:"retention_seconds,omitempty"`
}

// nameRE is the accepted namespace name: lowercase letters, digits, underscore
// and hyphen, 1..64 runes, starting with a letter or digit. Deliberately not
// "any string": a name is a wire and storage key, and a namespace spelled like
// the default (""/"default") or with whitespace would be ambiguous everywhere
// it is compared.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// rawPolicy is the JSON shape of one namespace in CR_NAMESPACES. It is decoded
// with DisallowUnknownFields: a misspelled key (rate_limits, guard_policy_json)
// is a startup error, never a silently ignored setting.
type rawPolicy struct {
	Name               string          `json:"name"`
	Auth               string          `json:"auth"`
	TokenRef           string          `json:"token_ref"`
	RateLimitPerMinute *int            `json:"rate_limit_per_minute"`
	GuardEnabled       *bool           `json:"guard_enabled"`
	GuardPolicy        json.RawMessage `json:"guard_policy"`
	RetentionSeconds   *int64          `json:"retention_seconds"`
}

// fileShape is the document CR_NAMESPACES (inline) and CR_NAMESPACES_FILE
// carry.
type fileShape struct {
	Namespaces []rawPolicy `json:"namespaces"`
}

// Canonical maps a namespace name to its canonical (wire and storage) form:
// the default namespace is spelled "" everywhere, and every other name spells
// itself. This is the seam that makes an undeclared deployment byte-identical
// to the pre-CR-FEAT-029 server, and it is the ONLY place the mapping lives.
func Canonical(name string) string {
	if name == "" || name == DefaultName {
		return ""
	}
	return name
}

// Display maps a canonical name back to the operator-facing one.
func Display(canonical string) string {
	if canonical == "" {
		return DefaultName
	}
	return canonical
}

// Registry is an immutable set of namespace policies with the default
// namespace always present. Its methods are nil-safe: a nil *Registry is a
// deployment with no declared namespaces, which resolves every name to the
// default policy and knows no other name — never a panic on the delivery path.
type Registry struct {
	defaultPolicy Policy
	named         map[string]Policy
	order         []string
	// defaultDeclared records whether the default namespace was declared
	// explicitly, so a second declaration is refused rather than silently
	// overwriting the first.
	defaultDeclared bool
	// lookupEnv resolves an env: reference. It is a field so a test can inject
	// a table, exactly as the guard's lookupEnv indirection does.
	lookupEnv func(string) (string, bool)
}

// NewRegistry builds a registry from parsed policies, validating each one and
// resolving its env: references. A nil/empty list yields the registry a
// deployment that declares no namespaces gets: the default namespace, no
// policy, nothing else.
//
// The default namespace's posture is normalised to AuthShared whether or not it
// was declared: an undeclared default realm and a declared-but-empty one must
// report the SAME posture, or GET /namespaces would answer differently for two
// deployments that behave identically.
func NewRegistry(policies []Policy, lookupEnv func(string) (string, bool)) (*Registry, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	r := &Registry{
		named:     map[string]Policy{},
		order:     []string{""},
		lookupEnv: lookupEnv,
	}
	r.defaultPolicy = Policy{Name: "", Display: DefaultName, Auth: AuthShared}
	for _, p := range policies {
		if err := validate(p); err != nil {
			return nil, fmt.Errorf("namespace %q: %w", Display(Canonical(p.Name)), err)
		}
		name := Canonical(p.Name)
		if name == "" {
			if p.Name != "" && p.Name != DefaultName {
				return nil, fmt.Errorf("namespace: invalid default namespace name %q", p.Name)
			}
			if r.defaultDeclared {
				return nil, fmt.Errorf("namespace: %q declared more than once", DefaultName)
			}
			r.defaultDeclared = true
			r.defaultPolicy = p
			r.defaultPolicy.Name = ""
			r.defaultPolicy.Display = DefaultName
			if r.defaultPolicy.Auth == "" {
				r.defaultPolicy.Auth = AuthShared
			}
			continue
		}
		if _, dup := r.named[name]; dup {
			return nil, fmt.Errorf("namespace: %q declared more than once", name)
		}
		r.named[name] = p
		r.order = append(r.order, name)
	}
	if err := validateAuth(r.defaultPolicy, r.lookupEnv); err != nil {
		return nil, err
	}
	for name, p := range r.named {
		if err := validateAuth(p, r.lookupEnv); err != nil {
			return nil, fmt.Errorf("namespace %q: %w", name, err)
		}
	}
	return r, nil
}

// Parse reads the namespace document CR_NAMESPACES carries and returns the
// registry it describes. An empty document is NOT an error here (the caller
// decides whether an unset variable means "no namespaces"); a malformed one
// always is — a namespace policy that cannot be read must fail the boot, never
// silently disappear.
func Parse(raw string, lookupEnv func(string) (string, bool)) (*Registry, error) {
	if strings.TrimSpace(raw) == "" {
		return NewRegistry(nil, lookupEnv)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc fileShape
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("namespace: CR_NAMESPACES: %w", err)
	}
	// A trailing second document is a typo, not a policy.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("namespace: CR_NAMESPACES: trailing content after the namespace document")
	}
	policies, err := resolveRaw(doc.Namespaces)
	if err != nil {
		return nil, err
	}
	return NewRegistry(policies, lookupEnv)
}

// ParseFile reads the namespace document from a file (CR_NAMESPACES_FILE).
func ParseFile(path string, lookupEnv func(string) (string, bool)) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("namespace: read %s: %w", path, err)
	}
	return Parse(string(raw), lookupEnv)
}

// resolveRaw validates and resolves the declared namespace documents.
func resolveRaw(raws []rawPolicy) ([]Policy, error) {
	out := make([]Policy, 0, len(raws))
	seen := map[string]bool{}
	for _, rp := range raws {
		name := strings.TrimSpace(rp.Name)
		canonical := Canonical(name)
		if name != "" && canonical != "" && !nameRE.MatchString(name) {
			return nil, fmt.Errorf("namespace: invalid name %q (want %s)", name, nameRE)
		}
		if seen[canonical] {
			if canonical == "" {
				return nil, fmt.Errorf("namespace: %q declared more than once", DefaultName)
			}
			return nil, fmt.Errorf("namespace: %q declared more than once", name)
		}
		seen[canonical] = true

		p := Policy{
			Name:               canonical,
			Display:            Display(canonical),
			TokenRef:           strings.TrimSpace(rp.TokenRef),
			RateLimitPerMinute: rp.RateLimitPerMinute,
			GuardEnabled:       rp.GuardEnabled,
			RetentionSeconds:   rp.RetentionSeconds,
		}
		switch posture := AuthPosture(strings.TrimSpace(rp.Auth)); posture {
		case "":
			p.Auth = AuthShared
		case AuthShared, AuthToken:
			p.Auth = posture
		default:
			return nil, fmt.Errorf("namespace %q: auth must be %s|%s (got %q)",
				Display(canonical), AuthShared, AuthToken, rp.Auth)
		}
		if len(rp.GuardPolicy) > 0 && string(rp.GuardPolicy) != "null" {
			gp, err := guard.ParsePolicy(string(rp.GuardPolicy))
			if err != nil {
				return nil, fmt.Errorf("namespace %q: guard_policy: %w", Display(canonical), err)
			}
			p.GuardPolicy = &gp
		}
		if err := validate(p); err != nil {
			return nil, fmt.Errorf("namespace %q: %w", Display(canonical), err)
		}
		out = append(out, p)
	}
	return out, nil
}

// validate applies every policy bound. The direction is fail-closed-but-loud:
// a value that cannot mean what it says is refused at load, because the
// alternative — a namespace policy that quietly does nothing — is the precise
// failure mode this feature exists to remove.
func validate(p Policy) error {
	if local := p.Name; local != "" && local != DefaultName && !nameRE.MatchString(local) {
		return fmt.Errorf("invalid name %q (want %s)", local, nameRE)
	}
	if p.RateLimitPerMinute != nil && *p.RateLimitPerMinute < 0 {
		return fmt.Errorf("rate_limit_per_minute must be >= 0 (0 = no per-namespace cap)")
	}
	if p.RetentionSeconds != nil {
		if *p.RetentionSeconds < 1 {
			return fmt.Errorf("retention_seconds must be >= 1 (a namespace default of 0 would mean \"never expires\" for every message in it)")
		}
		// Bounded so the value is portable into the int-typed ttl_seconds the
		// delivery path carries: MaxInt32 seconds is ~68 years, far past any
		// retention an operator can mean, and refusing it here keeps the
		// conversion from silently truncating on a 32-bit build.
		if *p.RetentionSeconds > maxRetentionSeconds {
			return fmt.Errorf("retention_seconds must be <= %d", maxRetentionSeconds)
		}
	}
	if p.TokenRef != "" && !strings.HasPrefix(p.TokenRef, EnvRefPrefix) {
		return fmt.Errorf("token_ref must be an %sVAR reference (never an inline secret)", EnvRefPrefix)
	}
	if p.Auth == AuthShared && p.TokenRef != "" {
		return fmt.Errorf("token_ref is set but auth is %s — a token no posture reads is a policy that cannot be enforced", AuthShared)
	}
	if p.Auth != AuthShared && p.Auth != AuthToken {
		return fmt.Errorf("auth must be %s|%s (got %q)", AuthShared, AuthToken, p.Auth)
	}
	return nil
}

// validateAuth refuses AuthToken posture unless its reference resolves to a
// non-empty secret NOW. A namespace that advertises a gate it cannot enforce
// would admit every caller, which is a silent widening of the deployment's
// auth — exactly the failure the fail-closed doctrine rejects.
func validateAuth(p Policy, lookupEnv func(string) (string, bool)) error {
	if p.Auth != AuthToken {
		return nil
	}
	if p.TokenRef == "" {
		return fmt.Errorf("auth %s requires token_ref", AuthToken)
	}
	if _, err := p.namespaceToken(lookupEnv); err != nil {
		return err
	}
	return nil
}

// namespaceToken resolves the namespace's credential at CHECK time (not at
// load), so a rotation in the environment is picked up without a rebuild —
// the same reason the guard resolves api_key_ref late.
func (p Policy) namespaceToken(lookupEnv func(string) (string, bool)) (string, error) {
	if p.TokenRef == "" {
		return "", fmt.Errorf("namespace %q: auth %s requires token_ref", p.Display, AuthToken)
	}
	v, ok := lookupEnv(strings.TrimPrefix(p.TokenRef, EnvRefPrefix))
	if !ok || v == "" {
		return "", fmt.Errorf("namespace %q: %s is unset or empty", p.Display, p.TokenRef)
	}
	return v, nil
}

// CheckAuth reports whether provided satisfies this namespace's auth posture.
// It is the ONE place the posture is enforced, shared by the registry
// (registration and delivery) and the relay (publish and subscribe), so no two
// lanes can disagree about what a namespace requires.
//
// A nil-safe empty policy (the default namespace with no declaration) has
// AuthShared and always passes, which is the pre-CR-FEAT-029 behaviour.
func (r *Registry) CheckAuth(name, provided string) error {
	p := r.Resolve(name)
	if p.Auth != AuthToken {
		return nil
	}
	want, err := p.namespaceToken(r.lookup)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(want)) != 1 {
		return fmt.Errorf("%w: namespace %q requires %s", ErrUnauthorized, p.Display, HeaderNamespaceToken)
	}
	return nil
}

// ErrUnauthorized is the sentinel for a namespace credential that is absent or
// wrong. Callers answer 401/403 — never a silent fallback to the default
// namespace, which would be the implicit crossing this feature forbids.
var ErrUnauthorized = fmt.Errorf("namespace authorization failed")

// ErrUnknownNamespace is the sentinel for a name no policy declares. It is
// distinct from ErrUnauthorized on purpose: a typo must be told apart from a
// refusal, and neither may resolve to the default namespace by accident.
var ErrUnknownNamespace = fmt.Errorf("unknown namespace")

// Lookup resolves a declared name. A nil registry — or ""/"default" — always
// yields the default policy; every other name is reported absent, so a caller
// can answer ErrUnknownNamespace instead of quietly using the default.
func (r *Registry) Lookup(name string) (Policy, bool) {
	canonical := Canonical(name)
	if r == nil {
		if canonical == "" {
			return Policy{Name: "", Display: DefaultName, Auth: AuthShared}, true
		}
		return Policy{}, false
	}
	if canonical == "" {
		return r.defaultPolicy, true
	}
	p, ok := r.named[canonical]
	return p, ok
}

// Resolve returns the policy for a name, falling back to the default policy.
// Use it where the name came from a validated source (an agent's stored row);
// use Lookup where the name came from a request.
func (r *Registry) Resolve(name string) Policy {
	p, ok := r.Lookup(name)
	if ok {
		return p
	}
	if r == nil {
		return Policy{Display: DefaultName, Auth: AuthShared}
	}
	return r.defaultPolicy
}

// Names lists the canonical namespace names, the default first.
func (r *Registry) Names() []string {
	if r == nil {
		return []string{""}
	}
	out := make([]string, 0, len(r.order))
	out = append(out, r.order...)
	return out
}

// Policies lists every policy, the default first, for the operator-facing
// listing. It reports references and postures, never a resolved secret.
func (r *Registry) Policies() []Policy {
	if r == nil {
		return []Policy{{Name: "", Display: DefaultName, Auth: AuthShared}}
	}
	out := make([]Policy, 0, len(r.order))
	for _, name := range r.order {
		if name == "" {
			out = append(out, r.defaultPolicy)
			continue
		}
		out = append(out, r.named[name])
	}
	return out
}

// lookup resolves an env reference through the registry's injectable lookup.
func (r *Registry) lookup(name string) (string, bool) {
	if r == nil || r.lookupEnv == nil {
		return os.LookupEnv(name)
	}
	return r.lookupEnv(name)
}

// RateLimit is the publish cap that applies inside a namespace: the
// namespace's own value when declared, else the deployment's
// CR_RATE_LIMIT_PER_MINUTE (the pre-CR-FEAT-029 behaviour, unchanged).
func (p Policy) RateLimit(deploymentDefault int) int {
	if p.RateLimitPerMinute != nil {
		return *p.RateLimitPerMinute
	}
	return deploymentDefault
}

// Retention is the default message lifetime for deliveries into a namespace:
// the namespace's own value when declared, else the store's default.
func (p Policy) Retention(deploymentDefault time.Duration) time.Duration {
	if p.RetentionSeconds == nil {
		return deploymentDefault
	}
	return time.Duration(*p.RetentionSeconds) * time.Second
}

// GuardSkipped reports whether deliveries into this namespace bypass the LLM
// message guard entirely. Default false — an undeclared namespace never
// changes the delivery path.
func (p Policy) GuardSkipped() bool {
	return p.GuardEnabled != nil && !*p.GuardEnabled
}

// LogPolicies writes one line naming the namespaces this process serves. It
// reports POSTURE and REFERENCES only: a namespace's secret is never logged.
func (r *Registry) LogPolicies() {
	if r == nil || len(r.order) <= 1 {
		slog.Info("namespaces: none declared — the implicit default namespace serves every agent")
		return
	}
	parts := make([]string, 0, len(r.order))
	for _, name := range r.order {
		p := r.Resolve(name)
		parts = append(parts, fmt.Sprintf("%s(auth=%s,rate=%s,retention=%s)",
			p.Display, p.Auth, rateString(p), retentionString(p)))
	}
	slog.Info("namespaces declared", "namespaces", strings.Join(parts, " "))
}

func rateString(p Policy) string {
	if p.RateLimitPerMinute == nil {
		return "deployment"
	}
	return fmt.Sprintf("%d/min", *p.RateLimitPerMinute)
}

func retentionString(p Policy) string {
	if p.RetentionSeconds == nil {
		return "deployment"
	}
	return (time.Duration(*p.RetentionSeconds) * time.Second).String()
}
