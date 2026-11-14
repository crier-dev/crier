package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGatedRouteTableIsUniqueAndNamesKnownConfig(t *testing.T) {
	seen := map[string]bool{}
	known := map[string]bool{
		"CR_DAGGER_URL": true, "CR_DETECT_ENABLED": true,
		"CR_PERMISSIONS_ADMIN_TOKEN": true,
	}
	for _, route := range gatedRoutes {
		if seen[route.Path] {
			t.Fatalf("gated route table repeats %q", route.Path)
		}
		seen[route.Path] = true
		if !known[route.Gate] {
			t.Errorf("gated route %q names unknown configuration gate %q", route.Path, route.Gate)
		}
	}
}

func TestGatedFallbackNamesGateOnDefaultPosture(t *testing.T) {
	cases := []struct {
		path string
		gate string
	}{
		{"/dagger/runs", "CR_DAGGER_URL"},
		{"/delivery-log", "CR_DETECT_ENABLED"},
		{"/agents/probe/kill-switch", "CR_DETECT_ENABLED"},
		{"/principals", "CR_PERMISSIONS_ADMIN_TOKEN"},
		{"/trust/anchors", "CR_PERMISSIONS_ADMIN_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := gateForRequestPath(tc.path)
			if got != tc.gate {
				t.Fatalf("gateForRequestPath(%q) = %q, want %q", tc.path, got, tc.gate)
			}
		})
	}
}

func TestOpenAPIGateExtensionsMatchRuntimeTable(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, route := range gatedRoutes {
		item, ok := doc.Paths[route.Path]
		if !ok {
			t.Errorf("OpenAPI is missing gated path %q", route.Path)
			continue
		}
		found := ""
		for _, rawOperation := range item {
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				continue
			}
			if gate, ok := operation["x-crier-gate"].(string); ok {
				found = gate
				break
			}
		}
		if found != route.Gate {
			t.Errorf("OpenAPI path %q x-crier-gate = %q, want %q", route.Path, found, route.Gate)
		}
	}
	for path, item := range doc.Paths {
		for _, rawOperation := range item {
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				continue
			}
			if gate, ok := operation["x-crier-gate"].(string); ok && gateForRequestPath(path) != gate && !strings.HasSuffix(path, "/{id}") {
				t.Errorf("OpenAPI path %q names gate %q but runtime table resolves %q", path, gate, gateForRequestPath(path))
			}
		}
	}
}

func TestDefaultPostureNamesEveryGatedRefusal(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_DAGGER_URL":              "",
		"CR_DETECT_ENABLED":          "",
		"CR_PERMISSIONS_ENABLED":     "",
		"CR_PERMISSIONS_ADMIN_TOKEN": "",
		"CR_AUTH_TOKEN":              "",
		"CR_REQUIRE_AGENT_SIG":       "false",
	})
	client := &http.Client{}
	cases := []struct {
		method string
		path   string
		status int
		gate   string
	}{
		{http.MethodGet, "/dagger/runs", http.StatusNotFound, "CR_DAGGER_URL"},
		{http.MethodGet, "/delivery-log", http.StatusNotFound, "CR_DETECT_ENABLED"},
		{http.MethodPost, "/agents/gated-probe/kill-switch", http.StatusNotFound, "CR_DETECT_ENABLED"},
		{http.MethodGet, "/principals", http.StatusForbidden, "CR_PERMISSIONS_ADMIN_TOKEN"},
		{http.MethodGet, "/trust/anchors", http.StatusForbidden, "CR_PERMISSIONS_ADMIN_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (body %#v)", resp.StatusCode, tc.status, body)
			}
			if body["gate"] != tc.gate {
				t.Fatalf("gate = %q, want %q (body %#v)", body["gate"], tc.gate, body)
			}
		})
	}
}

func TestPlainRelaySubscribeNamesWebSocketUpgrade(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_AUTH_TOKEN":        "",
	})
	resp, err := (&http.Client{}).Get(base + "/relay/subscribe/plain-get")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "websocket upgrade required" {
		t.Fatalf("plain subscribe = %d %#v, want 400 with websocket upgrade error", resp.StatusCode, body)
	}
}
