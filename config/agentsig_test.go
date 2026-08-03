package config

import "testing"

func TestLoad_RequireAgentSigDefaultTrue(t *testing.T) {
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_REQUIRE_AGENT_SIG", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("CRIER_PORT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireAgentSig {
		t.Fatal("RequireAgentSig should default to true (secure by default)")
	}
}

func TestLoad_RequireAgentSigDisabled(t *testing.T) {
	t.Setenv("CR_REQUIRE_AGENT_SIG", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequireAgentSig {
		t.Fatal("RequireAgentSig should be false when CR_REQUIRE_AGENT_SIG=false")
	}
}

func TestLoad_RequireAgentSigInvalidValue(t *testing.T) {
	t.Setenv("CR_REQUIRE_AGENT_SIG", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid CR_REQUIRE_AGENT_SIG value")
	}
}
