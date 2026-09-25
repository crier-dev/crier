package mcp

// Spec/tool coverage guard for the crier-mcp surface (INT-MUSTER-006).
//
// DESIGN DECISION (ratified by the foreman, tick 398): the spec operations with
// no bridge tool are DELIBERATE EXCLUSIONS, not drift. crier-mcp is the CURATED
// agent-messaging surface — registry + inboxes + mesh peers, plus the composite
// bridge verbs (send_message / get_messages / ask_agent / mesh_request) that own
// the transport detail a harness should not see. Raw relay topics, federation
// peers, server operational endpoints (/health, /version, /status) and the
// agent-owned self-configuration PATCH are REST-only by that decision; the
// exclusion set below is where that decision is written down and enforced, and
// the one-line justification per entry is the reason a future reader needs.
//
// The relationship is MANY-TO-MANY, not one tool per operation: 13 tools
// exercise 10 covered spec operations, 8 operations are explicitly excluded, and
// several tools share an operation (deliver_message and send_message both
// deliver to the durable inbox; retrieve_inbox and get_messages both retrieve it;
// get_messages and ack_messages both ack it). This guard therefore checks the
// ACCOUNTING in all four drift directions rather than equality of names.
//
// STRICT EQUALITY (every tool name == an operationId) was the premise the first
// attempt at this row started from, and it was measured RED at HEAD — the tool
// vocabulary and the REST vocabulary do not overlap by design. It was rejected:
// renaming 13 tools, or adding 8 operationIds to the spec, is new runtime
// behavior out of this row's scope. What replaces it is the accountability
// below: a tool cannot be added, removed or renamed, and a spec operation cannot
// be added, renamed or removed, without this table being updated in the same
// commit — and every exclusion carries its reason.

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
	"gopkg.in/yaml.v3"
)

// toolCoverage maps each MCP tool name to the docs/openapi.yaml operationId(s)
// its handler actually exercises, verified against the bridge code path — via
// the store abstraction for registry/inbox tools (RemoteStore maps it to the
// matching REST call) and via an explicit HTTP/WebSocket client for the mesh
// ones. A tool with no entry here is drift: assertion (a) names it.
//
// mesh_request is the one entry mapped by TRANSPORT rather than a one-to-one
// REST call, and that is recorded deliberately: the tool does not itself call
// GET /mesh/connect/{agentID} — it carries application-level REQUEST/RESPONSE
// frames (target/method/path/body) over the peer WebSocket connection the bridge
// opens on CRIER_MESH_URL, and the server route that operation describes is
// exactly the one being dialed (cmd/server/main.go registers
// /mesh/connect/{agentID}; the bridge dials its configured URL verbatim). The
// spec documents that operation only as the 101 upgrade, so this is the closest
// honest mapping; excluding it instead would claim the live mesh lane has no MCP
// surface at all, which is false. What is NOT claimed here is that the tool
// exposes the raw connect handshake.
var toolCoverage = map[string][]string{
	"register_agent":   {"registryRegisterAgent"},
	"list_agents":      {"registryListAgents"},
	"get_agent":        {"registryGetAgent"},
	"unregister_agent": {"registryUnregisterAgent"},
	"deliver_message":  {"inboxDeliver"},
	"send_message":     {"inboxDeliver"},
	// The brief for this row mapped ask_agent to inboxDeliver+inboxRetrieve;
	// the code disagrees and the code wins: handleAskAgent delivers the
	// question, polls its own inbox (store.Retrieve) AND acks every batch it
	// reads before matching the reply (messaging.go, best-effort Ack). Three
	// operations, not two.
	"ask_agent":      {"inboxDeliver", "inboxRetrieve", "inboxAck"},
	"retrieve_inbox": {"inboxRetrieve"},
	"get_messages":   {"inboxRetrieve", "inboxAck"},
	"ack_messages":   {"inboxAck"},
	"inbox_stats":    {"inboxStats"},
	"mesh_peers":     {"meshListPeers"},
	"mesh_request":   {"meshConnect"},
}

// excludedOperations is the ratified exclusion set: every spec operation crier-mcp
// deliberately does NOT expose as a tool, with the reason. Assertion (d) requires
// an operation to appear either as a toolCoverage value or here, so a NEW spec
// operation fails this guard until someone classifies it — which is the point.
//
// Each justification is grounded in the repo (the operation's own summary/tag
// description in docs/openapi.yaml, or the bridge's own comments), never invented.
var excludedOperations = map[string]string{
	"healthCheck":         "GET /health — server liveness probe (spec tag \"health\": \"Server liveness, build identity and effective runtime posture\"); an operator/load-balancer surface, not an agent-messaging verb.",
	"buildVersion":        "GET /version — \"Build identity of the running server\" (same identity `crier -version` prints and the startup log carries); an operator/CLI concern, not an agent-messaging verb.",
	"runtimeStatus":       "GET /status — \"Effective runtime posture (auth, guard, registry backend, build identity)\"; operator-only and deliberately NOT auth-exempt, so it is not something a bridge identity should fetch on a tool call.",
	"relayPublish":        "POST /relay/publish — \"Publish an event to a topic\": fire-and-forget topic fan-out (a publish to an empty topic is dropped by design), addressed to subscribers rather than to an agent's durable inbox.",
	"relaySubscribe":      "GET /relay/subscribe/{topic} — \"Subscribe to a topic or wildcard pattern via WebSocket\": a streaming client surface; the bridge already owns its WebSocket transport and exposes the content lane through the inbox tools.",
	"relayListTopics":     "GET /relay/topics — \"List active topics\" with subscriber counts; relay operational census, not an agent-messaging verb.",
	"fedListPeers":        "GET /fed/peers — \"List federation peers (local relay first, then linked relays with their agents)\" (CR-FEAT-006); cross-relay topology for operators, and each link is fetched live from that relay's own GET /agents.",
	"registryUpdateAgent": "PATCH /agents/{id} — \"Partially update an agent's registration (self-configuration directive, spec §7)\", requiring the per-agent ed25519 signature headers; the crier-mcp bridge has no tool for it yet, so it is uncovered — not an agent-messaging verb; reclassify when the bridge grows a tool for it.",
	// CR-FEAT-025 — the ownership surfaces. Both are operator/holder verbs on a
	// specific inbox, and neither is reachable through the bridge's store today:
	// crier-mcp runs on a RemoteStore, which implements neither the Transferrer
	// nor the DeadLetterStore capability (the relay's OWN handler is what answers
	// them), so a tool would have nothing to call. That is why they are
	// EXCLUDED rather than mapped — and why the exclusion names the missing
	// capability instead of merely saying \"no tool yet\": reclassify these when
	// RemoteStore grows the capability and the bridge grows the tool.
	"inboxTransfer":    "POST /agents/{id}/inbox/transfer — \"Transfer (reassign) messages from one inbox to another\": an operator rebalance of a stuck lease, signed by the current HOLDER whose inbox is being drained; RemoteStore does not implement Transferrer, so no bridge tool could execute it.",
	"inboxDeadLetters": "GET /agents/{id}/inbox/dead-letters — \"List messages that expired unacknowledged in this inbox\": a forensics/retrieval surface for the agent whose consumer stopped consuming (and for an operator holding the relay token); RemoteStore does not implement DeadLetterStore, so no bridge tool could execute it.",
}

// specRef is one spec operation's REST coordinates, for actionable messages.
type specRef struct {
	method string
	path   string
}

// readSpecOperations parses docs/openapi.yaml and returns operationId → REST
// coordinates. A path operation with no operationId is itself a failure: the
// guard cannot classify what the spec does not name.
func readSpecOperations(t *testing.T) map[string]specRef {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	ops := map[string]specRef{}
	for path, item := range doc.Paths {
		for method, op := range item {
			switch method {
			case "get", "post", "put", "delete", "patch":
			default:
				continue // path-level parameters, servers, summary, …
			}
			if op.OperationID == "" {
				t.Errorf("%s %s: operation has no operationId — the coverage guard cannot classify an unnamed operation", strings.ToUpper(method), path)
				continue
			}
			ops[op.OperationID] = specRef{method: strings.ToUpper(method), path: path}
		}
	}
	if len(ops) == 0 {
		t.Fatal("parsed 0 operations from docs/openapi.yaml — refusing a vacuous pass")
	}
	return ops
}

// TestSpecToolDrift holds the crier-mcp tool surface and docs/openapi.yaml
// accountable to each other in all four drift directions:
//
//	(a) a tool advertised by toolDefinitions() with no toolCoverage entry
//	(b) a toolCoverage entry naming a tool the server does not advertise
//	(c) a toolCoverage value naming an operationId the spec no longer has
//	(d) a spec operation neither covered by some tool nor explicitly excluded
func TestSpecToolDrift(t *testing.T) {
	ops := readSpecOperations(t)

	tools := map[string]bool{}
	for _, def := range New(registry.NewMemoryStore()).toolDefinitions() {
		tools[def.Name] = true
	}

	covered := map[string]bool{}
	for _, ids := range toolCoverage {
		for _, id := range ids {
			covered[id] = true
		}
	}

	var untrackedTools, ghostTools, staleRefs, unclassified []string

	// (a) every advertised tool is accounted for by the table.
	for name := range tools {
		if _, ok := toolCoverage[name]; !ok {
			untrackedTools = append(untrackedTools, name)
		}
	}
	// (b) every table entry names a tool that is actually advertised.
	for name := range toolCoverage {
		if !tools[name] {
			ghostTools = append(ghostTools, name)
		}
	}
	// (c) every operationId the table references still exists in the spec.
	for name, ids := range toolCoverage {
		for _, id := range ids {
			if _, ok := ops[id]; !ok {
				staleRefs = append(staleRefs, name+" -> "+id)
			}
		}
	}
	// (d) every spec operation is classified: covered by a tool, or excluded
	// with a reason.
	for id := range ops {
		if covered[id] {
			continue
		}
		if _, ok := excludedOperations[id]; !ok {
			unclassified = append(unclassified, id+" ("+ops[id].method+" "+ops[id].path+")")
		}
	}

	sort.Strings(untrackedTools)
	sort.Strings(ghostTools)
	sort.Strings(staleRefs)
	sort.Strings(unclassified)

	if len(untrackedTools) > 0 {
		t.Errorf("crier-mcp advertises tool(s) with no toolCoverage entry: [%s] — map each to the spec operationId(s) it exercises", strings.Join(untrackedTools, " "))
	}
	if len(ghostTools) > 0 {
		t.Errorf("toolCoverage names tool(s) crier-mcp does not advertise: [%s] — the tool was removed or renamed; update the table", strings.Join(ghostTools, " "))
	}
	if len(staleRefs) > 0 {
		t.Errorf("toolCoverage references operationId(s) absent from docs/openapi.yaml: [%s] — the operation was renamed or removed; update the table", strings.Join(staleRefs, " "))
	}
	if len(unclassified) > 0 {
		t.Errorf("docs/openapi.yaml operation(s) neither covered by a tool nor in excludedOperations: [%s] — classify each (add a toolCoverage value, or an excludedOperations entry with its reason)", strings.Join(unclassified, " "))
	}
}
