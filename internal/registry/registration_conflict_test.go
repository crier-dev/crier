package registry

import (
	"errors"
	"strings"
	"testing"
)

func TestRegistrationConflict_KeyCombinations(t *testing.T) {
	cases := []struct {
		name         string
		storedKey    HexKey
		incomingKey  HexKey
		wantMismatch bool
		wantKeyless  bool
	}{
		{name: "empty-vs-empty", wantKeyless: true},
		{name: "empty-vs-set", incomingKey: HexKey{1}, wantMismatch: true},
		{name: "set-vs-different-set", storedKey: HexKey{1}, incomingKey: HexKey{2}, wantMismatch: true},
		{name: "set-vs-same-set", storedKey: HexKey{1}, incomingKey: HexKey{1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := registrationConflict(&Agent{ID: "agent", PublicKey: tc.storedKey}, &Agent{ID: "agent", PublicKey: tc.incomingKey})
			if err == nil {
				t.Fatal("registrationConflict returned nil")
			}
			if errors.Is(err, ErrPublicKeyMismatch) != tc.wantMismatch {
				t.Fatalf("mismatch classification = %v, want %v: %v", errors.Is(err, ErrPublicKeyMismatch), tc.wantMismatch, err)
			}
			if tc.wantMismatch {
				if errors.Is(err, ErrAgentExists) {
					t.Fatalf("mismatch error also matches ErrAgentExists: %v", err)
				}
			} else if !errors.Is(err, ErrAgentExists) {
				t.Fatalf("error = %v, want ErrAgentExists", err)
			}
			if tc.wantKeyless {
				if !strings.Contains(err.Error(), "keyless") || strings.Contains(err.Error(), "different public key") {
					t.Fatalf("keyless error = %q, want actionable keyless detail without mismatch wording", err)
				}
			}
		})
	}
}

// ch:trace row=DF-CRIER-504 spec=none:dogfood-finding wave=crier-foreman-2026-11-12-15-18-43 test=TestRegistrationConflict_KeyCombinations doc=none:registry-error evidence=internal/registry/registration_conflict_test.go witness=none:live-bridge-not-required verdict=none:foreman-gitreins commit=none:worker-commit memory=none:foreman-closeout
// STATUS: complete
// TIER: T1
// EVIDENCE: TestRegistrationConflict_KeyCombinations
