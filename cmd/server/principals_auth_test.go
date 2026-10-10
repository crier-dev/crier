package main

// DF-CRIER-297 acceptance — the permissions management surface must be
// REACHABLE when CR_AUTH_TOKEN, CR_PERMISSIONS_ENABLED and
// CR_PERMISSIONS_ADMIN_TOKEN are all set.
//
// Before the fix the shared auth middleware demanded Bearer == CR_AUTH_TOKEN on
// every non-exempt path while the management handlers demanded the SAME header
// to constant-time-equal CR_PERMISSIONS_ADMIN_TOKEN, so one Authorization header
// had to equal two secrets and the surface answered 401 or 403 to everything.
//
// These cases run against the REAL server: startTestServerWithEnv boots run()
// with the same middleware, router, registry store and permissions store the
// fleet runs, so nothing here re-implements a code path, and the assertions are
// the STATUS CODES a client sees plus the handler's own JSON.
//
// Two names in the task's acceptance list do not match the served surface, and
// the tests assert what the code actually does (measured live on this build)
// rather than the assumption:
//
//   - the mint route answers 201 Created, not 200 (HandleMintPrincipal writes
//     http.StatusCreated) — every management create is a 201;
//   - there is no GET /principals route: /principals is registered POST-only,
//     so the method mismatch is a 405 from mux. The four openapi-documented
//     management paths are POST /principals, /bindings, /grants and
//     /grants/{grantid}/revoke; POST /agents/{agentid}/class is the fifth
//     route on the same admin gate, and this file drives all five.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	df297MessageToken = "message-secret"
	df297AdminToken   = "admin-secret"
)

// df297Request issues one request against the live server with an optional
// bearer token, an optional X-Agent-ID header and an optional JSON body.
func df297Request(t *testing.T, client *http.Client, method, url, bearer, agentID, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if agentID != "" {
		req.Header.Set("X-Agent-ID", agentID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, url, err)
	}
	return resp.StatusCode, string(raw)
}

// df297JSON decodes a response body into a map, failing the test on malformed
// JSON.
func df297JSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response body is not JSON: %v (body: %s)", err, body)
	}
	return out
}

// TestManagementRoutesWithDualTokens is the DF-CRIER-297 end-to-end gate: with
// all three env vars set, every management write is reachable with the ADMIN
// token and refused with the message token, while the message token still
// reaches the relay, the inbox and the mesh — and the admin token is refused
// everywhere outside the management surface.
func TestManagementRoutesWithDualTokens(t *testing.T) {
	// CR_REQUIRE_AGENT_SIG is off so the inbox leg exercises the AUTH
	// middleware rather than failing on the per-agent ed25519 signature the
	// documented dev shortcut disables; the management legs are unaffected by
	// it. Sig enforcement is deliberately not the subject of this gate.
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":              df297MessageToken,
		"CR_PERMISSIONS_ENABLED":     "true",
		"CR_PERMISSIONS_DIR":         t.TempDir(),
		"CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken,
		"CR_REQUIRE_AGENT_SIG":       "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	// ── 1. the management surface, reached with the ADMIN token ──────────────
	status, body := df297Request(t, client, http.MethodPost, baseURL+"/principals",
		df297AdminToken, "", `{"display_name":"Bane","namespace":"acme","role":"owner"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /principals with the admin token = %d, want 201 (body: %s)", status, body)
	}
	principalID, _ := df297JSON(t, body)["id"].(string)
	if !strings.HasPrefix(principalID, "prin_") {
		t.Fatalf("minted principal id = %q, want a prin_ id", principalID)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/bindings", df297AdminToken, "",
		fmt.Sprintf(`{"principal":%q,"agent":"atlas"}`, principalID))
	if status != http.StatusCreated {
		t.Fatalf("POST /bindings with the admin token = %d, want 201 (body: %s)", status, body)
	}

	// The agent-class route answers 201 for the same reason the other creates
	// do (HandleSetAgentClass writes StatusCreated).
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/agents/atlas/class",
		df297AdminToken, "", `{"class":"service"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /agents/atlas/class with the admin token = %d, want 201 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/grants", df297AdminToken, "",
		fmt.Sprintf(`{"principal":%q,"subject":{"type":"agent","ref":"atlas"},"actions":["send"]}`, principalID))
	if status != http.StatusCreated {
		t.Fatalf("POST /grants with the admin token = %d, want 201 (body: %s)", status, body)
	}
	grantID, _ := df297JSON(t, body)["id"].(string)
	if !strings.HasPrefix(grantID, "grant_") {
		t.Fatalf("created grant id = %q, want a grant_ id", grantID)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/grants/"+grantID+"/revoke",
		df297AdminToken, "", `{}`)
	if status != http.StatusOK {
		t.Fatalf("POST /grants/%s/revoke with the admin token = %d, want 200 (body: %s)", grantID, status, body)
	}
	if revoked, _ := df297JSON(t, body)["revoked"].(string); revoked != grantID {
		t.Fatalf("revoke answered %q, want the revoked id %q", revoked, grantID)
	}

	// The task's acceptance names "GET /principals 200". No such route exists:
	// /principals is POST-only, so mux answers the method mismatch — asserted
	// here so the absence is recorded rather than silently believed.
	status, body = df297Request(t, client, http.MethodGet, baseURL+"/principals", df297AdminToken, "", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /principals = %d, want 405 (there is no read route on this surface; body: %s)", status, body)
	}

	// ── 2. the same surface with the MESSAGE token: the handler's named 403 ──
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/principals",
		df297MessageToken, "", `{"display_name":"X"}`)
	if status != http.StatusForbidden {
		t.Fatalf("POST /principals with the message token = %d, want 403 (body: %s)", status, body)
	}
	if got := df297JSON(t, body)["error"]; got != "MANAGEMENT_FORBIDDEN" {
		t.Fatalf("message token on the management surface = %v, want MANAGEMENT_FORBIDDEN", got)
	}

	// ── 3. the message token still reaches relay, mesh, registry and inbox ──
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/relay/publish",
		df297MessageToken, "probe-297", `{"topic":"crier.297","event":{"n":1}}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST /relay/publish with the message token = %d, want 202 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodGet, baseURL+"/mesh/peers", df297MessageToken, "", "")
	if status != http.StatusOK {
		t.Fatalf("GET /mesh/peers with the message token = %d, want 200 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodGet, baseURL+"/agents", df297MessageToken, "", "")
	if status != http.StatusOK {
		t.Fatalf("GET /agents with the message token = %d, want 200 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/agents", df297MessageToken, "",
		`{"id":"probe-297","public_key":"`+strings.Repeat("ab", 32)+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /agents with the message token = %d, want 201 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/agents/probe-297/inbox",
		df297MessageToken, "", `{"payload":{"text":"hi"}}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /agents/probe-297/inbox with the message token = %d, want 201 (body: %s)", status, body)
	}

	status, body = df297Request(t, client, http.MethodGet, baseURL+"/agents/probe-297/inbox",
		df297MessageToken, "", "")
	if status != http.StatusOK {
		t.Fatalf("GET /agents/probe-297/inbox with the message token = %d, want 200 (body: %s)", status, body)
	}

	// ── 4. the admin token is a management-surface credential ONLY ───────────
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/relay/publish",
		df297AdminToken, "probe-297", `{"topic":"crier.297","event":{"n":1}}`)
	if status != http.StatusForbidden {
		t.Fatalf("POST /relay/publish with the admin token = %d, want 403 (body: %s)", status, body)
	}
	if !strings.Contains(body, "management surface only") {
		t.Fatalf("admin-token refusal body = %s, want the wrong-surface message", body)
	}

	status, _ = df297Request(t, client, http.MethodGet, baseURL+"/mesh/peers", df297AdminToken, "", "")
	if status != http.StatusForbidden {
		t.Fatalf("GET /mesh/peers with the admin token = %d, want 403", status)
	}

	status, _ = df297Request(t, client, http.MethodGet, baseURL+"/agents", df297AdminToken, "", "")
	if status != http.StatusForbidden {
		t.Fatalf("GET /agents with the admin token = %d, want 403", status)
	}

	// ── 5. no credential or an unknown one is still a plain 401 ─────────────
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/principals", "", "", `{}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /principals with no token = %d, want 401 (body: %s)", status, body)
	}
	if !strings.Contains(body, "missing Authorization header") {
		t.Fatalf("no-token refusal body = %s, want the missing-header message", body)
	}

	status, body = df297Request(t, client, http.MethodPost, baseURL+"/principals", "wrong-token", "", `{}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /principals with an unknown token = %d, want 401 (body: %s)", status, body)
	}
	if !strings.Contains(body, "invalid token") {
		t.Fatalf("unknown-token refusal body = %s, want the invalid-token message", body)
	}

	// The exemptions that predate this row are untouched.
	for _, path := range []string{"/health", "/version"} {
		status, body = df297Request(t, client, http.MethodGet, baseURL+path, "", "", "")
		if status != http.StatusOK {
			t.Fatalf("GET %s with no token = %d, want 200 (body: %s)", path, status, body)
		}
	}
}

// TestServerRefusesToBootWithCollidingPermissionSecrets is the config half of
// DF-CRIER-297 at the real entry point: run() must return a NON-ZERO code when
// the management admin token is set to the same secret as the message token,
// instead of serving a deployment whose admin gate is the shared credential.
func TestServerRefusesToBootWithCollidingPermissionSecrets(t *testing.T) {
	const shared = "one-secret-for-both"
	t.Setenv("CR_AUTH_TOKEN", shared)
	t.Setenv("CR_PERMISSIONS_ENABLED", "true")
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", shared)
	t.Setenv("CR_PERMISSIONS_DIR", t.TempDir())
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CRIER_PORT", strconv.Itoa(freePort(t)))

	exitCode := make(chan int, 1)
	go func() { exitCode <- run(nil) }()

	select {
	case code := <-exitCode:
		if code == 0 {
			t.Fatal("run() returned 0 with CR_PERMISSIONS_ADMIN_TOKEN == CR_AUTH_TOKEN; the boot must be refused")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run() did not return within 20s on the colliding-secret config (it must refuse the boot)")
	}
}
