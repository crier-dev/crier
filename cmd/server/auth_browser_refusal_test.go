package main

// DF-CRIER-444 — a BROWSER navigating to /chat on a CR_AUTH_TOKEN deployment
// gets a legible refusal page, not the bare middleware JSON. The middleware
// has no import path back into cmd/server, so cmd/server hands it the page
// through SetBrowserRefusalPage at boot; the acceptance here runs against the
// REAL server (startTestServerWithEnv boots run() with the production router
// and middleware chain), asserting the exact contract a browser reads:
//
//   Accept: text/html → 401 with a readable text/html page that names the
//   fronting requirement (DF-CRIER-333 holds the mechanism decision) and
//   carries NO token material of any kind;
//
//   no Accept (agent curl) → byte-identical {"error":"missing Authorization header"};
//   CR_AUTH_TOKEN unset → the page is served, unchanged.
//
// Status stays 401 in every arm: the refusal is legibility, not a new verdict.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// df444Token is a distinct, greppable sentinel: if any refusal body or doc
// surface leaked it, the scans in this file would find it. It is a test
// fixture value, not the deployment secret.
const df444Token = "df444-correct-horse-battery-staple"

// df444Get fetches one URL with optional Accept header and returns
// (status, content-type, body).
func df444Get(t *testing.T, url, accept string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw)
}

func TestBrowserChatNavigationUnderAuthTokenIsALegibleRefusal(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN": df444Token,
	})

	// ── 1. Browser navigation: Accept: text/html → legible HTML refusal, 401 ──
	status, ct, body := df444Get(t, baseURL+"/chat", "text/html,application/xhtml+xml")
	if status != http.StatusUnauthorized {
		t.Fatalf("browser GET /chat under CR_AUTH_TOKEN = %d, want 401 (the refusal is legibility, not a new verdict) (body: %s)", status, body)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("browser GET /chat Content-Type = %q, want text/html (got: %s)", ct, body)
	}
	for _, want := range []string{
		"fronting",
		"CR_AUTH_TOKEN",
		"DF-CRIER-333",
		"Bearer",
		"401",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("browser refusal page does not carry %q — the page must state why auth is needed and that DF-CRIER-333 holds the arrangement decision (body: %s)", want, body)
		}
	}

	// ── 2. No token material anywhere on the page ────────────────────────────
	// The deployment's real token must never appear, and neither the page nor
	// its instructions may carry token-shaped material (no assignment, no
	// curl -H Authorization recipe, no quoted placeholder in a command).
	if strings.Contains(body, df444Token) {
		t.Fatal("browser refusal page leaked the deployment token — AC 5 violated")
	}
	for _, banned := range []string{
		"Authorization:",
		"curl -H",
		"Bearer df444",
		"***",
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("browser refusal page carries token-shaped material %q — AC 5 violated", banned)
		}
	}

	// ── 3. Agent clients keep the exact JSON refusal ─────────────────────────
	// No Accept header (a plain curl / every API client): the middleware's
	// decision and body are untouched.
	status, ct, body = df444Get(t, baseURL+"/chat", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("token-less curl GET /chat = %d, want 401 (unchanged)", status)
	}
	if ct != "application/json" {
		t.Fatalf("token-less curl Content-Type = %q, want application/json (body: %s)", ct, body)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("token-less curl body is not JSON: %v (body: %s)", err, body)
	}
	if parsed["error"] != "missing Authorization header" {
		t.Fatalf("token-less curl error = %q, want the unchanged middleware wording", parsed["error"])
	}

	// A JSON-preferring Accept (every fetch/XHR API call the page itself makes)
	// also keeps the JSON refusal.
	status, ct, _ = df444Get(t, baseURL+"/chat", "application/json")
	if status != http.StatusUnauthorized || ct != "application/json" {
		t.Fatalf("JSON-accepting GET /chat = %d %s, want 401 application/json (unchanged)", status, ct)
	}

	// A WILDCARD Accept ("*/*") is not a browser navigation: it keeps JSON.
	status, ct, _ = df444Get(t, baseURL+"/chat", "*/*")
	if status != http.StatusUnauthorized || ct != "application/json" {
		t.Fatalf("wildcard-accept GET /chat = %d %s, want 401 application/json", status, ct)
	}

	// ── 4. Other paths keep the JSON refusal for browsers too ────────────────
	// Only /chat gets the page; an API path with a browser Accept header is
	// still the bare JSON 401 the middleware always answered.
	status, ct, body = df444Get(t, baseURL+"/agents", "text/html,application/xhtml+xml")
	if status != http.StatusUnauthorized {
		t.Fatalf("browser GET /agents = %d, want 401", status)
	}
	if ct != "application/json" || !strings.Contains(body, "missing Authorization header") {
		t.Fatalf("browser GET /agents = %s %s, want unchanged JSON refusal (body: %s)", ct, body, body)
	}

	// ── 5. CR_AUTH_TOKEN unset → unchanged, page served ──────────────────────
	openURL := startTestServerWithEnv(t, nil)
	status, ct, body = df444Get(t, openURL+"/chat", "text/html")
	if status != http.StatusOK || !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("auth-disabled GET /chat = %d %s, want 200 text/html (unchanged)", status, ct)
	}
	if !strings.Contains(body, "basisWords") {
		t.Fatalf("auth-disabled GET /chat body lost the chat client (no basisWords) — first 200 bytes: %.200s", body)
	}
}

// TestBrowserRefusalPageMatchesEmbeddedTemplate proves the served refusal is
// the EXACT template authored in chat.html — no drift between the page an
// operator reads and the source in the repo. The middleware receives whatever
// this template extractor produced; the extraction must not have rewritten it.
func TestBrowserRefusalPageMatchesEmbeddedTemplate(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN": df444Token,
	})
	_, _, body := df444Get(t, baseURL+"/chat", "text/html")
	if !strings.Contains(body, chatBrowserRefusalTemplate) {
		t.Fatal("served refusal page does not match the template authored in chat.html")
	}
}

// TestBrowserRefusalDocLineHasNoTokenMaterial pins the README paragraph
// (AC 2 + AC 5): the browser-fronting statement exists, points at
// DF-CRIER-333, and carries no token material on the doc line.
func TestBrowserRefusalDocLineHasNoTokenMaterial(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	// Paragraph-aware: line wrapping must not decide whether the statement
	// exists. Find the paragraph that names DF-CRIER-333, then require the
	// fronting statement in THAT paragraph, so a rewrap cannot silently
	// separate the two claims.
	var para string
	found := false
	for _, p := range strings.Split(string(raw), "\n\n") {
		if strings.Contains(p, "DF-CRIER-333") {
			para = p
			found = true
			break
		}
	}
	if !found {
		t.Fatal("README.md has no paragraph naming DF-CRIER-333")
	}
	if !strings.Contains(para, "front") || !strings.Contains(para, "Bearer") {
		t.Fatal("README DF-CRIER-333 paragraph does not state the browser-fronting requirement (fronting + Bearer)")
	}
	for _, banned := range []string{"Bearer df444", "curl -H", "Authorization:", "***"} {
		if strings.Contains(para, banned) {
			t.Fatalf("README fronting paragraph carries token-shaped material %q", banned)
		}
	}
}
