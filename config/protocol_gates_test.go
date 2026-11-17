// protocol_gates_test.go — INT-022: the relay and mesh surface gates
// (specs/CRIER-PROTOCOLS.md §3).
//
// Two properties are load-bearing and both are asserted here rather than
// inferred from a route census: an UNSET variable leaves both surfaces ON
// (the behaviour every earlier release shipped, so nothing an existing
// deployment does can change), and a set value is HONORED or REFUSED — never
// silently ignored, which is what an operator who turned a surface off would
// otherwise be told by the served route set.
package config_test

import (
	"strings"
	"testing"

	"github.com/crier-dev/crier/config"
)

// loadWith is LoadFrom over an overlay: only the named variables are set, every
// other lookup answers empty, so the test never depends on process env.
func loadWith(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("LoadFrom(%v): %v", env, err)
	}
	return cfg
}

// TestLoad_ProtocolSurfaceGates_DefaultsOn pins the default: with neither
// variable set, both surfaces are enabled. This is the backward-compatibility
// contract of INT-022 — the gate exists so a deployment CAN remove the
// surface, not so an existing one loses it.
func TestLoad_ProtocolSurfaceGates_DefaultsOn(t *testing.T) {
	cfg := loadWith(t, nil)
	if !cfg.RelayEnabled {
		t.Error("CR_RELAY_ENABLED unset must leave the relay surface ON (the routes every earlier release mounted)")
	}
	if !cfg.MeshEnabled {
		t.Error("CR_MESH_ENABLED unset must leave the mesh surface ON")
	}
}

// TestLoad_ProtocolSurfaceGates_Dialect pins the accepted spellings and
// independence: the two gates are separate switches, and each accepts the
// tolerant bool dialect the other switches use (true/1/yes, false/0/no).
func TestLoad_ProtocolSurfaceGates_Dialect(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantRelay bool
		wantMesh  bool
	}{
		{
			name:      "relay off, mesh untouched",
			env:       map[string]string{"CR_RELAY_ENABLED": "false"},
			wantRelay: false,
			wantMesh:  true,
		},
		{
			name:      "mesh off, relay untouched",
			env:       map[string]string{"CR_MESH_ENABLED": "false"},
			wantRelay: true,
			wantMesh:  false,
		},
		{
			name:      "both off",
			env:       map[string]string{"CR_RELAY_ENABLED": "0", "CR_MESH_ENABLED": "no"},
			wantRelay: false,
			wantMesh:  false,
		},
		{
			name:      "explicit on in every accepted spelling",
			env:       map[string]string{"CR_RELAY_ENABLED": "1", "CR_MESH_ENABLED": "YES"},
			wantRelay: true,
			wantMesh:  true,
		},
		{
			name:      "case and whitespace are not settings",
			env:       map[string]string{"CR_RELAY_ENABLED": " False ", "CR_MESH_ENABLED": "true"},
			wantRelay: false,
			wantMesh:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadWith(t, tc.env)
			if cfg.RelayEnabled != tc.wantRelay {
				t.Errorf("RelayEnabled = %v, want %v (env %v)", cfg.RelayEnabled, tc.wantRelay, tc.env)
			}
			if cfg.MeshEnabled != tc.wantMesh {
				t.Errorf("MeshEnabled = %v, want %v (env %v)", cfg.MeshEnabled, tc.wantMesh, tc.env)
			}
		})
	}
}

// TestLoad_ProtocolSurfaceGates_InvalidIsStartupError: a value that cannot be
// honoured fails the boot naming the variable — the alternative (silently
// keeping the surface on) is exactly the defect INT-022 was filed for.
func TestLoad_ProtocolSurfaceGates_InvalidIsStartupError(t *testing.T) {
	for _, key := range []string{"CR_RELAY_ENABLED", "CR_MESH_ENABLED"} {
		for _, bad := range []string{"off", "enabled", "2", "tru"} {
			_, err := config.LoadFrom(func(k string) string {
				if k == key {
					return bad
				}
				return ""
			})
			if err == nil {
				t.Fatalf("%s=%q must fail the boot, not be ignored", key, bad)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q error must name the variable, got: %v", key, bad, err)
			}
		}
	}
}
