package namespace

import (
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// lookup is a table-backed env resolver, the same seam the guard exposes.
type lookup map[string]string

func (l lookup) get(k string) (string, bool) {
	v, ok := l[k]
	return v, ok
}

func mustRegistry(t *testing.T, raw string, env lookup) *Registry {
	t.Helper()
	reg, err := Parse(raw, env.get)
	if err != nil {
		t.Fatalf("Parse(%s): %v", raw, err)
	}
	return reg
}

// TestCanonicalIsTheOneMapping pins the property the whole feature rests on:
// the default namespace is spelled "" on every wire and storage surface, and
// every other name spells itself. If this mapping ever moves, single-namespace
// deployments stop being byte-identical to the pre-CR-FEAT-029 server.
func TestCanonicalIsTheOneMapping(t *testing.T) {
	cases := map[string]string{
		"":          "",
		DefaultName: "",
		"acme":      "acme",
		"acme-2":    "acme-2",
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Display(""); got != DefaultName {
		t.Errorf("Display(\"\") = %q, want %q", got, DefaultName)
	}
	if got := Display("acme"); got != "acme" {
		t.Errorf("Display(acme) = %q, want acme", got)
	}
}

// TestNilRegistryIsTheImplicitNamespace: a deployment that declares nothing
// must resolve every name to the default policy without panicking — the nil
// registry is the whole "nothing changes" guarantee, and it is exercised on
// every request of every existing deployment.
func TestNilRegistryIsTheImplicitNamespace(t *testing.T) {
	var reg *Registry

	if p, ok := reg.Lookup(""); !ok || p.Display != DefaultName {
		t.Errorf("nil registry lookup \"\" = (%v, %v), want the default namespace", p, ok)
	}
	if p, ok := reg.Lookup(DefaultName); !ok || p.Display != DefaultName {
		t.Errorf("nil registry lookup %q = (%v, %v), want the default namespace", DefaultName, p, ok)
	}
	if _, ok := reg.Lookup("acme"); ok {
		t.Error("nil registry must not know a name it was never given")
	}
	if got := reg.Resolve("acme"); got.Name != "" || got.Auth != AuthShared {
		t.Errorf("nil registry resolve(acme) = %+v, want the default policy", got)
	}
	if err := reg.CheckAuth("acme", ""); err != nil {
		t.Errorf("nil registry must not require a credential: %v", err)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "" {
		t.Errorf("nil registry names = %v, want [\"\"]", names)
	}
	policies := reg.Policies()
	if len(policies) != 1 || policies[0].Display != DefaultName {
		t.Fatalf("nil registry policies = %+v, want one default", policies)
	}
	// Every inherited value must be the zero value, which is what makes the
	// default namespace mean "whatever the deployment already does".
	if policies[0].RateLimitPerMinute != nil || policies[0].GuardEnabled != nil ||
		policies[0].GuardPolicy != nil || policies[0].RetentionSeconds != nil {
		t.Errorf("default policy must inherit every setting, got %+v", policies[0])
	}
	if policies[0].RateLimit(100) != 100 {
		t.Errorf("default rate limit = %d, want the deployment's 100", policies[0].RateLimit(100))
	}
	if got := policies[0].Retention(24 * time.Hour); got != 24*time.Hour {
		t.Errorf("default retention = %s, want 24h", got)
	}
	if policies[0].GuardSkipped() {
		t.Error("the default namespace must never skip the guard")
	}
}

// TestParseResolvesPolicies covers the document shape and the resolved values.
func TestParseResolvesPolicies(t *testing.T) {
	raw := `{"namespaces":[
		{"name":"default","retention_seconds":3600},
		{"name":"acme","rate_limit_per_minute":5,"guard_enabled":false,"retention_seconds":60,
		 "guard_policy":{"id":"realm-default","thresholds":{"block_risk":"low"}}},
		{"name":"beta","auth":"token","token_ref":"env:BETA_TOKEN"}
	]}`
	reg := mustRegistry(t, raw, lookup{"BETA_TOKEN": "s3cret"})

	if names := reg.Names(); len(names) != 3 || names[0] != "" || names[1] != "acme" || names[2] != "beta" {
		t.Fatalf("names = %v, want [\"\" acme beta]", names)
	}

	def, ok := reg.Lookup(DefaultName)
	if !ok || def.Name != "" {
		t.Fatalf("default lookup = (%+v, %v)", def, ok)
	}
	if def.RetentionSeconds == nil || *def.RetentionSeconds != 3600 {
		t.Errorf("default retention = %v, want 3600", def.RetentionSeconds)
	}

	acme, ok := reg.Lookup("acme")
	if !ok {
		t.Fatal("acme must resolve")
	}
	if acme.RateLimit(100) != 5 {
		t.Errorf("acme rate limit = %d, want 5", acme.RateLimit(100))
	}
	if !acme.GuardSkipped() {
		t.Error("acme declares guard_enabled=false, so the guard must be skipped there")
	}
	if acme.GuardPolicy == nil || acme.GuardPolicy.ID != "realm-default" {
		t.Errorf("acme guard policy = %+v, want the realm default policy", acme.GuardPolicy)
	}
	if got := acme.Retention(time.Hour); got != time.Minute {
		t.Errorf("acme retention = %s, want 1m", got)
	}

	beta, ok := reg.Lookup("beta")
	if !ok {
		t.Fatal("beta must resolve")
	}
	if beta.Auth != AuthToken || beta.TokenRef != "env:BETA_TOKEN" {
		t.Errorf("beta = %+v, want the token posture with its env reference", beta)
	}
	if beta.RateLimit(100) != 100 {
		t.Errorf("beta inherits the deployment's rate limit, got %d", beta.RateLimit(100))
	}
	if err := reg.CheckAuth("beta", "s3cret"); err != nil {
		t.Errorf("the right credential must pass: %v", err)
	}
	if err := reg.CheckAuth("beta", "wrong"); err == nil {
		t.Error("a wrong credential must be refused")
	}
	if err := reg.CheckAuth("beta", ""); err == nil {
		t.Error("a missing credential must be refused")
	}
	// Another namespace's credential must not open beta.
	if err := reg.CheckAuth("acme", "s3cret"); err != nil {
		t.Errorf("a namespace with the shared posture must not ask for a token: %v", err)
	}
}

// TestParseRefusesUnenforceablePolicies: every one of these is a policy that
// would look like protection and enforce nothing, which is the failure mode
// the fail-closed doctrine exists to prevent. All of them must fail the boot.
func TestParseRefusesUnenforceablePolicies(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"token posture with no reference", `{"namespaces":[{"name":"a","auth":"token"}]}`, "token_ref"},
		{"inline secret", `{"namespaces":[{"name":"a","auth":"token","token_ref":"s3cret"}]}`, "env:"},
		{"reference that resolves to nothing", `{"namespaces":[{"name":"a","auth":"token","token_ref":"env:MISSING"}]}`, "unset or empty"},
		{"token with the shared posture", `{"namespaces":[{"name":"a","token_ref":"env:T"}]}`, "cannot be enforced"},
		{"unknown posture", `{"namespaces":[{"name":"a","auth":"open"}]}`, "auth must be"},
		{"negative rate limit", `{"namespaces":[{"name":"a","rate_limit_per_minute":-1}]}`, "rate_limit_per_minute"},
		{"zero retention", `{"namespaces":[{"name":"a","retention_seconds":0}]}`, "retention_seconds"},
		{"uppercase name", `{"namespaces":[{"name":"Acme"}]}`, "invalid name"},
		{"duplicate name", `{"namespaces":[{"name":"a"},{"name":"a"}]}`, "more than once"},
		{"default declared twice", `{"namespaces":[{"name":"default"},{"name":"default"}]}`, "more than once"},
		{"misspelled key", `{"namespaces":[{"name":"a","rate_limits":5}]}`, "unknown field"},
		{"trailing document", `{"namespaces":[]}{"namespaces":[]}`, "trailing content"},
		{"broken guard policy", `{"namespaces":[{"name":"a","guard_policy":{"id":""}}]}`, "guard_policy"},
		{"not json", `{`, "CR_NAMESPACES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.raw, lookup{"T": "x"}.get)
			if err == nil {
				t.Fatalf("Parse accepted an unenforceable policy: %s", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestParseEmptyDocumentIsTheImplicitNamespace: an unset variable and an
// explicitly empty document are the same deployment.
func TestParseEmptyDocumentIsTheImplicitNamespace(t *testing.T) {
	for _, raw := range []string{"", "   \n"} {
		reg, err := Parse(raw, nil)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if names := reg.Names(); len(names) != 1 || names[0] != "" {
			t.Errorf("Parse(%q) names = %v, want the single default namespace", raw, names)
		}
	}
}

// TestRotatedSecretIsPickedUpAtCheckTime: the token is resolved per check, not
// frozen at load, so a rotation in the environment takes effect without a
// rebuild — the same reason the guard resolves api_key_ref late.
func TestRotatedSecretIsPickedUpAtCheckTime(t *testing.T) {
	env := lookup{"T": "first"}
	reg := mustRegistry(t, `{"namespaces":[{"name":"a","auth":"token","token_ref":"env:T"}]}`, env)
	if err := reg.CheckAuth("a", "first"); err != nil {
		t.Fatalf("first secret must pass: %v", err)
	}
	env["T"] = "second"
	if err := reg.CheckAuth("a", "second"); err != nil {
		t.Fatalf("rotated secret must pass without a rebuild: %v", err)
	}
	if err := reg.CheckAuth("a", "first"); err == nil {
		t.Error("the rotated-out secret must stop working")
	}
}

// TestPoliciesNeverRenderASecret: the listing is the operator's view and it is
// also what GET /namespaces serves, so a resolved secret leaking into it would
// be a disclosure on an authenticated-but-ordinary route.
func TestPoliciesNeverRenderASecret(t *testing.T) {
	reg := mustRegistry(t, `{"namespaces":[{"name":"a","auth":"token","token_ref":"env:T"}]}`, lookup{"T": "top-secret"})
	for _, p := range reg.Policies() {
		if strings.Contains(p.TokenRef, "top-secret") {
			t.Fatalf("the resolved secret leaked into the reported policy: %q", p.TokenRef)
		}
	}
}

// TestDeclaredDefaultsDoNotChangeInheritedValues is the single-namespace
// safety net at the policy level: declaring the default namespace and setting
// nothing on it must leave every effective value exactly as the deployment
// already had it.
func TestDeclaredDefaultsDoNotChangeInheritedValues(t *testing.T) {
	bare, err := NewRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	declared := mustRegistry(t, `{"namespaces":[{"name":"default"}]}`, nil)

	gotBare := bare.Resolve("")
	gotDeclared := declared.Resolve("")
	if gotBare.Auth != gotDeclared.Auth {
		t.Errorf("auth: %q vs %q", gotBare.Auth, gotDeclared.Auth)
	}
	if gotDeclared.RateLimit(100) != 100 || gotDeclared.GuardSkipped() {
		t.Errorf("declaring the default namespace changed its effective policy: %+v", gotDeclared)
	}
	if gotDeclared.GuardPolicy != nil {
		t.Errorf("a declared-but-empty default namespace must not gain a guard policy: %+v", gotDeclared.GuardPolicy)
	}
}

// TestGuardPolicyIsValidatedThroughTheGuardPackage: a realm guard policy is
// the guard's own Policy type, parsed by the guard's own validator, so the two
// surfaces cannot disagree about what a valid policy is.
func TestGuardPolicyIsValidatedThroughTheGuardPackage(t *testing.T) {
	reg := mustRegistry(t, `{"namespaces":[{"name":"a","guard_policy":{"id":"strict","thresholds":{"block_risk":"high"},"fail_closed":true}}]}`, nil)
	p, ok := reg.Lookup("a")
	if !ok || p.GuardPolicy == nil {
		t.Fatal("guard policy must resolve")
	}
	if p.GuardPolicy.ID != "strict" || p.GuardPolicy.Thresholds.BlockRisk != guard.RiskHigh || !p.GuardPolicy.FailClosed {
		t.Errorf("guard policy = %+v, want the declared strict policy", p.GuardPolicy)
	}
}
