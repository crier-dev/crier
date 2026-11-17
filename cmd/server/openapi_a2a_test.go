// openapi_a2a_test.go — INT-019: the A2A protocol is part of the SERVED contract.
//
// The A2A surfaces have been mounted since INT-A2A-002/003, but the served
// /openapi.json declared neither path, so a stranger reading the contract could
// not discover the protocol at all. This gate closes that direction:
//
//  1. TestOpenAPIDocumentsA2ARoutes drives the REAL /openapi.json handler over
//     httptest (the embedded asset bytes the server actually serves) and
//     requires both A2A paths to be declared — with the exact path strings, the
//     exact media types, the exact query/header parameter names and the exact
//     opt-in gate the HANDLERS use, each read from internal/a2a's own exported
//     constants. A document that drifts from the handlers fails here, and so
//     does a document that only exists on disk: nothing here reads
//     docs/openapi.yaml.
//
//  2. TestA2AGateDetectsAnUndeclaredPath is the in-repo proof that the check
//     above can FAIL: it removes one A2A path from the served bytes and
//     requires the checker to name exactly that path. Its GREEN side runs the
//     same checker over the unmutated response, so the RED and GREEN sides
//     cannot both pass vacuously.
//
// What is deliberately NOT claimed here: no JSON-RPC method is exercised, and
// no task lifecycle is asserted (that is a different row's contract). This file
// is about the CONTRACT's discoverability — the paths, their framing and their
// gate — not about the behaviour behind them.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/a2a"
)

// a2aSpecOperation is the part of one declared operation this gate asserts on.
type a2aSpecOperation struct {
	OperationID string   `json:"operationId"`
	Tags        []string `json:"tags"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Gate        string   `json:"x-crier-gate"`
	Parameters  []struct {
		Name     string          `json:"name"`
		In       string          `json:"in"`
		Required bool            `json:"required"`
		Schema   json.RawMessage `json:"schema"`
	} `json:"parameters"`
	RequestBody struct {
		Required bool `json:"required"`
		Content  map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Description string `json:"description"`
		Content     map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

// a2aSpec is the served spec, reduced to what this gate reads.
type a2aSpec struct {
	Paths      map[string]map[string]a2aSpecOperation `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

// servedOpenAPISpec fetches whatever the running server would answer
// GET /openapi.json with — the embedded asset bytes, produced by the same
// handler main.go mounts — and returns them plus the status and content type.
func servedOpenAPISpec(t *testing.T) (body []byte, status int, contentType string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleOpenAPIJSON(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	return rec.Body.Bytes(), rec.Code, rec.Header().Get("Content-Type")
}

// parseA2ASpec decodes a served spec into the reduced shape.
func parseA2ASpec(t *testing.T, body []byte) a2aSpec {
	t.Helper()
	var spec a2aSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("the served /openapi.json is not valid JSON: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("the served /openapi.json declares no paths — this gate would be vacuous")
	}
	return spec
}

// a2aDispatchedMethods is the method set the JSON-RPC handler dispatches,
// transcribed from the constants cmd/server/a2a.go's switch is written against
// (a2a.Method*, plus the push-configuration methods from a2apush.go). It is
// built from the package constants rather than from string literals, so a
// renamed method is a compile error here instead of a silent drift.
func a2aDispatchedMethods() []string {
	return []string{
		a2a.MethodSendMessage,
		a2a.MethodSendStreamingMessage,
		a2a.MethodGetTask,
		a2a.MethodListTasks,
		a2a.MethodCancelTask,
		a2a.MethodSubscribeToTask,
		a2a.MethodCreateTaskPushNotificationConfig,
		a2a.MethodGetTaskPushNotificationConfig,
		a2a.MethodListTaskPushNotificationConfigs,
		a2a.MethodDeleteTaskPushNotificationConfig,
	}
}

// documentedA2AMethods reads the method table out of the /a2a operation's
// description — the rows spelled "| `<Method>` | params | result |" — and
// returns the method names it lists.
func documentedA2AMethods(description string) []string {
	var methods []string
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rest := strings.TrimPrefix(line, "| `")
		name, _, ok := strings.Cut(rest, "`")
		if !ok {
			continue
		}
		methods = append(methods, name)
	}
	return methods
}

// schemaRef returns the $ref an operation's content entry points at, or "".
func schemaRef(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var ref struct {
		Ref string `json:"$ref"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ""
	}
	return ref.Ref
}

// a2aContractGaps is the decision procedure: it returns one human-readable
// entry per way the SERVED spec fails to declare the A2A surface the handlers
// mount. An empty result means the served contract is complete.
//
// Every expectation that has a name in internal/a2a is read from that constant
// (the path, the query parameter, the media types, the version header), so this
// function can only agree with the handlers when the document agrees with them.
// It takes bytes, never *testing.T, so TestA2AGateDetectsAnUndeclaredPath can
// drive it with a mutated body.
func a2aContractGaps(body []byte) []string {
	var spec a2aSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return []string{"the served spec is not valid JSON: " + err.Error()}
	}
	var gaps []string
	add := func(format string, args ...any) {
		gaps = append(gaps, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}

	// ---- 1. Both paths are declared, at the strings the handlers register. ----
	cardPath, ok := spec.Paths[a2a.AgentCardPath]
	if !ok {
		add("%s: the discovery card path the handler registers is not declared", a2a.AgentCardPath)
	}
	rpcPath, ok := spec.Paths[a2a.JSONRPCBindingPath]
	if !ok {
		add("%s: the JSON-RPC binding path the handler registers is not declared", a2a.JSONRPCBindingPath)
	}

	// ---- 2. Each path declares exactly the method it is mounted with. ----
	if cardPath != nil {
		if len(cardPath) != 1 {
			add("%s declares %d operations, want exactly 1 (GET — the route is registered with Methods(http.MethodGet))", a2a.AgentCardPath, len(cardPath))
		}
		op, ok := cardPath["get"]
		if !ok {
			add("%s: no GET operation (registerA2ACardRoute registers GET only)", a2a.AgentCardPath)
		} else {
			if op.Gate != "CR_A2A_ENABLED" {
				add("%s GET x-crier-gate = %q, want %q (both A2A paths are registered only while that switch is set)", a2a.AgentCardPath, op.Gate, "CR_A2A_ENABLED")
			}
			if op.OperationID != "a2aAgentCard" {
				add("%s GET operationId = %q, want %q", a2a.AgentCardPath, op.OperationID, "a2aAgentCard")
			}
			if len(op.Tags) != 1 || op.Tags[0] != "a2a" {
				add("%s GET tags = %v, want [a2a]", a2a.AgentCardPath, op.Tags)
			}
			var tenantParam *struct {
				Name     string          `json:"name"`
				In       string          `json:"in"`
				Required bool            `json:"required"`
				Schema   json.RawMessage `json:"schema"`
			}
			for i := range op.Parameters {
				if op.Parameters[i].In == "query" {
					tenantParam = &op.Parameters[i]
				}
			}
			switch {
			case tenantParam == nil:
				add("%s GET declares no query parameter — the handler requires %q", a2a.AgentCardPath, a2a.AgentCardAgentQueryParam)
			case tenantParam.Name != a2a.AgentCardAgentQueryParam:
				add("%s GET query parameter = %q, want %q (the handler reads that exact name)", a2a.AgentCardPath, tenantParam.Name, a2a.AgentCardAgentQueryParam)
			case !tenantParam.Required:
				add("%s GET query parameter %q is documented optional, but a missing value is the handler's 400", a2a.AgentCardPath, tenantParam.Name)
			}
			if op.RequestBody.Required || len(op.RequestBody.Content) != 0 {
				add("%s GET declares a request body; the handler reads none", a2a.AgentCardPath)
			}
			if got := schemaRef(op.Responses["200"].Content[a2a.CardMediaType].Schema); got != "#/components/schemas/A2AAgentCard" {
				add("%s GET 200 content %s schema = %q, want #/components/schemas/A2AAgentCard", a2a.AgentCardPath, a2a.CardMediaType, got)
			}
			for _, code := range []string{"200", "304", "400", "401", "404", "500"} {
				if _, ok := op.Responses[code]; !ok {
					add("%s GET has no %s response — that is part of the handler's contract", a2a.AgentCardPath, code)
				}
			}
		}
	}

	if rpcPath != nil {
		if len(rpcPath) != 1 {
			add("%s declares %d operations, want exactly 1 (POST — registerA2ARoute registers the single JSON-RPC binding)", a2a.JSONRPCBindingPath, len(rpcPath))
		}
		op, ok := rpcPath["post"]
		if !ok {
			add("%s: no POST operation (registerA2ARoute registers POST only)", a2a.JSONRPCBindingPath)
		} else {
			if op.Gate != "CR_A2A_ENABLED" {
				add("%s POST x-crier-gate = %q, want %q", a2a.JSONRPCBindingPath, op.Gate, "CR_A2A_ENABLED")
			}
			if op.OperationID != "a2aJSONRPC" {
				add("%s POST operationId = %q, want %q", a2a.JSONRPCBindingPath, op.OperationID, "a2aJSONRPC")
			}
			if len(op.Tags) != 1 || op.Tags[0] != "a2a" {
				add("%s POST tags = %v, want [a2a]", a2a.JSONRPCBindingPath, op.Tags)
			}
			if !op.RequestBody.Required {
				add("%s POST requestBody is documented optional; the handler requires parsable JSON", a2a.JSONRPCBindingPath)
			}
			for _, mediaType := range []string{a2a.RPCMediaType, "application/json"} {
				if _, ok := op.RequestBody.Content[mediaType]; !ok {
					add("%s POST request body does not declare %s — a2aMediaTypeAccepted reads both", a2a.JSONRPCBindingPath, mediaType)
				}
			}
			if got := schemaRef(op.RequestBody.Content[a2a.RPCMediaType].Schema); got != "#/components/schemas/A2AJSONRPCRequest" {
				add("%s POST request schema = %q, want #/components/schemas/A2AJSONRPCRequest", a2a.JSONRPCBindingPath, got)
			}
			if got := schemaRef(op.Responses["200"].Content[a2a.RPCMediaType].Schema); got != "#/components/schemas/A2AJSONRPCResponse" {
				add("%s POST 200 %s schema = %q, want #/components/schemas/A2AJSONRPCResponse", a2a.JSONRPCBindingPath, a2a.RPCMediaType, got)
			}
			if _, ok := op.Responses["200"].Content[a2a.StreamMediaType]; !ok {
				add("%s POST 200 does not declare %s — SendStreamingMessage answers a stream, not a JSON body", a2a.JSONRPCBindingPath, a2a.StreamMediaType)
			}
			for _, code := range []string{"200", "400", "401", "404", "413", "415"} {
				if _, ok := op.Responses[code]; !ok {
					add("%s POST has no %s response — that is part of the handler's contract", a2a.JSONRPCBindingPath, code)
				}
			}

			// The A2A service parameters travel as headers; the document must
			// name the exact header the handler reads.
			var versionHeader bool
			for _, p := range op.Parameters {
				if p.In == "header" && p.Name == a2a.A2AVersionHeader {
					versionHeader = true
				}
			}
			if !versionHeader {
				add("%s POST does not declare the %s header the handler negotiates before dispatch", a2a.JSONRPCBindingPath, a2a.A2AVersionHeader)
			}

			// The method table: exactly the methods the handler dispatches,
			// no more (a documented method the switch would answer -32601 to
			// is a false promise) and no fewer (an undiscoverable one).
			documented := documentedA2AMethods(op.Description)
			if len(documented) == 0 {
				add("%s POST describes no JSON-RPC methods at all — the binding would be undiscoverable", a2a.JSONRPCBindingPath)
			}
			dispatched := map[string]bool{}
			for _, m := range a2aDispatchedMethods() {
				dispatched[m] = true
			}
			seen := map[string]bool{}
			for _, m := range documented {
				seen[m] = true
				if !dispatched[m] {
					add("%s POST documents method %q, which the handler's switch does not dispatch (it would answer -32601)", a2a.JSONRPCBindingPath, m)
				}
			}
			for _, m := range a2aDispatchedMethods() {
				if !seen[m] {
					add("%s POST does not document method %q, which the handler dispatches", a2a.JSONRPCBindingPath, m)
				}
			}
		}
	}

	// ---- 3. Every schema the two paths reference is declared. ----
	for _, ref := range a2aSpecRefs(spec) {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if name == ref {
			continue
		}
		if _, ok := spec.Components.Schemas[name]; !ok {
			add("the A2A contract references %s, which components.schemas does not declare", ref)
		}
	}

	sort.Strings(gaps)
	return gaps
}

// a2aSpecRefs collects the schema $refs reachable from the two A2A paths.
func a2aSpecRefs(spec a2aSpec) []string {
	var refs []string
	collect := func(raw json.RawMessage) {
		if ref := schemaRef(raw); ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, path := range []string{a2a.AgentCardPath, a2a.JSONRPCBindingPath} {
		item, ok := spec.Paths[path]
		if !ok {
			continue
		}
		for _, op := range item {
			for _, media := range op.RequestBody.Content {
				collect(media.Schema)
			}
			for _, response := range op.Responses {
				for _, media := range response.Content {
					collect(media.Schema)
				}
			}
		}
	}
	return refs
}

// TestOpenAPIDocumentsA2ARoutes is the INT-019 contract gate: the SERVED
// /openapi.json declares both A2A paths, with the handlers' own paths, media
// types, parameter names and opt-in gate.
func TestOpenAPIDocumentsA2ARoutes(t *testing.T) {
	body, status, contentType := servedOpenAPISpec(t)
	if status != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d, want %d", status, http.StatusOK)
	}
	if contentType != "application/json" {
		t.Fatalf("GET /openapi.json: Content-Type %q, want %q", contentType, "application/json")
	}
	spec := parseA2ASpec(t, body)
	for _, path := range []string{a2a.AgentCardPath, a2a.JSONRPCBindingPath} {
		if _, ok := spec.Paths[path]; !ok {
			t.Errorf("the SERVED /openapi.json does not declare %s — the route is mounted but absent from the contract", path)
		}
	}
	for _, gap := range a2aContractGaps(body) {
		t.Errorf("served A2A contract gap: %s", gap)
	}
	t.Logf("served /openapi.json declares %d paths; both A2A paths are present with gate CR_A2A_ENABLED", len(spec.Paths))
}

// TestA2AGateDetectsAnUndeclaredPath proves the gate above can FAIL: deleting
// one A2A path from the served bytes must be reported, naming that path. The
// unmutated body must report zero gaps in the same run, so the RED and GREEN
// sides cannot both pass vacuously.
func TestA2AGateDetectsAnUndeclaredPath(t *testing.T) {
	body, status, _ := servedOpenAPISpec(t)
	if status != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d, want %d", status, http.StatusOK)
	}
	if gaps := a2aContractGaps(body); len(gaps) != 0 {
		t.Fatalf("the served body does not satisfy the A2A contract, so the RED side would prove nothing: %v", gaps)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse served spec: %v", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("served spec has no paths object")
	}
	if _, ok := paths[a2a.JSONRPCBindingPath]; !ok {
		t.Fatalf("served spec has no %s path to delete — re-point this self-test", a2a.JSONRPCBindingPath)
	}
	delete(paths, a2a.JSONRPCBindingPath)
	mutated, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode mutated spec: %v", err)
	}

	gaps := a2aContractGaps(mutated)
	if len(gaps) == 0 {
		t.Fatal("the gate passed a spec with the /a2a path deleted — it cannot fail")
	}
	var named bool
	for _, gap := range gaps {
		if strings.Contains(gap, a2a.JSONRPCBindingPath) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the gate reported %d gap(s) but none names the deleted path %s: %v", len(gaps), a2a.JSONRPCBindingPath, gaps)
	}
	t.Logf("RED side: deleting %s yields %d gap(s), first: %s", a2a.JSONRPCBindingPath, len(gaps), gaps[0])
}

// TestA2ASpecOperationIDsAreUnique keeps the two A2A operationIds from colliding
// with anything else in the document: internal/mcp's spec-drift gate classifies
// operations BY operationId, so a duplicate would silently reclassify another
// operation.
func TestA2ASpecOperationIDsAreUnique(t *testing.T) {
	body, status, _ := servedOpenAPISpec(t)
	if status != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d, want %d", status, http.StatusOK)
	}
	spec := parseA2ASpec(t, body)
	owners := map[string][]string{}
	for path, item := range spec.Paths {
		for method, op := range item {
			if op.OperationID == "" {
				continue
			}
			owners[op.OperationID] = append(owners[op.OperationID], strings.ToUpper(method)+" "+path)
		}
	}
	for _, wanted := range []string{"a2aAgentCard", "a2aJSONRPC"} {
		where, ok := owners[wanted]
		if !ok {
			t.Errorf("operationId %q is absent from the served spec", wanted)
			continue
		}
		if len(where) != 1 {
			t.Errorf("operationId %q is declared %d times (%v), want exactly once", wanted, len(where), where)
		}
	}
}
