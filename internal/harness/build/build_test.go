package build_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/build"
)

// TestRegistryRegistersConfiguredAdaptersAndStandsInForLaterRows proves the
// config contract: hermes and opencode are real adapters, pi and codex are
// accepted blocks that report why they are unavailable, and a block removed
// from the config (a disabled one) is simply absent.
func TestRegistryRegistersConfiguredAdaptersAndStandsInForLaterRows(t *testing.T) {
	cfg := harness.DefaultConfig()
	cfg.Enabled = true
	cfg.Adapters["hermes"] = harness.AdapterConfig{Binary: "/nonexistent/hermes-binary"}
	cfg.Adapters["opencode"] = harness.AdapterConfig{Binary: "/nonexistent/opencode-binary"}
	delete(cfg.Adapters, "codex")
	cfg.Disabled["codex"] = "malformed adapter block: synthetic"

	reg := build.Registry(cfg)
	caps := reg.Capabilities(context.Background())

	if caps["hermes"].Available || !strings.Contains(caps["hermes"].Reason, "/nonexistent/hermes-binary") {
		t.Fatalf("hermes must be unavailable and name its binary: %+v", caps["hermes"])
	}
	if caps["opencode"].Available {
		t.Fatalf("opencode must be unavailable for a bogus binary: %+v", caps["opencode"])
	}
	if caps["pi"].Available || !strings.Contains(caps["pi"].Reason, "CRIER-HARNESS-2") {
		t.Fatalf("pi must report its later row: %+v", caps["pi"])
	}
	if _, ok := caps["codex"]; ok {
		t.Fatal("a disabled adapter block must not be registered at all")
	}
	if !reg.Enabled() {
		t.Fatal("registry must carry harness.enabled")
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
