package build_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/build"
)

// TestRegistryRegistersEveryConfiguredAdapter proves the config contract: all
// four adapters are real, so a bogus binary makes that adapter — and only that
// adapter — report why it is unavailable, and a refused block is simply absent.
func TestRegistryRegistersEveryConfiguredAdapter(t *testing.T) {
	cfg := harness.DefaultConfig()
	cfg.Enabled = true
	cfg.Adapters["hermes"] = harness.AdapterConfig{Binary: "/nonexistent/hermes-binary"}
	cfg.Adapters["opencode"] = harness.AdapterConfig{Binary: "/nonexistent/opencode-binary"}
	cfg.Adapters["pi"] = harness.AdapterConfig{Binary: "/nonexistent/pi-binary"}
	delete(cfg.Adapters, "codex")
	cfg.Disabled["codex"] = "malformed adapter block: synthetic"

	reg := build.Registry(cfg)
	caps := reg.Capabilities(context.Background())

	for name, binary := range map[string]string{
		"hermes":   "/nonexistent/hermes-binary",
		"opencode": "/nonexistent/opencode-binary",
		"pi":       "/nonexistent/pi-binary",
	} {
		if caps[name].Available || !strings.Contains(caps[name].Reason, binary) {
			t.Fatalf("%s must be a real adapter that is unavailable and names its binary: %+v", name, caps[name])
		}
	}
	if _, ok := caps["codex"]; ok {
		t.Fatal("a disabled adapter block must not be registered at all")
	}
	if !reg.Enabled() {
		t.Fatal("registry must carry harness.enabled")
	}
}

// TestRegistryPiAndCodexCarryTheirSurfaces: the two adapters this build adds
// advertise the flags they actually use, and codex's sandbox default is the
// one §4.1 documents.
func TestRegistryPiAndCodexCarryTheirSurfaces(t *testing.T) {
	cfg := harness.DefaultConfig()
	cfg.Enabled = true
	cfg.Adapters["pi"] = harness.AdapterConfig{Binary: "/nonexistent/pi-binary"}
	cfg.Adapters["codex"] = harness.AdapterConfig{Binary: "/nonexistent/codex-binary"}

	reg := build.Registry(cfg)
	caps := reg.Capabilities(context.Background())

	pi := caps["pi"]
	if !pi.Features["print"] || !pi.Features["session_id"] || !pi.Features["create"] || !pi.Features["continue"] {
		t.Fatalf("pi must advertise -p and --session-id for create+continue: %+v", pi)
	}
	codex := caps["codex"]
	if !codex.Features["resume"] || !codex.Features["create"] || !codex.Features["sandbox"] {
		t.Fatalf("codex must advertise exec, resume and an explicit sandbox: %+v", codex)
	}
}

func TestRegistryDeliveryFeaturesMatchConfig(t *testing.T) {
	cfg := harness.DefaultConfig()
	cfg.Adapters["hermes"] = harness.AdapterConfig{Binary: "hermes"}
	reg := build.Registry(cfg)

	cap, err := reg.Probe(context.Background(), "hermes")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if cap.Features["gateway"] {
		t.Fatal("no gateway_url configured: gateway feature must be false")
	}
	if cap.Features["create"] {
		t.Fatal("no gateway configured: the CLI path cannot create, so create must be false")
	}
	if !cap.Features["continue"] {
		t.Fatal("the CLI path can always continue: continue must be true")
	}
}
