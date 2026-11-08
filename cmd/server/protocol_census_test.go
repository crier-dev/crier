// protocol_census_test.go — INT-007 (specs/CRIER-PROTOCOLS.md §7).
//
// Two gates live here:
//
//  1. TestProtocolRouteCensusParity — the route-census parity gate (§7.1).
//     For every gate combination of {a2a on/off} × {dagger url set/unset} ×
//     {fed links set/unset} it boots the REAL server and asserts the set of
//     (method, path) pairs it mounts is byte-identical to the committed
//     golden census (cmd/server/testdata/protocol-census.json), taken at
//     pre-refactor HEAD. A flag that flips without the census updating, or a
//     protocol route that moves, fails here. Regenerate with
//     `go test ./cmd/server -run TestProtocolRouteCensusParity -census-update`
//     — and read the diff before committing it: the golden is the contract.
//
//  2. TestProtocolRoutesRegisterOnlyThroughLoop — the registration-
//     provenance gate. Every protocol surface (relay, mesh, a2a, dagger,
//     federation) must be mounted through the ONE registerProtocols loop
//     (cmd/server/protocols.go); an r.HandleFunc/r.Handle call for a protocol
//     path in cmd/server/main.go is the ad-hoc registration the spec abolishes
//     (§1, §3) and fails this test. Core surfaces (registry/inbox, ingress,
//     trust, sessions, status, detection, observability) are NOT protocols
//     (decision D-P1) and are exempt.
//
// Census mechanics: every method-constrained route registers OPTIONS on no
// path, so an OPTIONS probe of a mounted path is answered by the router's 405
// fallback and an unmounted one by the 404 fallback — the handler itself is
// never invoked and the probe has no side effects. The two any-method routes
// (relay subscribe, mesh connect) do reach their handler on an OPTIONS probe;
// neither answers the fallback 404 while mounted, so "404 means absent" holds
// for them too.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var censusUpdate = flag.Bool("census-update", false, "rewrite the protocol route-census golden from a live boot")

// censusPath is one path template the router can mount, with the methods it
// is registered under and the concrete URL an OPTIONS probe lands on. The
// method lists are transcribed from the registrations (main.go and the
// per-surface helpers); the probe verifies per combo whether the path is
// mounted at all. Method sets are pinned exactly by the walk-based census in
// protocols_test.go (TestRegisterProtocolsCensusWalk), which reads them off
// the mux itself.
type censusPath struct {
	methods   []string
	path      string
	probeURL  string
	anyMethod bool
}

// censusPaths is the route universe a boot can mount. Detection (CR_DETECT),
// pprof and the peer-records management surface (CR_FED_RECORDS_FILE) are
// scrubbed in every census combo, so their routes are absent by construction.
var censusPaths = []censusPath{
	// Core (NOT protocols, decision D-P1) — pinned so the golden is the
	// whole-server census R-A1 asks to be byte-identical.
	{methods: []string{"GET"}, path: "/health", probeURL: "/health"},
	{methods: []string{"GET"}, path: "/version", probeURL: "/version"},
	{methods: []string{"GET"}, path: "/openapi.json", probeURL: "/openapi.json"},
	{methods: []string{"GET"}, path: "/openapi.yaml", probeURL: "/openapi.yaml"},
	{methods: []string{"GET"}, path: "/docs", probeURL: "/docs"},
	{methods: []string{"GET"}, path: "/chat", probeURL: "/chat"},
	{methods: []string{"GET"}, path: "/status", probeURL: "/status"},
	{methods: []string{"GET"}, path: "/namespaces", probeURL: "/namespaces"},
	{methods: []string{"GET", "POST"}, path: "/agents", probeURL: "/agents"},
	{methods: []string{"POST", "GET", "DELETE", "PATCH"}, path: "/agents/{id}", probeURL: "/agents/probe-agent"},
	{methods: []string{"POST", "GET"}, path: "/agents/{id}/inbox", probeURL: "/agents/probe-agent/inbox"},
	{methods: []string{"POST"}, path: "/agents/{id}/inbox/ack", probeURL: "/agents/probe-agent/inbox/ack"},
	{methods: []string{"GET"}, path: "/agents/{id}/inbox/stats", probeURL: "/agents/probe-agent/inbox/stats"},
	{methods: []string{"POST"}, path: "/agents/{id}/inbox/transfer", probeURL: "/agents/probe-agent/inbox/transfer"},
	{methods: []string{"GET"}, path: "/agents/{id}/inbox/dead-letters", probeURL: "/agents/probe-agent/inbox/dead-letters"},
	{methods: []string{"POST"}, path: "/capabilities/{capability}/inbox", probeURL: "/capabilities/probe-capability/inbox"},
	{methods: []string{"POST", "GET"}, path: "/trust/anchors", probeURL: "/trust/anchors"},
	{methods: []string{"POST", "GET"}, path: "/trust/delegations", probeURL: "/trust/delegations"},
	{methods: []string{"POST"}, path: "/trust/verify", probeURL: "/trust/verify"},
	{methods: []string{"GET", "POST"}, path: "/principals", probeURL: "/principals"},
	{methods: []string{"GET", "POST"}, path: "/bindings", probeURL: "/bindings"},
	{methods: []string{"GET", "POST"}, path: "/grants", probeURL: "/grants"},
	{methods: []string{"POST"}, path: "/grants/{grantid}/revoke", probeURL: "/grants/probe-grant/revoke"},
	{methods: []string{"GET", "POST"}, path: "/agents/{agentid}/class", probeURL: "/agents/probe-agent/class"},
	{methods: []string{"GET", "POST"}, path: "/sessions", probeURL: "/sessions"},
	{methods: []string{"POST"}, path: "/sessions/{id}/state", probeURL: "/sessions/probe-session/state"},
	{methods: []string{"GET", "POST"}, path: "/sessions/{id}/messages", probeURL: "/sessions/probe-session/messages"},
	{methods: []string{"GET", "POST"}, path: "/sessions/{id}/tasks", probeURL: "/sessions/probe-session/tasks"},
	{methods: []string{"GET"}, path: "/sessions/{id}/tasks/{task_id}", probeURL: "/sessions/probe-session/tasks/probe-task"},
	{methods: []string{"POST"}, path: "/sessions/{id}/tasks/{task_id}/claim", probeURL: "/sessions/probe-session/tasks/probe-task/claim"},
	{methods: []string{"POST"}, path: "/sessions/{id}/tasks/{task_id}/complete", probeURL: "/sessions/probe-session/tasks/probe-task/complete"},
	{methods: []string{"GET", "POST"}, path: "/sessions/{id}/participants", probeURL: "/sessions/probe-session/participants"},
	{methods: []string{"GET", "PUT"}, path: "/sessions/{id}/participants/{member_type}/{member_id}/context", probeURL: "/sessions/probe-session/participants/agent/probe-member/context"},
	{methods: []string{"POST"}, path: "/sessions/{id}/compile", probeURL: "/sessions/probe-session/compile"},
	{methods: []string{"GET"}, path: "/sessions/{id}/messages/{mid}/expand", probeURL: "/sessions/probe-session/messages/probe-msg/expand"},
	{methods: []string{"GET"}, path: "/sessions/{id}/threads/{thread_id}", probeURL: "/sessions/probe-session/threads/probe-thread"},
	{methods: []string{"GET"}, path: "/sessions/{id}/threads/{thread_id}/messages", probeURL: "/sessions/probe-session/threads/probe-thread/messages"},
	{methods: []string{"GET"}, path: "/sessions/{id}/threads/{thread_id}/timeline", probeURL: "/sessions/probe-session/threads/probe-thread/timeline"},
	{methods: []string{"GET"}, path: "/sessions/{id}/threads/{thread_id}/summary", probeURL: "/sessions/probe-session/threads/probe-thread/summary"},
	{methods: []string{"GET"}, path: "/sessions/{id}/output", probeURL: "/sessions/probe-session/output"},
	{methods: []string{"GET"}, path: "/sessions/{id}/search", probeURL: "/sessions/probe-session/search"},
	{methods: []string{"POST", "GET"}, path: "/sessions/{id}/reads", probeURL: "/sessions/probe-session/reads"},
	{methods: []string{"GET"}, path: "/sessions/{id}/reads/unread", probeURL: "/sessions/probe-session/reads/unread"},
	{methods: []string{"GET"}, path: "/tags", probeURL: "/tags"},
	{methods: []string{"GET", "POST"}, path: "/groups", probeURL: "/groups"},
	{methods: []string{"GET"}, path: "/groups/{name}", probeURL: "/groups/probe-group"},
	{methods: []string{"PATCH"}, path: "/groups/{name}/members", probeURL: "/groups/probe-group/members"},
	{methods: []string{"POST"}, path: "/ingress/messages", probeURL: "/ingress/messages"},
	{methods: []string{"GET"}, path: "/metrics", probeURL: "/metrics"},

	// Relay protocol.
	{methods: []string{"POST"}, path: "/relay/publish", probeURL: "/relay/publish"},
	{methods: nil, path: "/relay/subscribe/{topic}", probeURL: "/relay/subscribe/probe-topic", anyMethod: true},
	{methods: []string{"GET"}, path: "/relay/topics", probeURL: "/relay/topics"},

	// Mesh protocol.
	{methods: nil, path: "/mesh/connect/{agentID}", probeURL: "/mesh/connect/probe-agent", anyMethod: true},
	{methods: []string{"GET"}, path: "/mesh/peers", probeURL: "/mesh/peers"},

	// A2A protocol (CR_A2A_ENABLED, default off — A2A-OPTION §1).
	{methods: []string{"GET"}, path: "/.well-known/agent-card.json", probeURL: "/.well-known/agent-card.json"},
	{methods: []string{"POST"}, path: "/a2a", probeURL: "/a2a"},

	// Dagger protocol (CR_DAGGER_URL, default off).
	{methods: []string{"POST"}, path: "/dagger/runs", probeURL: "/dagger/runs"},
	{methods: []string{"GET"}, path: "/dagger/runs/{id}", probeURL: "/dagger/runs/probe-run"},
	{methods: []string{"POST"}, path: "/dagger/runs/{id}/cancel", probeURL: "/dagger/runs/probe-run/cancel"},
	{methods: []string{"POST"}, path: "/dagger/runs/{id}/resume", probeURL: "/dagger/runs/probe-run/resume"},
	{methods: []string{"POST"}, path: "/dagger/runs/{id}/rewind", probeURL: "/dagger/runs/probe-run/rewind"},
	{methods: []string{"POST"}, path: "/dagger/skills/{skill}/run", probeURL: "/dagger/skills/probe-skill/run"},
	{methods: []string{"POST"}, path: "/dagger/wait", probeURL: "/dagger/wait"},
	{methods: []string{"POST"}, path: "/dagger/waits/{key}/resolve", probeURL: "/dagger/waits/probe-key/resolve"},
	{methods: []string{"GET"}, path: "/dagger/waits/{key}", probeURL: "/dagger/waits/probe-key"},

	// Federation protocol. The /fed/peers listing is mounted on every boot
	// where the peer-records management surface (CR_FED_RECORDS_FILE) does
	// not own /fed/*; CR_FED_LINKS changes the handler's client, not the
	// mounted set.
	{methods: []string{"GET"}, path: "/fed/peers", probeURL: "/fed/peers"},
}

// censusCombos is the gate axis of spec §7.1. Each combo's env is merged over
// the deterministic scrub: a "" value pins the variable unset so the process
// env cannot leak into the census.
var censusCombos = []struct {
	name string
	env  map[string]string
}{
	{"a2a=off,dagger=off,fed=off", map[string]string{
		"CR_A2A_ENABLED": "", "CR_DAGGER_URL": "", "CR_FED_LINKS": ""}},
	{"a2a=off,dagger=off,fed=on", map[string]string{
		"CR_A2A_ENABLED": "", "CR_DAGGER_URL": "", "CR_FED_LINKS": "http://127.0.0.1:1"}},
	{"a2a=off,dagger=on,fed=off", map[string]string{
		"CR_A2A_ENABLED": "", "CR_DAGGER_URL": "http://127.0.0.1:1", "CR_FED_LINKS": ""}},
	{"a2a=off,dagger=on,fed=on", map[string]string{
		"CR_A2A_ENABLED": "", "CR_DAGGER_URL": "http://127.0.0.1:1", "CR_FED_LINKS": "http://127.0.0.1:1"}},
	{"a2a=on,dagger=off,fed=off", map[string]string{
		"CR_A2A_ENABLED": "true", "CR_DAGGER_URL": "", "CR_FED_LINKS": ""}},
	{"a2a=on,dagger=off,fed=on", map[string]string{
		"CR_A2A_ENABLED": "true", "CR_DAGGER_URL": "", "CR_FED_LINKS": "http://127.0.0.1:1"}},
	{"a2a=on,dagger=on,fed=off", map[string]string{
		"CR_A2A_ENABLED": "true", "CR_DAGGER_URL": "http://127.0.0.1:1", "CR_FED_LINKS": ""}},
	{"a2a=on,dagger=on,fed=on", map[string]string{
		"CR_A2A_ENABLED": "true", "CR_DAGGER_URL": "http://127.0.0.1:1", "CR_FED_LINKS": "http://127.0.0.1:1"}},
}

// censusBaseEnv pins every config axis the combos do not vary so a census
// boot depends only on the combo's own switches.
var censusBaseEnv = map[string]string{
	"CR_FED_PEERS_FILE":   "",
	"CR_FED_RECORDS_FILE": "",
	"CR_DETECT_ENABLED":   "",
	"CR_ENABLE_PPROF":     "",
	"CR_ENABLE_METRICS":   "true",
	"CR_HARNESS_CONFIG":   "",
	"CR_DAGGER_STORE_DIR": "", // replaced per boot when dagger is on
	"CR_DAGGER_POLL_S":    "0",
}

// bootCensus boots the real server under extra and returns the sorted
// (method, path) pairs it mounted, discovered by OPTIONS probing (see the
// file comment for why that is side-effect free).
func bootCensus(t *testing.T, combo map[string]string) []string {
	t.Helper()

	extra := make(map[string]string, len(censusBaseEnv)+len(combo)+2)
	for k, v := range censusBaseEnv {
		extra[k] = v
	}
	for k, v := range combo {
		extra[k] = v
	}
	if extra["CR_DAGGER_URL"] != "" {
		// A dagger boot needs a usable run-record store directory; its
		// content and reachability are irrelevant to the route census.
		extra["CR_DAGGER_STORE_DIR"] = t.TempDir()
	}

	base := startTestServerWithEnv(t, extra)

	present := make(map[string]bool, len(censusPaths))
	for _, cp := range censusPaths {
		req, err := http.NewRequest(http.MethodOptions, base+cp.probeURL, nil)
		if err != nil {
			t.Fatalf("build OPTIONS probe for %s: %v", cp.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS %s: %v", cp.probeURL, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		switch {
		case cp.anyMethod:
			// The handler runs on any method; while mounted it never
			// answers the router's fallback 404.
			present[cp.path] = resp.StatusCode != http.StatusNotFound
		default:
			// OPTIONS is registered nowhere: a mounted path answers the
			// 405 fallback without the handler ever running.
			present[cp.path] = resp.StatusCode == http.StatusMethodNotAllowed
		}
	}

	var out []string
	for _, cp := range censusPaths {
		if !present[cp.path] {
			continue
		}
		if cp.anyMethod {
			out = append(out, "ANY "+cp.path)
			continue
		}
		for _, m := range cp.methods {
			out = append(out, m+" "+cp.path)
		}
	}
	sort.Strings(out)
	return out
}

func censusGoldenPath() string {
	return filepath.Join("testdata", "protocol-census.json")
}

// TestProtocolRouteCensusParity is the route-census parity gate (§7.1): for
// every gate combination the mounted (method, path) set must be byte-identical
// to the committed golden census taken at pre-refactor HEAD.
func TestProtocolRouteCensusParity(t *testing.T) {
	golden := map[string][]string{}
	if raw, err := os.ReadFile(censusGoldenPath()); err == nil {
		if err := json.Unmarshal(raw, &golden); err != nil {
			t.Fatalf("parse %s: %v", censusGoldenPath(), err)
		}
	} else if !*censusUpdate {
		t.Fatalf("read golden census %s: %v (regenerate with -census-update)", censusGoldenPath(), err)
	}

	for _, combo := range censusCombos {
		combo := combo
		t.Run(combo.name, func(t *testing.T) {
			census := bootCensus(t, combo.env)

			if *censusUpdate {
				golden[combo.name] = census
				return
			}
			want, ok := golden[combo.name]
			if !ok {
				t.Fatalf("golden census %s has no entry for combo %q (regenerate with -census-update)", censusGoldenPath(), combo.name)
			}
			if strings.Join(census, "\n") != strings.Join(want, "\n") {
				t.Errorf("route census for %q drifted from the golden taken at pre-refactor HEAD\n--- golden\n%s\n--- live\n%s",
					combo.name, strings.Join(want, "\n"), strings.Join(census, "\n"))
			}
		})
	}

	if *censusUpdate {
		raw, err := json.MarshalIndent(golden, "", "  ")
		if err != nil {
			t.Fatalf("marshal golden census: %v", err)
		}
		if err := os.WriteFile(censusGoldenPath(), append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write golden census: %v", err)
		}
		t.Logf("wrote golden census %s", censusGoldenPath())
	}
}

// TestA2AOffRegistersZeroA2ARoutes re-affirms the A2A-OPTION §1 two-half gate
// through the census: with CR_A2A_ENABLED unset, NEITHER A2A path is mounted
// (§7.2) — and with it set, both are.
func TestA2AOffRegistersZeroA2ARoutes(t *testing.T) {
	a2aPaths := []string{"/.well-known/agent-card.json", "/a2a"}

	for _, enabled := range []struct {
		on   bool
		env  string
		want bool
	}{
		{env: "", want: false},
		{env: "true", want: true},
	} {
		base := startTestServerWithEnv(t, map[string]string{
			"CR_A2A_ENABLED": enabled.env,
			"CR_DAGGER_URL":  "",
			"CR_FED_LINKS":   "",
		})
		for _, p := range a2aPaths {
			resp, err := http.Get(base + p)
			if err != nil {
				t.Fatalf("GET %s: %v", p, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			mounted := resp.StatusCode != http.StatusNotFound
			if mounted != enabled.want {
				t.Errorf("CR_A2A_ENABLED=%q: GET %s status %d, mounted=%v, want mounted=%v",
					enabled.env, p, resp.StatusCode, mounted, enabled.want)
			}
		}
	}
}

// TestProtocolRoutesRegisterOnlyThroughLoop is the registration-provenance
// gate: NO protocol surface may be mounted by an ad-hoc r.HandleFunc/r.Handle
// call or a direct register* call in cmd/server/main.go (§1: "adding a
// protocol means editing runWithEnv in three places" is the state this
// refactor abolishes). All five surfaces mount through the ONE
// registerProtocols loop in cmd/server/protocols.go. Core surfaces (registry/
// inbox, ingress, trust, sessions, status, detection, observability) are not
// protocols (decision D-P1) and stay direct.
func TestProtocolRoutesRegisterOnlyThroughLoop(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	// Protocol path prefixes an ad-hoc HandleFunc/Handle call would name.
	pathPrefixes := []string{"/relay/", "/mesh/", "/fed/", "/dagger/", "/a2a", "/.well-known/"}
	// Unexported registration helpers an ad-hoc call would reach for. The
	// adapters in protocols.go are the only allowed callers.
	helperCalls := []string{"registerA2ARoute(", "registerA2ACardRoute(", "registerDaggerRoutes("}

	var violations []string
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		loc := ""
		for _, call := range []string{"r.HandleFunc(", "r.Handle("} {
			if idx := strings.Index(trimmed, call); idx >= 0 {
				if q1 := strings.Index(trimmed[idx:], `"`); q1 >= 0 {
					rest := trimmed[idx+q1+1:]
					if q2 := strings.Index(rest, `"`); q2 >= 0 {
						loc = rest[:q2]
					}
				}
				break
			}
		}
		if loc != "" {
			for _, p := range pathPrefixes {
				if strings.HasPrefix(loc, p) {
					violations = append(violations, fmt.Sprintf("main.go:%d: ad-hoc protocol route %q: %s", i+1, loc, trimmed))
				}
			}
		}
		for _, h := range helperCalls {
			if strings.Contains(trimmed, h) {
				violations = append(violations, fmt.Sprintf("main.go:%d: ad-hoc protocol registration via %s — mount through registerProtocols: %s", i+1, strings.TrimSuffix(h, "("), trimmed))
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("protocol surface(s) registered outside the registerProtocols loop (specs/CRIER-PROTOCOLS.md §3, INT-007):\n%s",
			strings.Join(violations, "\n"))
	}
}
