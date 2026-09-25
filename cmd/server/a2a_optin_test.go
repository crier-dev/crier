package main

// INT-A2A-001 — the non-regression gate for the A2A option.
//
// The row's whole point is that the option is an EXTRA: with the switch unset
// (the default, and every existing deployment's posture) and with it on, crier
// must behave exactly as it did before this change. This test boots the REAL
// server (run(nil) — middleware, router and stores included) in both switch
// positions, under both signature postures, and asserts against the contract as
// it stood BEFORE the change:
//
//  1. the same status code on every existing route,
//  2. the same body shape (key set) and, for the deterministic routes, the same
//     body BYTES,
//  3. that an agent registered on that server still serializes with the
//     pre-A2A key set (register response and read-back row),
//  4. that the switch itself is invisible: the two positions produce identical
//     observations,
//  5. that the A2A route surface is exactly what the series has agreed: with the
//     switch OFF every A2A path (specs/A2A-OPTION.md §5.2) answers 404, and with
//     it ON exactly the one path a landed row registered answers anything else —
//     the INT-A2A-002 discovery route, which answers 400 without the agent
//     selector that a bus needs. Those statuses are recorded OUTSIDE the
//     pre-A2A maps, so the cross-position comparison below stays a statement
//     about the surface that existed before the A2A option.
//
// Nothing here is derived from the change under test: the expected statuses,
// bodies and key sets are pinned from the pre-A2A routes and the documented
// GET /status schema, and the A2A surface expectations are pinned from the
// spec's route table.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
)

// preA2AAgentKeys is the key set of a registry row with no optional config —
// the wire shape that existed before the A2A option. Pinned literally (SORTED:
// the comparison sorts both sides), never re-derived from the code under test.
var preA2AAgentKeys = []string{
	"capabilities", "id", "last_seen", "public_key", "registered_at", "status",
}

// inboxRetrieveKeys is the documented body of GET /agents/{id}/inbox.
var inboxRetrieveKeys = []string{"lease_id", "leased_count", "messages", "queue_depth"}

// a2aPaths are the paths the A2A series names (specs/A2A-OPTION.md §5.2). Only
// the discovery path is registered by a landed row (INT-A2A-002, and only while
// the switch is on); the JSON-RPC binding paths stay unregistered until
// INT-A2A-003, so each is probed in both switch positions below.
var a2aPaths = []string{
	a2a.AgentCardPath,
	"/a2a",
	"/a2a/rpc",
}

// surfaceObservation is one booted server's contract: the status of every
// probed route, the RAW body of the deterministic ones, the decoded key set of
// the ones that carry timestamps or generated ids, and — kept separate on
// purpose — the status of the A2A paths the option governs, which are compared
// only against the spec's route table and never against the pre-A2A surface.
type surfaceObservation struct {
	status map[string]int
	body   map[string]string
	keys   map[string][]string
	a2a    map[string]int
}

// probeSurface boots the server with the given environment additions, exercises
// the existing surface, and returns what it observed. sigRequired says whether
// the boot runs with per-agent signature enforcement on (the secure default),
// which decides what the agent-scoped routes answer; a2aEnabled says whether
// this boot carries the A2A switch.
func probeSurface(t *testing.T, env map[string]string, sigRequired, a2aEnabled bool) surfaceObservation {
	t.Helper()
	base := startTestServerWithEnv(t, env)
	client := &http.Client{Timeout: 5 * time.Second}

	obs := surfaceObservation{
		status: map[string]int{},
		body:   map[string]string{},
		keys:   map[string][]string{},
		a2a:    map[string]int{},
	}

	do := func(method, path, payload string) (int, string) {
		t.Helper()
		var reader io.Reader
		if payload != "" {
			reader = strings.NewReader(payload)
		}
		req, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			t.Fatalf("%s %s: build request: %v", method, path, err)
		}
		if payload != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s %s: read body: %v", method, path, err)
		}
		obs.status[method+" "+path] = resp.StatusCode
		return resp.StatusCode, string(raw)
	}

	// --- read routes, deterministic bodies -----------------------------------
	// Bodies are recorded ONLY where the response carries no generated value:
	// the cross-position comparison below then compares them byte for byte.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/version"},
		{http.MethodGet, "/status"},
		{http.MethodGet, "/agents"},
		{http.MethodGet, "/agents/does-not-exist"},
		{http.MethodGet, "/relay/topics"},
		{http.MethodGet, "/mesh/peers"},
	} {
		if _, body := do(probe.method, probe.path, ""); body == "" {
			t.Errorf("%s %s answered an empty body", probe.method, probe.path)
		} else {
			obs.body[probe.method+" "+probe.path] = body
		}
	}
	obs.keys["GET /version"] = sortedKeys(t, obs.body["GET /version"])
	obs.keys["GET /agents"] = sortedKeys(t, obs.body["GET /agents"])

	// --- the A2A route surface (specs/A2A-OPTION.md §5.2/§5.3) ----------------
	// Probed for every boot and recorded OUTSIDE the pre-A2A maps above: the
	// cross-position comparison in the test below must stay a statement about
	// the surface that existed before the A2A option, and these paths are the
	// option's own.
	for _, path := range a2aPaths {
		code, _ := do(http.MethodGet, path, "")
		// do() records every probe it makes; this one is deliberately NOT part
		// of the pre-A2A surface, so its entry is removed immediately — the
		// cross-position comparison below has to stay a statement about the
		// routes that existed before the A2A option.
		delete(obs.status, http.MethodGet+" "+path)
		obs.a2a[path] = code
	}

	// --- register an agent and re-read it ------------------------------------
	// These bodies carry a generated keypair and timestamps, so they are
	// compared by SHAPE (below) rather than by bytes.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	registerBody := fmt.Sprintf(`{"id":"agent-a","public_key":%q,"capabilities":["relay"]}`, hex.EncodeToString(pub))

	code, body := do(http.MethodPost, "/agents", registerBody)
	if code != http.StatusCreated {
		t.Fatalf("POST /agents = %d: %s; want 201", code, body)
	}
	obs.keys["POST /agents"] = sortedKeys(t, body)

	code, body = do(http.MethodGet, "/agents/agent-a", "")
	if code != http.StatusOK {
		t.Fatalf("GET /agents/agent-a = %d: %s; want 200", code, body)
	}
	obs.keys["GET /agents/agent-a"] = sortedKeys(t, body)

	code, body = do(http.MethodGet, "/agents", "")
	if code != http.StatusOK {
		t.Fatalf("GET /agents (populated) = %d: %s; want 200", code, body)
	}
	obs.keys["GET /agents (populated)"] = agentsEntryKeys(t, body)

	// Agent-scoped routes: with enforcement on they need the signature headers
	// (unchanged by this row); with it off the read path answers, and its body
	// shape is part of the contract.
	inboxKey := "GET /agents/agent-a/inbox"
	if sigRequired {
		code, body := do(http.MethodGet, "/agents/agent-a/inbox", "")
		obs.body[inboxKey] = body
		if code != http.StatusUnauthorized {
			t.Fatalf("%s = %d: %s; want 401 (signature enforcement is on)", inboxKey, code, body)
		}
	} else {
		code, body := do(http.MethodGet, "/agents/agent-a/inbox", "")
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s; want 200", inboxKey, code, body)
		}
		obs.body[inboxKey] = body
		obs.keys[inboxKey] = sortedKeys(t, body)
	}
	code, body = do(http.MethodGet, "/agents/agent-a/inbox/stats", "")
	if sigRequired && code != http.StatusUnauthorized {
		t.Fatalf("GET /agents/agent-a/inbox/stats = %d: %s; want 401 (signature enforcement is on)", code, body)
	}

	return obs
}

// errorOfBody returns the {"error": …} string of a JSON error response.
func errorOfBody(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return out.Error
}

// sortedKeys returns the sorted top-level key set of a JSON object body.
func sortedKeys(t *testing.T, body string) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// agentsEntryKeys returns the sorted key set of agents[0] in a GET /agents body.
func agentsEntryKeys(t *testing.T, body string) []string {
	t.Helper()
	var list struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(list.Agents) != 1 {
		t.Fatalf("GET /agents returned %d agents, want 1: %s", len(list.Agents), body)
	}
	keys := make([]string, 0, len(list.Agents[0]))
	for k := range list.Agents[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestA2AOption_ExistingSurfaceUnchanged is the gate. Each posture group boots
// the server once per switch position, pins the pre-A2A contract on each boot,
// and then asserts the two positions are byte-for-byte indistinguishable.
func TestA2AOption_ExistingSurfaceUnchanged(t *testing.T) {
	groups := []struct {
		name        string
		baseEnv     map[string]string
		sigRequired bool
	}{
		// The default posture: signature enforcement ON (CR_REQUIRE_AGENT_SIG
		// defaults to true), which is what every existing deployment runs.
		{name: "signatures required (the default)", baseEnv: nil, sigRequired: true},
		// The documented dev shortcut, so the signed read path's BODY is part
		// of the compared contract too.
		{name: "signatures off (CR_REQUIRE_AGENT_SIG=false)", baseEnv: map[string]string{"CR_REQUIRE_AGENT_SIG": "false"}, sigRequired: false},
	}

	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			observed := map[string]surfaceObservation{}
			for _, tc := range []struct {
				name     string
				envValue string
			}{
				{"switch unset (default off)", ""},
				{"switch on (CR_A2A_ENABLED=true)", "true"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					env := map[string]string{"CR_A2A_ENABLED": tc.envValue}
					for k, v := range g.baseEnv {
						env[k] = v
					}
					obs := probeSurface(t, env, g.sigRequired, tc.envValue == "true")
					assertPreA2AContract(t, obs, g.sigRequired)
					assertA2ARouteSurface(t, obs, tc.envValue == "true")
					observed[tc.name] = obs
				})
			}

			// The switch must be INVISIBLE on the surface that existed BEFORE
			// it: the two positions answer identically, on every pre-A2A
			// route, in every shape. (The A2A paths themselves are compared
			// against the spec's route table by assertA2ARouteSurface above —
			// they are the option's own surface, and the point of this
			// assertion is everything else.)
			off := observed["switch unset (default off)"]
			on := observed["switch on (CR_A2A_ENABLED=true)"]
			if !reflect.DeepEqual(off.status, on.status) {
				t.Errorf("status codes differ between the switch positions:\noff = %v\n on = %v", off.status, on.status)
			}
			if !reflect.DeepEqual(off.body, on.body) {
				for route, body := range off.body {
					if on.body[route] != body {
						t.Errorf("route %s answered differently with the A2A switch on:\noff = %s\n on = %s", route, body, on.body[route])
					}
				}
			}
			if !reflect.DeepEqual(off.keys, on.keys) {
				t.Errorf("body shapes differ between the switch positions:\noff = %v\n on = %v", off.keys, on.keys)
			}
		})
	}
}

// assertA2ARouteSurface pins the A2A paths to the route table the series has
// agreed (specs/A2A-OPTION.md §5.2), which is the one part of this surface the
// switch is SUPPOSED to move:
//
//   - with the switch off, every A2A path is unregistered — a plain 404, the
//     same answer an unregistered path has always given;
//   - with the switch on, exactly the path a landed row registered exists: the
//     INT-A2A-002 discovery route, which answers 400 to a request that names no
//     agent (this bus hosts many agents on one origin, so the path alone cannot
//     name one — 400 is how "registered, malformed request" differs from
//     "not registered"). Every other A2A path still answers 404.
func assertA2ARouteSurface(t *testing.T, obs surfaceObservation, a2aEnabled bool) {
	t.Helper()
	position := "off"
	if a2aEnabled {
		position = "on"
	}
	for _, path := range a2aPaths {
		want := http.StatusNotFound
		if a2aEnabled && path == a2a.AgentCardPath {
			want = http.StatusBadRequest
		}
		if got := obs.a2a[path]; got != want {
			t.Errorf("GET %s = %d with the A2A switch %s, want %d (specs/A2A-OPTION.md §5.2)", path, got, position, want)
		}
	}
}

// assertPreA2AContract pins one boot's observation to the contract that existed
// BEFORE the A2A option — statuses, bodies and key sets taken from the shipped
// routes, not from anything this change introduced.
func assertPreA2AContract(t *testing.T, obs surfaceObservation, sigRequired bool) {
	t.Helper()

	// Status codes: the documented answer of every probed route.
	signedAgentRoutes := http.StatusUnauthorized
	if !sigRequired {
		signedAgentRoutes = http.StatusOK
	}
	wantStatus := map[string]int{
		"GET /health":                     http.StatusOK,
		"GET /version":                    http.StatusOK,
		"GET /status":                     http.StatusOK,
		"GET /agents":                     http.StatusOK,
		"GET /agents/does-not-exist":      http.StatusNotFound,
		"GET /relay/topics":               http.StatusOK,
		"GET /mesh/peers":                 http.StatusOK,
		"POST /agents":                    http.StatusCreated,
		"GET /agents/agent-a":             http.StatusOK,
		"GET /agents/agent-a/inbox":       signedAgentRoutes,
		"GET /agents/agent-a/inbox/stats": signedAgentRoutes,
	}
	for route, want := range wantStatus {
		if got := obs.status[route]; got != want {
			t.Errorf("%s = %d, want %d", route, got, want)
		}
	}

	// Bodies: byte-pinned where the response carries no generated value —
	// including the auth refusal, which must be the same requirement as before.
	if got := obs.body["GET /health"]; got != `{"status":"ok"}` {
		t.Errorf("GET /health body = %q, want %q", got, `{"status":"ok"}`)
	}
	if got, want := errorOfBody(t, obs.body["GET /agents/does-not-exist"]),
		`agent not found: "does-not-exist"`; got != want {
		t.Errorf("GET /agents/{id} 404 error = %q, want %q", got, want)
	}
	if sigRequired {
		if got, want := errorOfBody(t, obs.body["GET /agents/agent-a/inbox"]),
			"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"; got != want {
			t.Errorf("GET /agents/{id}/inbox 401 error = %q, want %q", got, want)
		}
	}

	// Shapes: the pinned key sets.
	if got, want := obs.keys["GET /version"], []string{"build_time", "commit", "modified", "version"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GET /version keys = %v, want %v", got, want)
	}

	statusBody := decodeStatus(t, []byte(obs.body["GET /status"]))
	statusKeys := make([]string, 0, len(statusBody))
	for k := range statusBody {
		statusKeys = append(statusKeys, k)
	}
	sort.Strings(statusKeys)
	wantStatusKeys := append([]string(nil), statusTopLevelKeys...)
	sort.Strings(wantStatusKeys)
	if !reflect.DeepEqual(statusKeys, wantStatusKeys) {
		t.Errorf("GET /status keys = %v, want %v (the A2A switch adds no key here)", statusKeys, wantStatusKeys)
	}
	assertStatusBuild(t, statusBody)

	if got, want := obs.keys["GET /agents"], []string{"agents"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GET /agents keys = %v, want %v", got, want)
	}
	if got, want := obs.keys["GET /agents (populated)"], preA2AAgentKeys; !reflect.DeepEqual(got, want) {
		t.Errorf("agents[0] keys = %v, want %v (the pre-A2A registry row)", got, want)
	}
	if !sigRequired {
		if got, want := obs.keys["GET /agents/agent-a/inbox"], inboxRetrieveKeys; !reflect.DeepEqual(got, want) {
			t.Errorf("GET /agents/{id}/inbox keys = %v, want %v", got, want)
		}
	}
	// The registration response and the read-back row carry the pre-A2A key set
	// — no new member appeared for an agent that carries no optional config.
	for _, route := range []string{"POST /agents", "GET /agents/agent-a"} {
		if got, want := obs.keys[route], preA2AAgentKeys; !reflect.DeepEqual(got, want) {
			t.Errorf("%s agent keys = %v, want %v", route, got, want)
		}
	}
}
