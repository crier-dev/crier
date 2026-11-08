package protocol

import (
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
)

// fakeProtocol is a minimal Protocol for the closed-set checks.
type fakeProtocol struct{ name string }

func (f fakeProtocol) Name() string                           { return f.name }
func (f fakeProtocol) Enabled() bool                          { return false }
func (f fakeProtocol) RegisterRoutes(*mux.Router, Deps) error { return nil }

func knownSet() []Protocol {
	out := make([]Protocol, 0, len(KnownProtocols))
	for _, n := range KnownProtocols {
		out = append(out, fakeProtocol{name: n})
	}
	return out
}

func TestValidateAcceptsTheClosedSet(t *testing.T) {
	if err := Validate(knownSet()); err != nil {
		t.Errorf("the full KnownProtocols set must validate, got: %v", err)
	}
}

func TestValidateRefusesUnknownName(t *testing.T) {
	protos := append(knownSet(), fakeProtocol{name: "bogus"})
	err := Validate(protos)
	if err == nil {
		t.Fatal("an unknown protocol name must be a load-time hard failure (spec §5)")
	}
	if got := err.Error(); !strings.Contains(got, "unknown protocol") || !strings.Contains(got, `"bogus"`) {
		t.Errorf("error must name the unknown protocol, got: %v", err)
	}
}

func TestValidateRefusesDuplicateName(t *testing.T) {
	protos := append(knownSet(), fakeProtocol{name: "relay"})
	err := Validate(protos)
	if err == nil {
		t.Fatal("a duplicate Name() must be a load-time hard failure (spec §5)")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error must name the duplicate, got: %v", err)
	}
}

func TestValidateRefusesMissingKnownName(t *testing.T) {
	protos := knownSet()
	err := Validate(protos[:len(protos)-1])
	if err == nil {
		t.Fatal("a known protocol left unbuilt must be a hard failure — the closed set is exact")
	}
}

func TestValidateRefusesNilProtocol(t *testing.T) {
	err := Validate([]Protocol{nil})
	if err == nil {
		t.Fatal("a nil protocol must be a hard failure, not a later nil-pointer panic")
	}
}

func TestIsKnown(t *testing.T) {
	if !IsKnown("a2a") {
		t.Errorf(`IsKnown("a2a") = false, want true`)
	}
	if IsKnown("bogus") {
		t.Errorf(`IsKnown("bogus") = true, want false`)
	}
}

// TestDepsShape pins the five spec fields so a silent rename does not drift
// the layer's contract from specs/CRIER-PROTOCOLS.md §3.
func TestDepsShape(t *testing.T) {
	d := Deps{
		RegistryHandler: registry.NewHandler(registry.NewMemoryStore()),
		Store:           registry.NewMemoryStore(),
		Relay:           nil,
		Config:          &config.Config{},
		Logger:          nil,
	}
	if d.Store == nil || d.Config == nil || d.RegistryHandler == nil {
		t.Fatal("Deps must accept the spec §3 wiring values")
	}
}
