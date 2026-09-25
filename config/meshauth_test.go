package config

// meshauth_test.go — DF-CRIER-287: the configuration half of the mesh connect
// handshake and the mesh's own origin policy.
//
// Two properties are load-bearing and are pinned here rather than left to the
// docs:
//
//  1. the DEFAULT is permissive — authentication off, every origin allowed.
//     The row that filed this fix says explicitly not to tighten the default in
//     a way that breaks existing single-host deployments, and the only way to
//     keep that promise is to measure it: with the environment scrubbed, the
//     posture must be the pre-fix one.
//  2. an operator who OPTS IN gets a real refusal on both axes (an
//     unauthenticated connect, and a browser origin that is not listed).

import (
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/mesh"
)

func scrubMeshAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CR_REQUIRE_MESH_AUTH", "")
	t.Setenv("CR_MESH_AUTH_TIMEOUT_S", "")
	t.Setenv("CR_MESH_ALLOWED_ORIGINS", "")
}

// TestLoad_MeshAuthDefaultsAreOff is the migration guarantee at the config
// layer: an untouched environment leaves the mesh exactly as permissive as it
// shipped.
func TestLoad_MeshAuthDefaultsAreOff(t *testing.T) {
	scrubMeshAuthEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequireMeshAuth {
		t.Error("RequireMeshAuth defaulted to true — the mesh handshake is opt-in, and turning it on by default refuses every existing client")
	}
	if cfg.MeshAuthTimeout != 0 {
		t.Errorf("MeshAuthTimeout = %v, want the zero value (the mesh package resolves it to its own default)", cfg.MeshAuthTimeout)
	}
	if cfg.MeshAllowedOrigins != "" {
		t.Errorf("MeshAllowedOrigins = %q, want empty (allow-all)", cfg.MeshAllowedOrigins)
	}
	if got := MeshOriginPolicy(cfg.MeshAllowedOrigins); got != MeshOriginPolicyAllowAll {
		t.Errorf("MeshOriginPolicy = %q, want %q", got, MeshOriginPolicyAllowAll)
	}
	// The unset timeout must resolve to the constant the mesh package actually
	// uses, so docs and code cannot drift into two different numbers.
	if got := mesh.DefaultMeshAuthTimeout(); got != 10*time.Second {
		t.Errorf("mesh.DefaultMeshAuthTimeout() = %v, want 10s — update this test and docs/mesh-protocol.md together if the default moves", got)
	}
}

func TestLoad_RequireMeshAuth(t *testing.T) {
	cases := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{"true", true, false},
		{"1", true, false},
		{"yes", true, false},
		{"TRUE", true, false},
		{"false", false, false},
		{"0", false, false},
		{"no", false, false},
		{"maybe", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			scrubMeshAuthEnv(t)
			t.Setenv("CR_REQUIRE_MESH_AUTH", tc.value)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load accepted CR_REQUIRE_MESH_AUTH=%q, want a loud error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RequireMeshAuth != tc.want {
				t.Errorf("RequireMeshAuth = %v, want %v", cfg.RequireMeshAuth, tc.want)
			}
		})
	}
}

func TestLoad_MeshAuthTimeout(t *testing.T) {
	t.Run("seconds are parsed", func(t *testing.T) {
		scrubMeshAuthEnv(t)
		t.Setenv("CR_MESH_AUTH_TIMEOUT_S", "30")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MeshAuthTimeout != 30*time.Second {
			t.Errorf("MeshAuthTimeout = %v, want 30s", cfg.MeshAuthTimeout)
		}
	})

	for _, bad := range []string{"0", "-1", "abc", "10s"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			scrubMeshAuthEnv(t)
			t.Setenv("CR_MESH_AUTH_TIMEOUT_S", bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted CR_MESH_AUTH_TIMEOUT_S=%q, want an error naming the variable", bad)
			}
		})
	}
}

func TestLoad_MeshAllowedOriginsPassthrough(t *testing.T) {
	scrubMeshAuthEnv(t)
	t.Setenv("CR_MESH_ALLOWED_ORIGINS", "https://console.example.com, https://ops.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MeshAllowedOrigins != "https://console.example.com, https://ops.example.com" {
		t.Errorf("MeshAllowedOrigins = %q, want the raw value passed through", cfg.MeshAllowedOrigins)
	}
	if got := MeshOriginPolicy(cfg.MeshAllowedOrigins); got != MeshOriginPolicyAllowlist {
		t.Errorf("MeshOriginPolicy = %q, want %q", got, MeshOriginPolicyAllowlist)
	}
}

// TestMeshOriginPolicyModes pins the two values GET /status publishes.
func TestMeshOriginPolicyModes(t *testing.T) {
	for _, tc := range []struct {
		allowed string
		want    string
	}{
		{"", MeshOriginPolicyAllowAll},
		{"*", MeshOriginPolicyAllowAll},
		{"https://console.example.com", MeshOriginPolicyAllowlist},
		{"a, b", MeshOriginPolicyAllowlist},
	} {
		if got := MeshOriginPolicy(tc.allowed); got != tc.want {
			t.Errorf("MeshOriginPolicy(%q) = %q, want %q", tc.allowed, got, tc.want)
		}
	}
}

// TestBuildMeshCheckOrigin covers the mesh's origin rule, which is DIFFERENT
// from the relay's on purpose: an agent that sends no Origin header must keep
// working under an allowlist, and a browser on an unlisted page must not.
func TestBuildMeshCheckOrigin(t *testing.T) {
	allowlist := "https://console.example.com, https://ops.example.com"

	for _, tc := range []struct {
		name    string
		allowed string
		origin  string
		want    bool
	}{
		{"unset allows every origin", "", "https://anywhere.example", true},
		{"unset allows an absent origin", "", "", true},
		{"star allows every origin", "*", "https://anywhere.example", true},
		{"allowlist accepts a listed origin", allowlist, "https://console.example.com", true},
		{"allowlist accepts the second listed origin", allowlist, "https://ops.example.com", true},
		{"allowlist refuses an unlisted origin", allowlist, "https://evil.example", false},
		{"allowlist accepts an ABSENT origin (non-browser agent clients send none)", allowlist, "", true},
		{"allowlist is exact — no substring match", allowlist, "https://console.example.com.evil.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := BuildMeshCheckOrigin(tc.allowed)
			req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8767/mesh/connect/agent-a", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := check(req); got != tc.want {
				t.Errorf("BuildMeshCheckOrigin(%q) with Origin %q = %v, want %v", tc.allowed, tc.origin, got, tc.want)
			}
		})
	}
}

// TestBuildCheckOriginUnchangedOnTheRelayRule is the counterpart: the relay's
// rule (which DOES require an Origin once an allowlist is set) must not have
// been changed by the mesh addition, because change there would break browser
// subscribers with an absent-or-mismatched Origin.
func TestBuildCheckOriginUnchangedOnTheRelayRule(t *testing.T) {
	check := BuildCheckOrigin("https://console.example.com")

	for _, tc := range []struct {
		name   string
		origin string
		want   bool
	}{
		{"a listed origin is accepted", "https://console.example.com", true},
		{"an unlisted origin is refused", "https://evil.example", false},
		{"an ABSENT origin is refused (the relay rule requires a match)", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8767/relay/subscribe/topic", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := check(req); got != tc.want {
				t.Errorf("BuildCheckOrigin with Origin %q = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}

	if !BuildCheckOrigin("")(mustRequest(t)) {
		t.Error("an unset relay allowlist must still permit every origin")
	}
}

func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8767/relay/topics", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}
