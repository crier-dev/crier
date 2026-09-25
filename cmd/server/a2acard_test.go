package main

// a2acard_test.go — INT-A2A-002: the A2A Agent Card discovery route
// (GET /.well-known/agent-card.json).
//
// The row's acceptance criteria are the shape of this file:
//
//  1. the route exists ONLY while CR_A2A_ENABLED is true — with the switch off
//     the path answers the router's JSON 404, exactly as it did before the row;
//  2. a card is served ONLY for an agent whose registry row opted in — an agent
//     that did not opt in, and an id that is not a row at all, both get 404
//     rather than an empty card;
//  3. the served card VALIDATES against the A2A v1.0.0 AgentCard shape, checked
//     below against the shape transcribed from the specification's own proto
//     definition (never against the Go types under test);
//  4. the card reports crier's REAL auth posture, and the route adds no auth
//     requirement to any existing route (§6.4 of specs/A2A-OPTION.md).
//
// Every case boots the real server (run(nil) — middleware, router and stores
// included), so what is asserted is the registered surface, not a handler built
// by hand for the test.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
)

// specObject is one object of the A2A AgentCard shape: the members the spec
// defines and the JSON type each carries, plus the members the spec marks
// REQUIRED.
type specObject struct {
	members  map[string]string
	required []string
}

// theA2AAgentCardShape is the AgentCard shape of A2A v1.0.0, transcribed from
// the specification's proto definition (specification/a2a.proto — AgentCard
// §4.4.1, AgentInterface §4.4.6, AgentCapabilities §4.4.3, AgentSkill §4.4.5,
// SecurityScheme §4.5.1, HTTPAuthSecurityScheme §4.5.3, APIKeySecurityScheme
// §4.5.2, SecurityRequirement/SecurityScheme §4.5.1) with §5.5's rule that JSON
// member names are camelCase.
//
// It is pinned literally, NEVER derived from internal/a2a's types: that is what
// makes it a check on the card rather than a restatement of the code. The
// spec's published JSON Schema sets additionalProperties: false on every one of
// these objects, so an undefined member is as much a failure as a missing
// required one or a wrong type.
var theA2AAgentCardShape = struct {
	card                specObject
	iface               specObject
	capabilities        specObject
	extension           specObject
	skill               specObject
	securityScheme      specObject
	httpAuthScheme      specObject
	apiKeyScheme        specObject
	securityRequirement specObject
	stringList          specObject
}{
	card: specObject{
		members: map[string]string{
			"name": "string", "description": "string", "supportedInterfaces": "array",
			"provider": "object", "version": "string", "documentationUrl": "string",
			"capabilities": "object", "securitySchemes": "object", "securityRequirements": "array",
			"defaultInputModes": "array", "defaultOutputModes": "array", "skills": "array",
			"signatures": "array", "iconUrl": "string",
		},
		// The members the proto marks REQUIRED — always present, even when empty.
		required: []string{"name", "description", "supportedInterfaces", "version", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"},
	},
	iface: specObject{
		members: map[string]string{
			"url": "string", "protocolBinding": "string", "tenant": "string", "protocolVersion": "string",
		},
		required: []string{"url", "protocolBinding", "protocolVersion"},
	},
	capabilities: specObject{
		members: map[string]string{
			"streaming": "boolean", "pushNotifications": "boolean", "extensions": "array", "extendedAgentCard": "boolean",
		},
	},
	extension: specObject{
		members: map[string]string{
			"uri": "string", "description": "string", "required": "boolean", "params": "object",
		},
	},
	skill: specObject{
		members: map[string]string{
			"id": "string", "name": "string", "description": "string", "tags": "array",
			"examples": "array", "inputModes": "array", "outputModes": "array", "securityRequirements": "array",
		},
		required: []string{"id", "name", "description", "tags"},
	},
	securityScheme: specObject{
		// The spec's SecurityScheme is a oneof: exactly one member may be set.
		members: map[string]string{
			"apiKeySecurityScheme": "object", "httpAuthSecurityScheme": "object",
			"oauth2SecurityScheme": "object", "openIdConnectSecurityScheme": "object", "mtlsSecurityScheme": "object",
		},
	},
	httpAuthScheme: specObject{
		members:  map[string]string{"description": "string", "scheme": "string", "bearerFormat": "string"},
		required: []string{"scheme"},
	},
	apiKeyScheme: specObject{
		members:  map[string]string{"description": "string", "location": "string", "name": "string"},
		required: []string{"location", "name"},
	},
	securityRequirement: specObject{members: map[string]string{"schemes": "object"}},
	stringList:          specObject{members: map[string]string{"list": "array"}},
}

// jsonKind names the JSON type of a raw value — enough to tell a string from an
// array from a boolean without deciding what the value SHOULD be.
func jsonKind(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// assertSpecObject checks one object against its spec shape and returns its
// members. Undefined members, wrong types and missing REQUIRED members are each
// reported with the path they sit at.
func assertSpecObject(t *testing.T, path string, raw json.RawMessage, spec specObject) map[string]json.RawMessage {
	t.Helper()
	if kind := jsonKind(raw); kind != "object" {
		t.Fatalf("%s: is %s, want an object", path, kind)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	for name, value := range obj {
		want, ok := spec.members[name]
		if !ok {
			t.Errorf("%s: member %q is not part of the A2A AgentCard shape (the spec's schema sets additionalProperties: false on %s)", path, name, path)
			continue
		}
		if got := jsonKind(value); got != want {
			t.Errorf("%s.%s: is %s, want %s", path, name, got, want)
		}
	}
	for _, name := range spec.required {
		if _, ok := obj[name]; !ok {
			t.Errorf("%s: REQUIRED member %q is absent", path, name)
		}
	}
	return obj
}

// assertCardMatchesTheSpecShape validates a fetched card against the spec shape
// above, member by member and nested object by nested object.
func assertCardMatchesTheSpecShape(t *testing.T, body []byte) {
	t.Helper()
	card := assertSpecObject(t, "agentCard", body, theA2AAgentCardShape.card)

	// supportedInterfaces: at least one, each a full AgentInterface. The
	// interface URL must be absolute (§4.4.6: "must be a valid absolute HTTPS
	// URL in production") — a relative one would be unusable by a client.
	interfaces := decodeArray(t, "agentCard.supportedInterfaces", card["supportedInterfaces"])
	if len(interfaces) == 0 {
		t.Error("agentCard.supportedInterfaces is empty — a client cannot select a transport")
	}
	for i, iface := range interfaces {
		path := fmt.Sprintf("agentCard.supportedInterfaces[%d]", i)
		obj := assertSpecObject(t, path, iface, theA2AAgentCardShape.iface)
		var url string
		if err := json.Unmarshal(obj["url"], &url); err != nil {
			t.Errorf("%s.url: decode: %v", path, err)
			continue
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			t.Errorf("%s.url = %q, want an absolute URL", path, url)
		}
	}

	// capabilities: streaming/pushNotifications are booleans (present), and any
	// declared extension is a full AgentExtension.
	caps := assertSpecObject(t, "agentCard.capabilities", card["capabilities"], theA2AAgentCardShape.capabilities)
	for _, name := range []string{"streaming", "pushNotifications"} {
		var v bool
		if err := json.Unmarshal(caps[name], &v); err != nil {
			t.Errorf("agentCard.capabilities.%s: %v", name, err)
		}
	}
	if raw, ok := caps["extensions"]; ok {
		for i, ext := range decodeArray(t, "agentCard.capabilities.extensions", raw) {
			assertSpecObject(t, fmt.Sprintf("agentCard.capabilities.extensions[%d]", i), ext, theA2AAgentCardShape.extension)
		}
	}

	// skills: each a full AgentSkill with its REQUIRED members.
	skills := decodeArray(t, "agentCard.skills", card["skills"])
	for i, skill := range skills {
		assertSpecObject(t, fmt.Sprintf("agentCard.skills[%d]", i), skill, theA2AAgentCardShape.skill)
	}

	// securitySchemes: a MAP whose keys are the caller's own scheme names, so
	// only each VALUE is checked — it must be a oneof carrying exactly one
	// scheme kind, with that kind's own required members.
	if raw, ok := card["securitySchemes"]; ok {
		if kind := jsonKind(raw); kind != "object" {
			t.Errorf("agentCard.securitySchemes is %s, want an object", kind)
		} else {
			var schemes map[string]json.RawMessage
			if err := json.Unmarshal(raw, &schemes); err != nil {
				t.Errorf("agentCard.securitySchemes: decode: %v", err)
			}
			for name, schemeRaw := range schemes {
				path := fmt.Sprintf("agentCard.securitySchemes[%q]", name)
				scheme := assertSpecObject(t, path, schemeRaw, theA2AAgentCardShape.securityScheme)
				set := 0
				for kind := range theA2AAgentCardShape.securityScheme.members {
					if _, ok := scheme[kind]; ok {
						set++
					}
				}
				if set != 1 {
					t.Errorf("%s: carries %d scheme kinds, want exactly 1 (the spec models SecurityScheme as a oneof)", path, set)
				}
				if raw, ok := scheme["httpAuthSecurityScheme"]; ok {
					assertSpecObject(t, path+".httpAuthSecurityScheme", raw, theA2AAgentCardShape.httpAuthScheme)
				}
				if raw, ok := scheme["apiKeySecurityScheme"]; ok {
					assertSpecObject(t, path+".apiKeySecurityScheme", raw, theA2AAgentCardShape.apiKeyScheme)
				}
			}
		}
	}

	// securityRequirements: each entry's schemes map holds StringList values.
	if raw, ok := card["securityRequirements"]; ok {
		for i, req := range decodeArray(t, "agentCard.securityRequirements", raw) {
			path := fmt.Sprintf("agentCard.securityRequirements[%d]", i)
			obj := assertSpecObject(t, path, req, theA2AAgentCardShape.securityRequirement)
			schemesRaw, ok := obj["schemes"]
			if !ok {
				t.Errorf("%s: no schemes map", path)
				continue
			}
			var schemes map[string]json.RawMessage
			if err := json.Unmarshal(schemesRaw, &schemes); err != nil {
				t.Errorf("%s.schemes: decode: %v", path, err)
				continue
			}
			for name, list := range schemes {
				assertSpecObject(t, fmt.Sprintf("%s.schemes[%q]", path, name), list, theA2AAgentCardShape.stringList)
			}
		}
	}
}

// decodeArray decodes a raw JSON array, reporting its path on failure.
func decodeArray(t *testing.T, path string, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	if raw == nil {
		t.Fatalf("%s is absent", path)
	}
	if kind := jsonKind(raw); kind != "array" {
		t.Fatalf("%s: is %s, want an array", path, kind)
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	return out
}

// a2aCardAgent registers one agent on a booted server, optionally opting it in
// to A2A and optionally attaching a webhook (the agent's push channel).
func a2aCardAgent(t *testing.T, client *http.Client, baseURL, id string, capabilities []string, optIn, webhook bool) {
	t.Helper()
	body := map[string]any{
		"id":           id,
		"public_key":   strings.Repeat("ab", 32),
		"capabilities": capabilities,
	}
	if optIn {
		body["a2a"] = map[string]any{"enabled": true}
	}
	if webhook {
		body["webhook"] = map[string]any{"url": "http://127.0.0.1:1/hook"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/agents", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build registration: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status %d body %s, want 201", id, resp.StatusCode, got)
	}
}

// getCard fetches the discovery path for one agent (or with agentID "" for the
// path as the scanner sees it: no selector at all).
func getCard(t *testing.T, client *http.Client, baseURL, agentID string) *http.Response {
	t.Helper()
	url := baseURL + a2a.AgentCardPath
	if agentID != "" {
		url += "?" + a2a.AgentCardAgentQueryParam + "=" + agentID
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build card request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("fetch card: %v", err)
	}
	return resp
}

// TestA2ACard_RouteExistsOnlyWhenTheOptionIsOn is acceptance criterion 1 and 4
// together: with CR_A2A_ENABLED unset the path is not registered at all (the
// router's JSON 404, byte-shaped like every other unregistered path), and with
// it set the route exists — which the selector-less request proves by answering
// 400 ("which agent?") instead of 404 ("no such route").
func TestA2ACard_RouteExistsOnlyWhenTheOptionIsOn(t *testing.T) {
	t.Run("switch unset: the path is not registered", func(t *testing.T) {
		baseURL := startTestServerWithEnv(t, nil)
		client := &http.Client{Timeout: 5 * time.Second}

		resp := getCard(t, client, baseURL, "any-agent")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 with the A2A option off", a2a.AgentCardPath, resp.StatusCode)
		}
		if got := errorOfBody(t, string(body)); got != notFoundMessage {
			t.Errorf("404 body = %q, want the router's own %q — with the switch off this path is indistinguishable from any unregistered one", got, notFoundMessage)
		}
	})

	t.Run("switch on: the route is registered", func(t *testing.T) {
		baseURL := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
		client := &http.Client{Timeout: 5 * time.Second}

		// No selector: a registered route, and a request it cannot answer. 400
		// rather than 404 is the point — the route exists.
		resp := getCard(t, client, baseURL, "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s (no agent_id) = %d body %s, want 400", a2a.AgentCardPath, resp.StatusCode, body)
		}
		if got := errorOfBody(t, string(body)); !strings.Contains(got, a2a.AgentCardAgentQueryParam) {
			t.Errorf("400 body = %q, want it to name the required %s parameter", got, a2a.AgentCardAgentQueryParam)
		}

		// The route answers GET only; the router's own 405 fallback covers the
		// rest (still the JSON envelope, and with the switch off this path
		// answers 404 instead).
		req, err := http.NewRequest(http.MethodPost, baseURL+a2a.AgentCardPath, nil)
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		post, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", a2a.AgentCardPath, err)
		}
		defer post.Body.Close()
		if post.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", a2a.AgentCardPath, post.StatusCode)
		}
	})
}

// TestA2ACard_ServedOnlyForOptedInAgents is acceptance criteria 2 and 3: the
// card is served for an opted-in agent and validates against the spec's shape,
// and every other id gets 404 — never an empty card.
func TestA2ACard_ServedOnlyForOptedInAgents(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}

	a2aCardAgent(t, client, baseURL, "opted-in", []string{"solver", "router"}, true, true)
	a2aCardAgent(t, client, baseURL, "not-opted-in", []string{"solver"}, false, false)

	resp := getCard(t, client, baseURL, "opted-in")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read card body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET card for an opted-in agent = %d body %s, want 200", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != a2a.CardMediaType {
		t.Errorf("Content-Type = %q, want %q (the A2A media type registration)", ct, a2a.CardMediaType)
	}

	// The spec's shape, checked member by member.
	assertCardMatchesTheSpecShape(t, body)

	// The projection itself: the row's id, its capability tags as skills in row
	// order, the interface naming this server's binding, and the tenant a
	// client must echo back.
	var card struct {
		Name                string `json:"name"`
		Description         string `json:"description"`
		Version             string `json:"version"`
		DocumentationURL    string `json:"documentationUrl"`
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			Tenant          string `json:"tenant"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Streaming         bool `json:"streaming"`
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
		Skills []struct {
			ID   string   `json:"id"`
			Tags []string `json:"tags"`
		} `json:"skills"`
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Name != "opted-in" {
		t.Errorf("card name = %q, want the registry id", card.Name)
	}
	if card.Description == "" || card.Version == "" || card.DocumentationURL == "" {
		t.Errorf("card is missing generated metadata: description=%q version=%q documentationUrl=%q", card.Description, card.Version, card.DocumentationURL)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("supportedInterfaces = %d, want 1", len(card.SupportedInterfaces))
	}
	iface := card.SupportedInterfaces[0]
	if iface.ProtocolBinding != "JSONRPC" || iface.ProtocolVersion != "1.0" {
		t.Errorf("interface = %+v, want the JSONRPC binding at protocol version 1.0", iface)
	}
	if iface.Tenant != "opted-in" {
		t.Errorf("interface tenant = %q, want the agent id (a bus routes by it)", iface.Tenant)
	}
	if !strings.HasPrefix(iface.URL, baseURL) {
		t.Errorf("interface url = %q, want it on the origin the client reached (%s)", iface.URL, baseURL)
	}
	var ids []string
	for _, skill := range card.Skills {
		ids = append(ids, skill.ID)
	}
	if strings.Join(ids, ",") != "solver,router" {
		t.Errorf("skill ids = %v, want the row's capabilities in order", ids)
	}
	if card.Capabilities.Streaming {
		t.Error("capabilities.streaming = true — no A2A streaming binding is served yet")
	}
	if !card.Capabilities.PushNotifications {
		t.Error("capabilities.pushNotifications = false — this row carries a webhook, so it has a push channel")
	}

	// The refusals: an agent that did not opt in, and an id that is not a row.
	for _, tc := range []struct{ name, agentID string }{
		{"registered but not opted in", "not-opted-in"},
		{"no such registry row", "ghost-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := getCard(t, client, baseURL, tc.agentID)
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("GET card for %s = %d body %s, want 404", tc.name, resp.StatusCode, raw)
			}
			// Never an empty card: the refusal is the repo's error envelope,
			// and it says nothing that decodes as an AgentCard.
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode refusal body %s: %v", raw, err)
			}
			if _, isCard := payload["name"]; isCard {
				t.Fatalf("404 body %s carries card members — a non-opted-in agent must not be advertised", raw)
			}
			if _, ok := payload["error"]; !ok {
				t.Fatalf("404 body %s carries no error member", raw)
			}
		})
	}
}

// TestA2ACard_CachingContract pins the caching guidance the row carries (§8.6):
// a validator derived from the card content, a bounded max-age, and a
// conditional request answered 304 with no body.
func TestA2ACard_CachingContract(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	a2aCardAgent(t, client, baseURL, "cached-agent", []string{"solver"}, true, false)

	resp := getCard(t, client, baseURL, "cached-agent")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET card = %d, want 200", resp.StatusCode)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag — §8.6.1 asks for a validator derived from the card")
	}
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(cacheControl, fmt.Sprintf("max-age=%d", a2a.CardCacheMaxAgeSeconds)) {
		t.Errorf("Cache-Control = %q, want a max-age=%d directive", cacheControl, a2a.CardCacheMaxAgeSeconds)
	} else if strings.Contains(cacheControl, "public") {
		t.Errorf("Cache-Control = %q — the card is served behind crier's auth, so a SHARED cache must not store it", cacheControl)
	}

	// A conditional request for the same card: 304, the same validator, no body.
	req, err := http.NewRequest(http.MethodGet, baseURL+a2a.AgentCardPath+"?"+a2a.AgentCardAgentQueryParam+"=cached-agent", nil)
	if err != nil {
		t.Fatalf("build conditional request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("If-None-Match", etag)
	conditional, err := client.Do(req)
	if err != nil {
		t.Fatalf("conditional request: %v", err)
	}
	defer conditional.Body.Close()
	conditionalBody, _ := io.ReadAll(conditional.Body)
	if conditional.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d body %s, want 304", conditional.StatusCode, conditionalBody)
	}
	if conditional.Header.Get("ETag") != etag {
		t.Errorf("304 ETag = %q, want the same validator %q", conditional.Header.Get("ETag"), etag)
	}
	if len(conditionalBody) != 0 {
		t.Errorf("304 carried a body (%d bytes) — a 304 must not", len(conditionalBody))
	}

	// The tag follows the content: another agent's card is a different document
	// and a different validator, so a client holding one cannot be served the
	// other.
	a2aCardAgent(t, client, baseURL, "other-agent", []string{"solver", "router"}, true, false)
	other := getCard(t, client, baseURL, "other-agent")
	defer other.Body.Close()
	otherBody, _ := io.ReadAll(other.Body)
	if other.Header.Get("ETag") == etag {
		t.Errorf("two different cards share the validator %q — a conditional request would answer with stale content", etag)
	}
	if other.Header.Get("ETag") != a2a.CardETag(otherBody) {
		t.Errorf("served ETag %q does not describe the served body", other.Header.Get("ETag"))
	}

	// Re-reading the same row serves the identical body and validator.
	resp2 := getCard(t, client, baseURL, "cached-agent")
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.Header.Get("ETag") != a2a.CardETag(body2) {
		t.Errorf("served ETag %q does not describe the served body — a conditional request would answer the wrong question", resp2.Header.Get("ETag"))
	}
	if resp2.Header.Get("ETag") != etag || string(body2) != string(body) {
		t.Errorf("the card for an unchanged row changed between two reads:\netag %q -> %q\nbody %s -> %s", etag, resp2.Header.Get("ETag"), body, body2)
	}
}

// TestA2ACard_NoNewAuthRequirementOnExistingRoutes pins §6.4 of
// specs/A2A-OPTION.md for this route: it inherits the middleware chain
// unchanged. With CR_AUTH_TOKEN set the card needs the same Bearer header as
// every other route (which is what the card itself declares), and the
// pre-existing exempt list is untouched — /health stays public with the option
// on or off.
func TestA2ACard_NoNewAuthRequirementOnExistingRoutes(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true", "CR_AUTH_TOKEN": "test-token"})
	client := &http.Client{Timeout: 5 * time.Second}
	a2aCardAgent(t, client, baseURL, "authed-agent", []string{"solver"}, true, false)

	// Without the bearer: the same 401 shape every authenticated route answers.
	req, err := http.NewRequest(http.MethodGet, baseURL+a2a.AgentCardPath+"?"+a2a.AgentCardAgentQueryParam+"=authed-agent", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated card request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated card request = %d body %s, want 401", resp.StatusCode, body)
	}
	if got := errorOfBody(t, string(body)); got != "missing Authorization header" {
		t.Errorf("401 body = %q, want the middleware's own message (no A2A-specific auth path)", got)
	}

	// The exempt list is unchanged: /health is public with the option ON, just
	// as it is with it off.
	health, err := client.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d with the A2A option on, want 200 (the exempt list is untouched)", health.StatusCode)
	}

	// And the card it does serve declares that posture: bearerAuth is both
	// declared and demanded, while the crier-native agentSignature scheme
	// (enforced by default) is declared and NOT demanded — a generic A2A client
	// holds no crier key.
	resp = getCard(t, client, baseURL, "authed-agent")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated card request = %d body %s, want 200", resp.StatusCode, raw)
	}
	var card struct {
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
		SecurityReqs    []struct {
			Schemes map[string]json.RawMessage `json:"schemes"`
		} `json:"securityRequirements"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	var declared []string
	for name := range card.SecuritySchemes {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	if strings.Join(declared, ",") != "agentSignature,bearerAuth" {
		t.Errorf("securitySchemes = %v, want agentSignature and bearerAuth declared (the postures actually in force)", declared)
	}
	var demanded []string
	for _, req := range card.SecurityReqs {
		for name := range req.Schemes {
			demanded = append(demanded, name)
		}
	}
	sort.Strings(demanded)
	if strings.Join(demanded, ",") != "bearerAuth" {
		t.Errorf("securityRequirements = %v, want bearerAuth only — the ed25519 trio is not computable by a generic A2A client", demanded)
	}
}

// TestA2ACardPathIsTheSpecPath pins the wire path: §14.3 registers the suffix
// `agent-card.json` under `/.well-known/`, and the route, the served card's
// interface and the documentation all name the same string.
func TestA2ACardPathIsTheSpecPath(t *testing.T) {
	if a2a.AgentCardPath != "/.well-known/agent-card.json" {
		t.Fatalf("a2a.AgentCardPath = %q, want the well-known discovery path from A2A §8.2/§14.3", a2a.AgentCardPath)
	}
	if a2a.AgentCardAgentQueryParam != "agent_id" {
		t.Fatalf("a2a.AgentCardAgentQueryParam = %q, want agent_id", a2a.AgentCardAgentQueryParam)
	}
}
