package addressing

// CR-CHAT-004 acceptance — the addressing grammar + parser of
// specs/CHAT-ADDRESSING.md §1.
//
// The tests are the acceptance criteria, named to match them:
//
//	A2  table-driven valid/invalid parsing, every failure a NAMED error
//	A3  an address list parses and re-renders canonically (round trip)
//	A4  `@ns/*` never parses as a remote_ref (§2.1.1 precedence)
//
// The parser is deliberately mechanical (D12, §2.5): it returns address
// tokens + kinds and nothing else. Nothing here asserts a delivery, because
// nothing here may deliver.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// A2 — every valid form parses to the right kind + parts
// ---------------------------------------------------------------------------

func TestParse_SingleForms(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantKind     Kind
		wantRef      string
		wantInstance string
		wantMember   string
		wantCanon    string
	}{
		{"agent_bare", "@atlas", KindAgent, "atlas", "", "", "@atlas"},
		{"agent_upper", "@Kappa", KindAgent, "Kappa", "", "", "@Kappa"},
		{"agent_digit_leading", "@9lives", KindAgent, "9lives", "", "", "@9lives"},
		{"agent_underscore_leading", "@_x", KindAgent, "_x", "", "", "@_x"},
		{"agent_hyphen_dot", "@kappa-2.1", KindAgent, "kappa-2.1", "", "", "@kappa-2.1"},
		{"team_bare", "@team:infra", KindTeam, "infra", "", "", "@team:infra"},
		{"team_hyphen", "@team:dev-agents", KindTeam, "dev-agents", "", "", "@team:dev-agents"},
		{"cap_simple", "@cap:solver", KindCapability, "solver", "", "", "@cap:solver"},
		{"cap_hyphen", "@cap:data-capabilities", KindCapability, "data-capabilities", "", "", "@cap:data-capabilities"},
		{"ns_top_wildcard", "@ns/*", KindNamespace, "", "", "*", "@ns/*"},
		{"ns_named_wildcard", "@ns/acme/*", KindNamespace, "acme", "", "*", "@ns/acme/*"},
		{"ns_named_agent", "@ns/acme/atlas", KindNamespace, "acme", "", "atlas", "@ns/acme/atlas"},
		{"ns_named_only", "@ns/acme", KindNamespace, "acme", "", "", "@ns/acme"},
		{"ns_hyphen_name", "@ns/realm-1/a.b", KindNamespace, "realm-1", "", "a.b", "@ns/realm-1/a.b"},
		{"remote", "@peer/atlas", KindRemote, "atlas", "peer", "", "@peer/atlas"},
		{"remote_hyphen", "@inst-1/agent_2", KindRemote, "agent_2", "inst-1", "", "@inst-1/agent_2"},
		{"session", "#build-plan", KindSession, "build-plan", "", "", "#build-plan"},
		{"session_upper", "#A7F3", KindSession, "A7F3", "", "", "#A7F3"},

		// §2.1.1 — a reserved prefix is a prefix, not a whole token: the bare
		// spellings are ordinary agent ids.
		{"team_without_colon_is_agent", "@team", KindAgent, "team", "", "", "@team"},
		{"cap_without_colon_is_agent", "@cap", KindAgent, "cap", "", "", "@cap"},
		{"ns_without_slash_is_agent", "@ns", KindAgent, "ns", "", "", "@ns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want nil", tc.in, err)
			}
			if a.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", a.Kind, tc.wantKind)
			}
			if a.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", a.Ref, tc.wantRef)
			}
			if a.Instance != tc.wantInstance {
				t.Errorf("Instance = %q, want %q", a.Instance, tc.wantInstance)
			}
			if a.Member != tc.wantMember {
				t.Errorf("Member = %q, want %q", a.Member, tc.wantMember)
			}
			if got := a.String(); got != tc.wantCanon {
				t.Errorf("String() = %q, want %q", got, tc.wantCanon)
			}
			// Whitespace around a single address is insignificant.
			if _, err := Parse("  " + tc.in + "\t"); err != nil {
				t.Errorf("Parse(padded %q) error = %v, want nil", tc.in, err)
			}
		})
	}
}

// A 256-character address (maxAddressLength, sigil included) is legal; one
// character more is a named refusal, and the echoed token is CLIPPED so a
// refusal body stays bounded.
func TestParse_AddressTooLong(t *testing.T) {
	atBound := "@" + strings.Repeat("a", MaxAddressLength-1)
	if len(atBound) != MaxAddressLength {
		t.Fatalf("fixture length = %d, want %d", len(atBound), MaxAddressLength)
	}
	if _, err := Parse(atBound); err != nil {
		t.Fatalf("Parse(%d-char address) error = %v, want nil", MaxAddressLength, err)
	}

	over := atBound + "a"
	_, err := Parse(over)
	if err == nil {
		t.Fatalf("Parse(%d-char address) = nil error, want %s", len(over), RuleAddressTooLong)
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("error %T = %v, want *addressing.Error", err, err)
	}
	if aerr.Rule != RuleAddressTooLong {
		t.Errorf("Rule = %q, want %q", aerr.Rule, RuleAddressTooLong)
	}
	if clipped := strings.TrimSuffix(aerr.Token, "…"); !strings.HasPrefix(over, clipped) {
		t.Errorf("Token = %q, want a truncation of the offending token", aerr.Token)
	} else if utf8.RuneCountInString(clipped) > MaxAddressLength {
		t.Errorf("Token carries %d runes, want it bounded to %d", utf8.RuneCountInString(clipped), MaxAddressLength)
	}
	for _, want := range []string{"256", "257"} {
		if !strings.Contains(aerr.Detail, want) {
			t.Errorf("Detail = %q, want it to name %q", aerr.Detail, want)
		}
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantRule string
	}{
		{"empty", "", RuleEmptyToken},
		{"at_only", "@", RuleEmptyToken},
		{"hash_only", "#", RuleEmptyToken},
		{"no_sigil", "atlas", RuleNotAnAddress},
		{"no_sigil_dotted", "team:infra", RuleNotAnAddress},

		{"team_empty_name", "@team:", RuleEmptyGroupName},
		{"cap_empty_name", "@cap:", RuleEmptyCapabilityName},
		{"ns_empty", "@ns/", RuleEmptyNamespace},

		{"ns_empty_target", "@ns/acme/", RuleEmptyNamespaceTarget},
		{"ns_upper_name", "@ns/ACME/*", RuleInvalidNamespaceName},
		{"ns_punct_name", "@ns/acme.thing/*", RuleInvalidNamespaceName},
		{"ns_wildcard_name", "@ns/*/x", RuleInvalidNamespaceName},
		{"ns_bad_target", "@ns/acme/*/x", RuleBadTokenCharset},
		{"ns_too_long_name", "@ns/" + strings.Repeat("a", 65) + "/*", RuleInvalidNamespaceName},

		{"remote_empty_agent", "@peer/", RuleEmptyRemoteAgent},
		{"remote_empty_instance", "@/atlas", RuleEmptyRemoteInstance},
		{"remote_double_slash", "@peer//x", RuleBadTokenCharset},

		{"agent_bad_charset_bang", "@ag!ent", RuleBadTokenCharset},
		{"agent_bad_charset_space", "@ag ent", RuleBadTokenCharset},
		{"agent_bad_charset_colon", "@teams:x", RuleBadTokenCharset},
		{"agent_bad_leading_hyphen", "@-agent", RuleBadTokenCharset},
		{"agent_trailing_junk", "@atlas@@", RuleBadTokenCharset},
		{"team_bad_name", "@team:a!b", RuleBadTokenCharset},
		{"team_name_with_slash", "@team:a/b", RuleBadTokenCharset},
		{"session_bad_charset", "#a/b", RuleBadTokenCharset},
		{"session_trailing_junk", "#build-plan!", RuleBadTokenCharset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.in)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a NAMED refusal", tc.in)
			}
			assertNamedError(t, err, tc.wantRule, tc.in)
		})
	}
}

// assertNamedError is the shape every parse failure must have: the typed
// *Error, the INVALID_ADDRESS code, the offending token verbatim, and a rule
// name from the closed vocabulary — never a bare string.
func assertNamedError(t *testing.T, err error, wantRule, wantToken string) {
	t.Helper()
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("error %T = %v, want *addressing.Error (a bare string is not an address refusal)", err, err)
	}
	if aerr.Code != ErrorInvalidAddress {
		t.Errorf("Code = %q, want %q", aerr.Code, ErrorInvalidAddress)
	}
	if aerr.Rule != wantRule {
		t.Errorf("Rule = %q, want %q", aerr.Rule, wantRule)
	}
	if !ValidRule(aerr.Rule) {
		t.Errorf("Rule %q is not a member of the closed vocabulary %v", aerr.Rule, Rules)
	}
	if wantToken != "" && aerr.Token != wantToken {
		t.Errorf("Token = %q, want the offending token %q", aerr.Token, wantToken)
	}
	if aerr.Detail == "" {
		t.Error("Detail is empty; a named refusal must explain the rule")
	}
	// The message must name BOTH the rule and the offending token.
	msg := aerr.Error()
	if !strings.Contains(msg, wantRule) {
		t.Errorf("Error() = %q, want it to name the rule %q", msg, wantRule)
	}
	if wantToken != "" && !strings.Contains(msg, wantToken) {
		t.Errorf("Error() = %q, want it to name the offending token %q", msg, wantToken)
	}
}

// ---------------------------------------------------------------------------
// A3 — an address list parses and re-renders canonically
// ---------------------------------------------------------------------------

func TestParseList_ValidAndRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantKinds []Kind
		wantCanon string
	}{
		{
			name:      "comma",
			in:        "@atlas, @team:infra, @cap:solver",
			wantKinds: []Kind{KindAgent, KindTeam, KindCapability},
			wantCanon: "@atlas, @team:infra, @cap:solver",
		},
		{
			name:      "semicolon",
			in:        "@atlas;@cap:solver",
			wantKinds: []Kind{KindAgent, KindCapability},
			wantCanon: "@atlas, @cap:solver",
		},
		{
			name:      "whitespace",
			in:        "@atlas\t@cap:solver\n#build-plan",
			wantKinds: []Kind{KindAgent, KindCapability, KindSession},
			wantCanon: "@atlas, @cap:solver, #build-plan",
		},
		{
			name:      "mixed_separators_and_runs",
			in:        "  @atlas ,  ;  @ns/acme/*   #A7F3 ",
			wantKinds: []Kind{KindAgent, KindNamespace, KindSession},
			wantCanon: "@atlas, @ns/acme/*, #A7F3",
		},
		{
			name:      "remote_in_a_list",
			in:        "@peer/atlas @ns/acme/atlas",
			wantKinds: []Kind{KindRemote, KindNamespace},
			wantCanon: "@peer/atlas, @ns/acme/atlas",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list, err := ParseList(tc.in)
			if err != nil {
				t.Fatalf("ParseList(%q) error = %v, want nil", tc.in, err)
			}
			if len(list) != len(tc.wantKinds) {
				t.Fatalf("len(list) = %d, want %d", len(list), len(tc.wantKinds))
			}
			for i, k := range tc.wantKinds {
				if list[i].Kind != k {
					t.Errorf("list[%d].Kind = %q, want %q", i, list[i].Kind, k)
				}
			}
			if got := list.String(); got != tc.wantCanon {
				t.Errorf("list.String() = %q, want canonical %q", got, tc.wantCanon)
			}

			// A3 round trip: re-parsing the canonical rendering yields the
			// SAME list, and re-rendering is a fixed point.
			again, err := ParseList(list.String())
			if err != nil {
				t.Fatalf("ParseList(canonical %q) error = %v, want nil", list.String(), err)
			}
			if !list.Equal(again) {
				t.Errorf("round trip changed the list:\n first = %v\nsecond = %v", list, again)
			}
			if got := again.String(); got != tc.wantCanon {
				t.Errorf("second render = %q, want the fixed point %q", got, tc.wantCanon)
			}
		})
	}
}

func TestParseList_Invalid(t *testing.T) {
	// 65 addresses: one past the bound.
	tooMany := strings.TrimSuffix(strings.Repeat("@a,", MaxAddressesPerWrite+1), ",")

	tests := []struct {
		name     string
		in       string
		wantRule string
	}{
		{"empty", "", RuleEmptyAddressList},
		{"whitespace_only", "   \t\n", RuleEmptyAddressList},
		{"leading_comma", ",@atlas", RuleLeadingSeparator},
		{"leading_semicolon", "; @atlas", RuleLeadingSeparator},
		{"trailing_comma", "@atlas,", RuleTrailingSeparator},
		{"trailing_semicolon", "@atlas @cap:x ;", RuleTrailingSeparator},
		{"too_many", tooMany, RuleTooManyAddresses},
		{"one_bad_member_fails_the_list", "@atlas, @team:", RuleEmptyGroupName},
		{"no_sigil_member", "@atlas, atlas", RuleNotAnAddress},
		{"trailing_comma_run", "@atlas,,,", RuleTrailingSeparator},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseList(tc.in)
			if err == nil {
				t.Fatalf("ParseList(%q) = nil error, want a NAMED refusal", tc.in)
			}
			assertNamedError(t, err, tc.wantRule, "")
		})
	}
}

// The bound names BOTH the limit and the count received (§1.1) — a caller
// must be able to see that the list was refused, not truncated.
func TestParseList_TooManyNamesLimitAndCount(t *testing.T) {
	tooMany := strings.TrimSuffix(strings.Repeat("@a,", MaxAddressesPerWrite+1), ",")
	_, err := ParseList(tooMany)
	if err == nil {
		t.Fatalf("ParseList(%d addresses) = nil error, want %s", MaxAddressesPerWrite+1, RuleTooManyAddresses)
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("error %T = %v, want *addressing.Error", err, err)
	}
	if aerr.Rule != RuleTooManyAddresses {
		t.Fatalf("Rule = %q, want %q", aerr.Rule, RuleTooManyAddresses)
	}
	for _, want := range []string{"64", "65"} {
		if !strings.Contains(aerr.Detail, want) {
			t.Errorf("Detail = %q, want it to name %q", aerr.Detail, want)
		}
	}
}

// Exactly at the bound is legal: the limit bounds the fan-out, it does not
// round down.
func TestParseList_AtTheBoundIsLegal(t *testing.T) {
	exact := strings.TrimSuffix(strings.Repeat("@a,", MaxAddressesPerWrite), ",")
	list, err := ParseList(exact)
	if err != nil {
		t.Fatalf("ParseList(%d addresses) error = %v, want nil", MaxAddressesPerWrite, err)
	}
	if len(list) != MaxAddressesPerWrite {
		t.Fatalf("len(list) = %d, want %d", len(list), MaxAddressesPerWrite)
	}
}

// ---------------------------------------------------------------------------
// A4 — precedence: `@ns/` is reserved and is NEVER a remote_ref (§2.1.1)
// ---------------------------------------------------------------------------

func TestParse_NamespacePrefixBeatsRemote(t *testing.T) {
	for _, in := range []string{"@ns/*", "@ns/acme/*", "@ns/acme/atlas", "@ns/peer/agent"} {
		t.Run(in, func(t *testing.T) {
			a, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want nil", in, err)
			}
			if a.Kind != KindNamespace {
				t.Errorf("Kind = %q, want %q (the reserved prefix wins)", a.Kind, KindNamespace)
			}
			if a.Kind == KindRemote {
				t.Fatal("@ns/ parsed as a remote_ref")
			}
			if a.Instance != "" {
				t.Errorf("Instance = %q, want empty (a namespace address has no peer)", a.Instance)
			}
		})
	}
	// The peer named `ns` is therefore unaddressable: `@ns/x` is the namespace
	// `x`, never the agent `x` on a peer called `ns`.
	a, err := Parse("@ns/atlas")
	if err != nil {
		t.Fatalf("Parse(@ns/atlas) error = %v, want nil", err)
	}
	if a.Kind != KindNamespace || a.Ref != "atlas" {
		t.Fatalf("Parse(@ns/atlas) = %+v, want a namespace address named \"atlas\"", a)
	}
}

// ---------------------------------------------------------------------------
// §1.3 — an addressed write: at most one `#` room, plus the recipients
// ---------------------------------------------------------------------------

func TestParseWrite(t *testing.T) {
	t.Run("session_plus_recipients", func(t *testing.T) {
		w, err := ParseWrite("#build-plan @atlas @cap:research")
		if err != nil {
			t.Fatalf("ParseWrite error = %v", err)
		}
		if w.Session == nil || w.Session.Kind != KindSession || w.Session.Ref != "build-plan" {
			t.Fatalf("Session = %+v, want #build-plan", w.Session)
		}
		if len(w.Addresses) != 2 || w.Addresses[0].Ref != "atlas" || w.Addresses[1].Ref != "research" {
			t.Fatalf("Addresses = %v, want [@atlas @cap:research]", w.Addresses)
		}
		if got, want := w.String(), "#build-plan @atlas @cap:research"; got != want {
			t.Errorf("w.String() = %q, want %q", got, want)
		}
		again, err := ParseWrite(w.String())
		if err != nil {
			t.Fatalf("ParseWrite(canonical) error = %v", err)
		}
		if !sameWrite(w, again) {
			t.Errorf("write round trip changed: %v -> %v", w, again)
		}
	})

	t.Run("recipients_only", func(t *testing.T) {
		w, err := ParseWrite("@atlas @cap:x")
		if err != nil {
			t.Fatalf("ParseWrite error = %v", err)
		}
		if w.Session != nil {
			t.Errorf("Session = %+v, want nil (no room named)", w.Session)
		}
		if len(w.Addresses) != 2 {
			t.Errorf("len(Addresses) = %d, want 2", len(w.Addresses))
		}
	})

	t.Run("session_only", func(t *testing.T) {
		w, err := ParseWrite("#build-plan")
		if err != nil {
			t.Fatalf("ParseWrite error = %v", err)
		}
		if w.Session == nil || len(w.Addresses) != 0 {
			t.Errorf("ParseWrite(#build-plan) = %+v, want a session and no recipients", w)
		}
	})

	t.Run("two_sessions_refused", func(t *testing.T) {
		_, err := ParseWrite("#a #b")
		if err == nil {
			t.Fatal("ParseWrite(#a #b) = nil error, want a refusal (§1.3: one write has at most one room)")
		}
		assertNamedError(t, err, RuleMultipleSessions, "")
	})

	t.Run("empty_refused", func(t *testing.T) {
		if _, err := ParseWrite("   "); err == nil {
			t.Fatal("ParseWrite(blank) = nil error, want EMPTY_ADDRESS_LIST")
		}
	})

	t.Run("malformed_member_refused", func(t *testing.T) {
		_, err := ParseWrite("#room @ghost!")
		if err == nil {
			t.Fatal("ParseWrite with a malformed member = nil error, want a refusal")
		}
		assertNamedError(t, err, RuleBadTokenCharset, "@ghost!")
	})
}

func sameWrite(a, b Write) bool {
	if (a.Session == nil) != (b.Session == nil) {
		return false
	}
	if a.Session != nil && *a.Session != *b.Session {
		return false
	}
	return a.Addresses.Equal(b.Addresses)
}

// ---------------------------------------------------------------------------
// The refusal wire shape (§4) — what the 400 body carries
// ---------------------------------------------------------------------------

func TestError_RefusalWireShape(t *testing.T) {
	_, err := Parse("@team:")
	if err == nil {
		t.Fatal("Parse(@team:) = nil error, want a refusal")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("error %T = %v, want *addressing.Error", err, err)
	}
	raw, mErr := json.Marshal(aerr.Refusal())
	if mErr != nil {
		t.Fatalf("marshal refusal: %v", mErr)
	}
	var got map[string]string
	if mErr := json.Unmarshal(raw, &got); mErr != nil {
		t.Fatalf("unmarshal refusal: %v", mErr)
	}
	if got["error"] != "INVALID_ADDRESS" {
		t.Errorf("error = %q, want INVALID_ADDRESS", got["error"])
	}
	if got["address"] != "@team:" {
		t.Errorf("address = %q, want the offending token @team:", got["address"])
	}
	if got["reason"] != RuleEmptyGroupName {
		t.Errorf("reason = %q, want %q", got["reason"], RuleEmptyGroupName)
	}
	if got["detail"] == "" {
		t.Error("detail is empty; the §4 body carries one")
	}
}

// ---------------------------------------------------------------------------
// The vocabulary is closed and inspectable
// ---------------------------------------------------------------------------

func TestRules_ClosedVocabulary(t *testing.T) {
	if len(Rules) == 0 {
		t.Fatal("Rules is empty")
	}
	seen := map[string]bool{}
	for _, r := range Rules {
		if r == "" {
			t.Fatalf("Rules contains an empty rule name")
		}
		if seen[r] {
			t.Errorf("rule %q is declared twice", r)
		}
		seen[r] = true
		if !ValidRule(r) {
			t.Errorf("ValidRule(%q) = false for a member of Rules", r)
		}
	}
	if ValidRule("NOT_A_RULE") {
		t.Error("ValidRule accepted an unknown rule name")
	}
	if ValidRule("") {
		t.Error("ValidRule accepted the empty rule name")
	}
}

// ---------------------------------------------------------------------------
// D12 — the parser is mechanical: it never resolves, fans out or executes
// ---------------------------------------------------------------------------

// The package exposes no resolution/delivery surface at all. This test pins
// the vocabulary of the API rather than a behaviour: if a future change adds a
// "deliver" or "resolve" verb to this package, it fails here, and the reviewer
// has to justify the widening against §2.5.
func TestPackageSurface_HasNoDeliveryVerb(t *testing.T) {
	// Kinds are the only classification this package produces, and every
	// kind is a TARGET KIND, never an action.
	want := []Kind{KindAgent, KindTeam, KindCapability, KindNamespace, KindRemote, KindSession}
	if len(Kinds) != len(want) {
		t.Fatalf("Kinds = %v, want exactly %v", Kinds, want)
	}
	for i, k := range want {
		if Kinds[i] != k {
			t.Errorf("Kinds[%d] = %q, want %q", i, Kinds[i], k)
		}
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false", k)
		}
	}
	if ValidKind("task") || ValidKind("action") {
		t.Error("ValidKind accepted an action word as a kind (§2.5: a tag is addressing, never an action)")
	}
}

func TestValidKind_AllMembers(t *testing.T) {
	for _, k := range []Kind{KindAgent, KindTeam, KindCapability, KindNamespace, KindRemote, KindSession} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false, want true", k)
		}
	}
	for _, k := range []Kind{"", "Agent", "peer", "temp"} {
		if ValidKind(k) {
			t.Errorf("ValidKind(%q) = true, want false", k)
		}
	}
}
