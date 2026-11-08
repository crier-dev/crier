// protocols_test.go — INT-007 (specs/CRIER-PROTOCOLS.md §7 tests 3, 4, 5 and
// the walk-based protocol census). The probe-based whole-server census and
// the registration-provenance gate live in protocol_census_test.go.
//
// These tests swap entries in protocolConstructors to inject protocols the
// closed set would never serve (a bogus name, a failing adapter, a throwaway
// one), so they mutate package state and must NOT run in parallel.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/protocol"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
)

// bogusProtocol carries a name outside the closed set.
type bogusProtocol struct{ name string }

func (p bogusProtocol) Name() string  { return p.name }
func (p bogusProtocol) Enabled() bool { return true }
func (p bogusProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	r.HandleFunc("/bogus", func(w http.ResponseWriter, _ *http.Request) {}).Methods("GET")
	return nil
}

// failingProtocol mounts nothing and fails registration.
type failingProtocol struct{ err error }

func (p failingProtocol) Name() string  { return "relay" }
func (p failingProtocol) Enabled() bool { return true }
func (p failingProtocol) RegisterRoutes(*mux.Router, protocol.Deps) error {
	return p.err
}

// throwawayProtocol is spec §7 test 5's proof case: a NEW protocol wired with
// zero edits outside this file — a name in protocol.KnownProtocols and a
// constructor entry are its whole integration.
type throwawayProtocol struct{ mounted *bool }

func (p throwawayProtocol) Name() string  { return "throwaway" }
func (p throwawayProtocol) Enabled() bool { return true }
func (p throwawayProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	r.HandleFunc("/throwaway", func(w http.ResponseWriter, _ *http.Request) {}).Methods("GET")
	*p.mounted = true
	return nil
}

// swapConstructor replaces one constructor entry for the duration of the test.
func swapConstructor(t *testing.T, name string, build func(protocol.Deps) protocol.Protocol) {
	t.Helper()
	orig, had := protocolConstructors[name]
	protocolConstructors[name] = build
	t.Cleanup(func() {
		if had {
			protocolConstructors[name] = orig
		} else {
			delete(protocolConstructors, name)
		}
	})
}

// testDeps builds wiring values sufficient to register every real adapter:
// relay/mesh services are real, A2A and dagger gates are off, federation is
// in its default posture (records mode off, no links — the listing still
// mounts, exactly as a bare boot does).
func testDeps(t *testing.T) protocol.Deps {
	t.Helper()
	return protocol.Deps{
		RegistryHandler: registry.NewHandler(registry.NewMemoryStore()),
		Store:           registry.NewMemoryStore(),
		Relay:           relay.New(0),
		Config:          &config.Config{},
		Mesh:            mesh.NewMesh(mesh.DefaultMeshConfig("crier")),
	}
}

// TestBogusProtocolRefusedAtLoad is spec §7 test 3: a protocol named outside
// the closed KnownProtocols set is a load-time hard failure — registerProtocols
// returns the error (runWithEnv exits 1 on it) and nothing is mounted.
func TestBogusProtocolRefusedAtLoad(t *testing.T) {
	swapConstructor(t, "relay", func(d protocol.Deps) protocol.Protocol {
		return bogusProtocol{name: "bogus"}
	})

	r := mux.NewRouter()
	err := registerProtocols(r, testDeps(t), nil)
	if err == nil {
		t.Fatal("a protocol named outside KnownProtocols must fail the load (spec §5)")
	}
	if !strings.Contains(err.Error(), "unknown protocol") || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("error must name the unknown protocol, got: %v", err)
	}
	// Load-time hard failure: the partial mux is never served — the loop
	// stops at validation, so not even the enabled adapters that come after
	// the bogus entry mounted anything.
	assertNoRoutes(t, r)
}

// TestProtocolRegistrationErrorBlocksStart is spec §7 test 4: an adapter whose
// RegisterRoutes returns an error prevents server start — registerProtocols
// returns the wrapped error and runWithEnv exits 1.
func TestProtocolRegistrationErrorBlocksStart(t *testing.T) {
	sentinel := errors.New("boom: the executor is a lie")
	swapConstructor(t, "relay", func(d protocol.Deps) protocol.Protocol {
		return failingProtocol{err: sentinel}
	})

	r := mux.NewRouter()
	err := registerProtocols(r, testDeps(t), nil)
	if err == nil {
		t.Fatal("a failing RegisterRoutes must block the start (spec §5)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error must wrap the adapter's own failure for errors.Is, got: %v", err)
	}
	if !strings.Contains(err.Error(), "protocol relay:") {
		t.Errorf("error must name the protocol, got: %v", err)
	}
}

// TestThrowawayProtocolNeedsNoMainEdit is spec §7 test 5: a NEW protocol
// integrates by adding its name to protocol.KnownProtocols and one constructor
// entry — buildProtocols and cmd/server/main.go need no edit. The constructor
// entry alone is demonstrably sufficient to mount the surface.
func TestThrowawayProtocolNeedsNoMainEdit(t *testing.T) {
	mounted := false
	swapConstructor(t, "throwaway", func(d protocol.Deps) protocol.Protocol {
		return throwawayProtocol{mounted: &mounted}
	})

	r := mux.NewRouter()
	// First: the loop refuses the set while the name is unknown — the closed
	// set holds even for new protocols.
	if err := registerProtocols(r, testDeps(t), nil); err == nil {
		t.Fatal("an unknown throwaway name must be refused before its constructor is honored")
	}
	assertNoRoutes(t, r)

	// Adding the name (the other half of the two-line integration) admits it.
	orig := protocol.KnownProtocols
	protocol.KnownProtocols = append(append([]string{}, orig...), "throwaway")
	t.Cleanup(func() { protocol.KnownProtocols = orig })

	r = mux.NewRouter()
	if err := registerProtocols(r, testDeps(t), nil); err != nil {
		t.Fatalf("throwaway protocol must register with no main.go edit: %v", err)
	}
	if !mounted {
		t.Fatal("throwaway protocol's RegisterRoutes never ran")
	}
	req := httptest.NewRequest(http.MethodGet, "/throwaway", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /throwaway status %d, want 200 — the throwaway protocol must be served", w.Code)
	}
}

// TestRegisterProtocolsCensusWalk is the walk half of the census gate (§7.1):
// it builds the closed set through the REAL loop for every gate combination
// and walks the resulting mux, asserting the mounted (method, path) set —
// with exact methods, read off the routes themselves — equals the protocol
// slice of the committed golden census taken at pre-refactor HEAD.
func TestRegisterProtocolsCensusWalk(t *testing.T) {
	raw, err := os.ReadFile(censusGoldenPath())
	if err != nil {
		t.Fatalf("read golden census %s: %v", censusGoldenPath(), err)
	}
	golden := map[string][]string{}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden census: %v", err)
	}

	for _, combo := range censusCombos {
		combo := combo
		t.Run(combo.name, func(t *testing.T) {
			deps := testDeps(t)
			deps.Config.A2AEnabled = strings.Contains(combo.name, "a2a=on")
			if strings.Contains(combo.name, "fed=on") {
				deps.Config.Federation.Links = []string{"http://127.0.0.1:1"}
				deps.FedClient = federation.NewClient(deps.Config.Federation.Links, 0, "")
			}
			if strings.Contains(combo.name, "dagger=on") {
				deps.Config.Dagger.URL = "http://127.0.0.1:1"
				svc, err := buildDaggerService(config.DaggerConfig{
					URL:      deps.Config.Dagger.URL,
					StoreDir: t.TempDir(),
				}, registry.NewHandler(deps.Store))
				if err != nil {
					t.Fatalf("build dagger service: %v", err)
				}
				deps.Dagger = svc
			}
			deps.LocalPeer = func() federation.Peer { return federation.Peer{} }

			r := mux.NewRouter()
			if err := registerProtocols(r, deps, nil); err != nil {
				t.Fatalf("registerProtocols: %v", err)
			}

			got, err := routeWalkCensus(r)
			if err != nil {
				t.Fatalf("walk mux: %v", err)
			}
			// The golden is the whole-server census; the loop's walk covers
			// the protocol surfaces only.
			var want []string
			for _, entry := range golden[combo.name] {
				if path := strings.SplitN(entry, " ", 2)[1]; isProtocolPath(path) {
					want = append(want, entry)
				}
			}
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("protocol-layer census for %q drifted from the golden taken at pre-refactor HEAD\n--- golden\n%s\n--- walked\n%s",
					combo.name, strings.Join(want, "\n"), strings.Join(got, "\n"))
			}
		})
	}
}

// isProtocolPath reports path belongs to one of the five protocol surfaces.
func isProtocolPath(path string) bool {
	for _, p := range []string{"/relay/", "/mesh/", "/fed/", "/dagger/", "/a2a", "/.well-known/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// TestRegisterProtocolsOrderIsKnownProtocolsOrder pins §4: the loop registers
// in KnownProtocols slice order.
func TestRegisterProtocolsOrderIsKnownProtocolsOrder(t *testing.T) {
	protos, err := buildProtocols(testDeps(t), nil)
	if err != nil {
		t.Fatalf("buildProtocols: %v", err)
	}
	if len(protos) != len(protocol.KnownProtocols) {
		t.Fatalf("built %d protocols, want %d", len(protos), len(protocol.KnownProtocols))
	}
	for i, name := range protocol.KnownProtocols {
		if protos[i].Name() != name {
			t.Errorf("position %d is %q, want %q (KnownProtocols order must be preserved, §4)", i, protos[i].Name(), name)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func assertNoRoutes(t *testing.T, r *mux.Router) {
	t.Helper()
	got, err := routeWalkCensus(r)
	if err != nil {
		t.Fatalf("walk mux: %v", err)
	}
	if len(got) > 0 {
		t.Errorf("a refused load must mount nothing (spec §5: a partial mux is never served), got: %v", got)
	}
}

// routeWalkCensus walks r and returns the sorted "METHOD path" pairs, "ANY"
// for a route registered without a Methods constraint.
func routeWalkCensus(r *mux.Router) ([]string, error) {
	var out []string
	err := r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, err := route.GetPathTemplate()
		if err != nil {
			return err
		}
		methods, err := route.GetMethods()
		if err != nil || len(methods) == 0 {
			out = append(out, "ANY "+tpl)
			return nil
		}
		for _, m := range methods {
			out = append(out, m+" "+tpl)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
