package a2a

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// serverFor returns a ServerInfo for a booted posture. The origin is spelled in
// the test so the assertions below pin what the card is built from.
func serverFor(authTokenSet, sigEnforced bool) ServerInfo {
	return ServerInfo{
		Origin:                 "http://127.0.0.1:8767",
		Version:                "v1.2.3-abc12345",
		AuthTokenSet:           authTokenSet,
		AgentSignatureEnforced: sigEnforced,
	}
}

// TestBuildCard_ProjectsTheRegistryRow is the projection contract
// (specs/A2A-OPTION.md §3, AgentSkill §4.4.5): the card is made of the row's own
// facts — the id, the capability tags in row order — and nothing invented.
func TestBuildCard_ProjectsTheRegistryRow(t *testing.T) {
	card := BuildCard(CardRow{
		AgentID:        "agent-b",
		Capabilities:   []string{"solver", "router", "solver"},
		PushConfigured: true,
	}, serverFor(false, false))

	if card.Name != "agent-b" {
		t.Errorf("name = %q, want the registry id %q", card.Name, "agent-b")
	}
	if card.Description == "" {
		t.Error("description is empty — the spec marks it REQUIRED and an empty string tells a client nothing")
	}
	if !strings.Contains(card.Description, "agent-b") || !strings.Contains(card.Description, "solver") {
		t.Errorf("description %q does not name the row it projects (id + capabilities)", card.Description)
	}
	if card.Version != "v1.2.3-abc12345" {
		t.Errorf("version = %q, want the serving build identity", card.Version)
	}
	if card.DocumentationURL != "http://127.0.0.1:8767/docs" {
		t.Errorf("documentationUrl = %q, want the origin's /docs", card.DocumentationURL)
	}

	// Interfaces: one entry, the JSON-RPC binding, at the A2A path, carrying the
	// agent id as the tenant a client must echo back (§4.4.6, §8.3).
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("supportedInterfaces = %d entries, want 1 (JSON-RPC only, §2 of specs/A2A-OPTION.md)", len(card.SupportedInterfaces))
	}
	iface := card.SupportedInterfaces[0]
	want := AgentInterface{
		URL:             "http://127.0.0.1:8767/a2a",
		ProtocolBinding: ProtocolBindingJSONRPC,
		Tenant:          "agent-b",
		ProtocolVersion: ProtocolVersion,
	}
	if iface != want {
		t.Errorf("interface = %+v, want %+v", iface, want)
	}

	// Skills: the row's tags, in order, deduplicated — a repeated tag is one
	// skill, not two — with the REQUIRED members filled from the tag itself.
	wantSkills := []AgentSkill{
		{ID: "solver", Name: "solver", Description: `crier capability tag "solver" of agent "agent-b" (crier's registry stores capability tags, not skill prose).`, Tags: []string{"solver"}},
		{ID: "router", Name: "router", Description: `crier capability tag "router" of agent "agent-b" (crier's registry stores capability tags, not skill prose).`, Tags: []string{"router"}},
	}
	if !reflect.DeepEqual(card.Skills, wantSkills) {
		t.Errorf("skills = %+v, want %+v", card.Skills, wantSkills)
	}

	// Capabilities: streaming is TRUE because the JSON-RPC binding beside this
	// card serves SendStreamingMessage (INT-A2A-003); pushNotifications mirrors
	// the row's webhook.
	if !card.Capabilities.Streaming {
		t.Error("capabilities.streaming = false, want true — the JSON-RPC binding serves SendStreamingMessage (INT-A2A-003)")
	}
	if !card.Capabilities.PushNotifications {
		t.Error("capabilities.pushNotifications = false, want true — the row carries a webhook")
	}
	if card.Capabilities.Extensions != nil {
		t.Errorf("capabilities.extensions = %+v, want absent (crier declares no extension)", card.Capabilities.Extensions)
	}

	// Media modes: the delivery wire is JSON, and they are REQUIRED fields.
	if !reflect.DeepEqual(card.DefaultInputModes, []string{"application/json"}) {
		t.Errorf("defaultInputModes = %v, want [application/json]", card.DefaultInputModes)
	}
	if !reflect.DeepEqual(card.DefaultOutputModes, []string{"application/json"}) {
		t.Errorf("defaultOutputModes = %v, want [application/json]", card.DefaultOutputModes)
	}
}

// TestBuildCard_NoCapabilitiesIsAnEmptyArrayNotNull pins the shape a JSON null
// would break: `skills` is REQUIRED, so an agent with no capability tags gets
// `"skills": []`, never `"skills": null`.
func TestBuildCard_NoCapabilitiesIsAnEmptyArrayNotNull(t *testing.T) {
	card := BuildCard(CardRow{AgentID: "bare"}, serverFor(false, false))

	if card.Skills == nil {
		t.Fatal("skills is nil — a nil slice serializes as null, not as the REQUIRED empty array")
	}
	if len(card.Skills) != 0 {
		t.Fatalf("skills = %+v, want empty", card.Skills)
	}
	if !strings.Contains(card.Description, "none registered") {
		t.Errorf("description %q does not say the agent registered no capabilities", card.Description)
	}

	body, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"skills":[]`) {
		t.Errorf("serialized card carries %q, want \"skills\":[]", extractField(string(body), "skills"))
	}
}

// TestBuildCard_SecurityPosture is the auth half of the projection: the card
// declares what the server ACTUALLY enforces, and demands only what an A2A
// client can actually satisfy.
func TestBuildCard_SecurityPosture(t *testing.T) {
	cases := []struct {
		name             string
		authTokenSet     bool
		sigEnforced      bool
		wantSchemes      []string
		wantRequirements []string
	}{
		{
			name:        "no token, signatures off — crier requires nothing",
			wantSchemes: nil,
		},
		{
			name:             "no token, signatures enforced — the scheme is declared, not demanded",
			sigEnforced:      true,
			wantSchemes:      []string{SchemeAgentSignature},
			wantRequirements: nil,
		},
		{
			name:             "token set, signatures off — bearerAuth is required",
			authTokenSet:     true,
			wantSchemes:      []string{SchemeBearerAuth},
			wantRequirements: []string{SchemeBearerAuth},
		},
		{
			name:             "token set, signatures enforced — both declared, only bearer demanded",
			authTokenSet:     true,
			sigEnforced:      true,
			wantSchemes:      []string{SchemeAgentSignature, SchemeBearerAuth},
			wantRequirements: []string{SchemeBearerAuth},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := BuildCard(CardRow{AgentID: "agent-b"}, serverFor(tc.authTokenSet, tc.sigEnforced))

			if got := sortedSchemeNames(card.SecuritySchemes); !reflect.DeepEqual(got, tc.wantSchemes) {
				t.Errorf("securitySchemes = %v, want %v", got, tc.wantSchemes)
			}
			if got := requirementSchemeNames(card); !reflect.DeepEqual(got, tc.wantRequirements) {
				t.Errorf("securityRequirements = %v, want %v", got, tc.wantRequirements)
			}

			// The bearer scheme, when present, is the HTTP one crier's own
			// OpenAPI spec names bearerAuth — never the custom header.
			if s, ok := card.SecuritySchemes[SchemeBearerAuth]; ok {
				if s.HTTPAuth == nil || s.HTTPAuth.Scheme != "Bearer" {
					t.Errorf("bearerAuth = %+v, want an httpAuthSecurityScheme with scheme Bearer", s)
				}
				if s.APIKey != nil {
					t.Errorf("bearerAuth carries an apiKey member too — the spec's SecurityScheme is a oneof: %+v", s)
				}
			}
			// agentSignature, when present, is the apiKey-style header crier
			// does enforce — and it is never demanded of a generic client.
			if s, ok := card.SecuritySchemes[SchemeAgentSignature]; ok {
				if s.APIKey == nil || s.APIKey.Name != "X-Agent-Sig" || s.APIKey.Location != "header" {
					t.Errorf("agentSignature = %+v, want an apiKeySecurityScheme on the X-Agent-Sig header", s)
				}
				if s.HTTPAuth != nil {
					t.Errorf("agentSignature carries an httpAuth member too — the spec's SecurityScheme is a oneof: %+v", s)
				}
				for _, req := range card.SecurityRequirements {
					if _, demanded := req.Schemes[SchemeAgentSignature]; demanded {
						t.Error("agentSignature appears in securityRequirements — a generic A2A client cannot compute crier's ed25519 trio, so demanding it advertises a scheme that client is guaranteed to fail")
					}
				}
			}
		})
	}
}

// TestBuildCard_OmitsWhatCrierDoesNotHave pins the fields the card must NOT
// carry: crier has no provider identity to state and computes no card signature,
// so both are omitted rather than filled with a placeholder (§5.7, §8.4).
func TestBuildCard_OmitsWhatCrierDoesNotHave(t *testing.T) {
	card := BuildCard(CardRow{AgentID: "agent-b"}, serverFor(true, true))
	body, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"provider", "signatures", "iconUrl"} {
		if raw, ok := fields[absent]; ok {
			t.Errorf("card carries %q = %s, want it absent — crier has no such fact to state", absent, raw)
		}
	}
}

// TestCardETag_TracksTheBody pins the ETag's contract (§8.6.1): a strong,
// quoted tag derived from the exact bytes served, so any change to the
// projection is a different validator and an unchanged card keeps its own.
func TestCardETag_TracksTheBody(t *testing.T) {
	base := BuildCard(CardRow{AgentID: "agent-b", Capabilities: []string{"solver"}}, serverFor(false, false))
	baseBody := mustJSON(t, base)

	// Same inputs, same bytes, same tag — the property a conditional request
	// depends on.
	if got, want := CardETag(baseBody), CardETag(mustJSON(t, base)); got != want {
		t.Errorf("CardETag is not stable for identical cards: %s vs %s", got, want)
	}
	if !strings.HasPrefix(CardETag(baseBody), `"`) || !strings.HasSuffix(CardETag(baseBody), `"`) {
		t.Errorf("CardETag(%s) is not a quoted entity tag", CardETag(baseBody))
	}

	// Each moving input moves the tag: another agent, another capability set,
	// another auth posture.
	variants := map[string]AgentCard{
		"another agent":      BuildCard(CardRow{AgentID: "agent-c", Capabilities: []string{"solver"}}, serverFor(false, false)),
		"another capability": BuildCard(CardRow{AgentID: "agent-b", Capabilities: []string{"solver", "router"}}, serverFor(false, false)),
		"a webhook":          BuildCard(CardRow{AgentID: "agent-b", Capabilities: []string{"solver"}, PushConfigured: true}, serverFor(false, false)),
		"a bearer posture":   BuildCard(CardRow{AgentID: "agent-b", Capabilities: []string{"solver"}}, serverFor(true, false)),
	}
	for name, variant := range variants {
		if CardETag(mustJSON(t, variant)) == CardETag(baseBody) {
			t.Errorf("a card differing by %s carries the same ETag %s — a conditional request would serve stale content", name, CardETag(baseBody))
		}
	}
}

// TestConfig_OptedInIsFailClosed pins the per-agent half of the gate: absent and
// present-but-false are both "no".
func TestConfig_OptedInIsFailClosed(t *testing.T) {
	var absent *Config
	if absent.OptedIn() {
		t.Error("an absent block (nil) reports opted in — it must take no part in A2A")
	}
	if (&Config{}).OptedIn() {
		t.Error("an empty block reports opted in — the zero value is off")
	}
	if (&Config{Enabled: false}).OptedIn() {
		t.Error("an explicit false reports opted in")
	}
	if !(&Config{Enabled: true}).OptedIn() {
		t.Error("an explicit true does not report opted in")
	}
}

// mustJSON serializes a card the way the server serves it.
func mustJSON(t *testing.T, card AgentCard) []byte {
	t.Helper()
	body, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	return body
}

// sortedSchemeNames returns the securitySchemes keys in a stable order.
func sortedSchemeNames(schemes map[string]SecurityScheme) []string {
	if len(schemes) == 0 {
		return nil
	}
	names := make([]string, 0, len(schemes))
	for name := range schemes {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// requirementSchemeNames returns the scheme names every securityRequirements
// entry demands, in a stable order.
func requirementSchemeNames(card AgentCard) []string {
	var names []string
	for _, req := range card.SecurityRequirements {
		for name := range req.Schemes {
			names = append(names, name)
		}
	}
	sortStrings(names)
	return names
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// extractField returns the raw JSON of one top-level member, for failure
// messages that quote what was actually serialized.
func extractField(body, field string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return "(unparsable)"
	}
	return string(fields[field])
}
