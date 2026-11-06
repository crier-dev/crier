package opencode_test

// CRIER-HARNESS-10 pins the measurement half of the opencode id policy: the
// adapter id stays `opencode` whatever major the installed CLI carries, and
// Probe reports the MEASURED version through the standard probe mechanism — so
// a future opencode 2.x is detected by the VERSION column of `crier harness
// list`, never by a new id (`opencode-v2` names no artifact and is refused by
// name; that refusal is pinned in internal/harness/names_routing_test.go).
// The policy text and the admission evidence a v2 must produce live in
// specs/CRIER-HARNESS.md §6.7.
//
// ch:trace row=CRIER-HARNESS-10 spec=specs/CRIER-HARNESS.md#opencode-id-policy
// wave=.coding-hermes/waves/crier-foreman-2026-11-06-16-31-19.json#CRIER-HARNESS-10
// test=internal/harness/opencode/opencode_test.go::TestProbeReportsTheMeasuredVersion
// doc=specs/CRIER-HARNESS.md evidence=go test -short ./internal/harness/... (local run log in
// the closeout report) witness=none:local-worktree-no-published-endpoint
// verdict=none:foreman-completes-after-merge commit=wt/CRIER-HARNESS-10 head reported at closeout
// memory=none:foreman-closeout

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/crier-dev/crier/internal/harness/opencode"
)

// versionStub is a stand-in opencode binary that answers --version with one
// line and exits 0. It consults no network and starts no session.
func versionStub(t *testing.T, versionLine string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/bash\n" +
		"if [ \"${1:-}\" = \"--version\" ]; then\n" +
		"  printf '%s\\n' '" + versionLine + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return bin
}

// TestProbeReportsTheMeasuredVersion: the capability the adapter exposes must
// carry the version the probe measured — that column is the only admission
// surface a future v2 has (specs/CRIER-HARNESS.md §6.7).
func TestProbeReportsTheMeasuredVersion(t *testing.T) {
	a := opencode.New(opencode.Config{Binary: versionStub(t, "opencode 1.18.29")})
	if a.Name() != "opencode" {
		t.Fatalf("Name() = %q, want the single id opencode", a.Name())
	}
	cap, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !cap.Available {
		t.Fatalf("the stub measures cleanly and must be available: %+v", cap)
	}
	if cap.Version != "1.18.29" {
		t.Fatalf("the measured version must reach the capability: %+v", cap)
	}
}

// TestTheIdStaysOpencodeWhileTheVersionCarriesTheMajor is the policy in one
// table: the id never forks per major, only the version moves. A hypothetical
// opencode 2.0.0 binary is still served by the id `opencode`, and its measured
// version — not a new id — is what would admit it.
func TestTheIdStaysOpencodeWhileTheVersionCarriesTheMajor(t *testing.T) {
	for _, tc := range []struct{ printed, want string }{
		{"opencode 1.18.29", "1.18.29"},
		{"opencode 2.0.0", "2.0.0"},
	} {
		a := opencode.New(opencode.Config{Binary: versionStub(t, tc.printed)})
		if a.Name() != "opencode" {
			t.Fatalf("Name() = %q with %q installed: the id must not fork per major", a.Name(), tc.printed)
		}
		cap, err := a.Probe(context.Background())
		if err != nil {
			t.Fatalf("probe %q: %v", tc.printed, err)
		}
		if cap.Version != tc.want {
			t.Fatalf("printed %q: Version = %q, want %q (the version carries the major)", tc.printed, cap.Version, tc.want)
		}
	}
}
