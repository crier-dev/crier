// fallback_test.go — DF-CRIER-213 acceptance evidence: the router's own 404
// and 405 answers.
//
// Everything here runs against the REAL router via startTestServer (the
// cmd/server harness in main_test.go: auth off, in-memory registry, free
// port, run(nil), SIGTERM cleanup), so what is asserted is the wire contract
// of the running server — not a hand-built mux in the test.
//
// The pre-fix behaviour, captured from a live boot of HEAD 96684ed on a
// scratch port before this change:
//
//	GET /nope              404, Content-Type: text/plain; charset=utf-8,
//	                       19-byte body "404 page not found\n", no X-Request-Id
//	DELETE /health         405, zero-length body, no Content-Type, no Allow,
//	                       no X-Request-Id
//	GET /relay/publish     405, same empty shape
//	GET /agents (no token) 401 {"error":"missing Authorization header"}
//	                       + Content-Type: application/json + X-Request-Id
//
// The last two lines are the contrast that makes the first three a defect:
// the JSON error surfaces already existed, the two router defaults were the
// only ones outside them. The tests below pin the post-fix contract for
// both, and pin the matched-route answers so the fallbacks cannot have
// swallowed real traffic.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// wireResponse is the whole response surface a fallback assertion needs:
// status, headers (Content-Type, X-Request-Id, X-Content-Type-Options,
// Allow) and the body as exact bytes.
type wireResponse struct {
	status int
	header http.Header
	body   string
}

// probeWire issues one request against the running server and returns the
// full response.
func probeWire(t *testing.T, client *http.Client, method, url string, hdrs map[string]string) wireResponse {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: build request: %v", method, url, err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, url, err)
	}
	return wireResponse{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// assertJSONErrorEnvelope asserts the full contract of a router-fallback
// answer: the status, the exact Content-Type, the nosniff hardening header
// (both set by internal/httperr, so the fallback is indistinguishable from
// every other JSON error in the repo), the exact wire bytes of the
// {"error": …} envelope, a decodable body carrying the message, and a
// non-empty correlation id.
func assertJSONErrorEnvelope(t *testing.T, name string, got wireResponse, wantStatus int, wantMessage string) {
	t.Helper()

	if got.status != wantStatus {
		t.Errorf("%s: status %d, want %d", name, got.status, wantStatus)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q, want %q — a JSON body must declare it, never leave the router/net/http default in place",
			name, ct, "application/json")
	}
	if got.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options %q, want %q (internal/httperr sets it on every JSON error)",
			name, got.header.Get("X-Content-Type-Options"), "nosniff")
	}

	// Exact bytes: the envelope has to be the same shape the other JSON
	// surfaces write, compact object plus the terminating newline.
	wantBody := `{"error":"` + wantMessage + `"}` + "\n"
	if got.body != wantBody {
		t.Errorf("%s: body %q, want exactly %q", name, got.body, wantBody)
	}

	var decoded map[string]string
	if err := json.Unmarshal([]byte(got.body), &decoded); err != nil {
		t.Errorf("%s: body %q is not a JSON object: %v", name, got.body, err)
	} else if decoded["error"] != wantMessage {
		t.Errorf("%s: decoded error %q, want %q", name, decoded["error"], wantMessage)
	}

	if id := got.header.Get("X-Request-Id"); strings.TrimSpace(id) == "" {
		t.Errorf("%s: X-Request-Id is missing/empty — the fallback ran outside middleware.RequestID", name)
	}
}

// TestUnknownPathAnswersJSON404 pins the 404 contract a mistyped path gets:
// the same JSON envelope as every other error surface, with a correlation
// id, instead of net/http's text/plain "404 page not found".
func TestUnknownPathAnswersJSON404(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	got := probeWire(t, client, http.MethodGet, baseURL+"/nope", nil)
	assertJSONErrorEnvelope(t, "GET /nope", got, http.StatusNotFound, "not found")

	// An inbound correlation id is adopted verbatim (middleware.RequestID is
	// outermost in the fallback chain, so the echo is already set when the
	// status line goes out).
	const inbound = "df213-inbound-correlation-id"
	got = probeWire(t, client, http.MethodGet, baseURL+"/nope", map[string]string{"X-Request-Id": inbound})
	if id := got.header.Get("X-Request-Id"); id != inbound {
		t.Errorf("GET /nope with an inbound X-Request-Id: echo %q, want %q (the fallback must adopt it, not drop it)", id, inbound)
	}

	// An unknown path is unknown whatever the method: the router must not
	// answer 405 for a path nothing serves (that would be an Allow header
	// derived from a mismatch that never happened).
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodOptions} {
		got := probeWire(t, client, method, baseURL+"/nope", nil)
		assertJSONErrorEnvelope(t, method+" /nope", got, http.StatusNotFound, "not found")
		if allow := got.header.Get("Allow"); allow != "" {
			t.Errorf("%s /nope: Allow %q, want unset — no route serves that path", method, allow)
		}
	}
}

// TestUnknownPathStaysTokenFreeWithAuthEnabled: the 404 fallback is NOT
// wrapped in middleware.Auth. An unknown path must keep answering 404 when
// auth is on (a token-less probe must not be told 401 for a route that does
// not exist), which is exactly the pre-fix behaviour apart from the body and
// headers.
func TestUnknownPathStaysTokenFreeWithAuthEnabled(t *testing.T) {
	baseURL := bootObservabilityServerAuth(t, "df-213-token")
	client := &http.Client{Timeout: 5 * time.Second}

	got := probeWire(t, client, http.MethodGet, baseURL+"/nope", nil)
	assertJSONErrorEnvelope(t, "GET /nope (auth enabled, no token)", got, http.StatusNotFound, "not found")

	// The contrast on the same server: a matched route still rejects the
	// token-less request, so the 404 above is the fallback's own answer and
	// not an auth exemption for everything.
	if authed := probeWire(t, client, http.MethodGet, baseURL+"/agents", nil); authed.status != http.StatusUnauthorized {
		t.Errorf("GET /agents without a token: status %d, want 401 (a matched route must still require the token)", authed.status)
	}
}

// TestWrongMethodAnswersJSON405 pins the 405 contract, including the Allow
// header derived from the router's own match table.
func TestWrongMethodAnswersJSON405(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	cases := []struct {
		name   string
		method string
		path   string
		allow  string
	}{
		// One route per path: the single registered method.
		{name: "DELETE /health", method: http.MethodDelete, path: "/health", allow: "GET"},
		{name: "GET /relay/publish", method: http.MethodGet, path: "/relay/publish", allow: "POST"},
		// Two routes share /agents (GET + POST): both methods, probe order.
		{name: "DELETE /agents", method: http.MethodDelete, path: "/agents", allow: "GET, POST"},
		// Three routes share /agents/{id} (GET, PATCH, DELETE) and the
		// request method is none of them.
		{name: "PUT /agents/probe-213", method: http.MethodPut, path: "/agents/probe-213", allow: "GET, PATCH, DELETE"},
	}

	for _, tc := range cases {
		got := probeWire(t, client, tc.method, baseURL+tc.path, nil)
		assertJSONErrorEnvelope(t, tc.name, got, http.StatusMethodNotAllowed, "method not allowed")
		if allow := got.header.Get("Allow"); allow != tc.allow {
			t.Errorf("%s: Allow %q, want %q — derived from the router, not fabricated", tc.name, allow, tc.allow)
		}
	}

	// The Allow derivation must not leak onto a status where it has no
	// meaning: a 404 never carries one (covered in the 404 test), and a
	// matched 200 never carries one either.
	if ok := probeWire(t, client, http.MethodGet, baseURL+"/health", nil); ok.header.Get("Allow") != "" {
		t.Errorf("GET /health: Allow %q, want unset on a 200", ok.header.Get("Allow"))
	}
}

// TestMatchedRoutesUnchangedByFallbacks: installing the two fallbacks must
// not have changed any route the router does match. Same harness, same
// assertions on the two public JSON endpoints (/health, /version) plus a
// registry route that runs the full middleware chain.
func TestMatchedRoutesUnchangedByFallbacks(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	health := probeWire(t, client, http.MethodGet, baseURL+"/health", nil)
	if health.status != http.StatusOK {
		t.Errorf("GET /health: status %d, want 200", health.status)
	}
	if ct := health.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /health: Content-Type %q, want application/json", ct)
	}
	if health.body != `{"status":"ok"}` {
		t.Errorf("GET /health: body %q, want exactly %q", health.body, `{"status":"ok"}`)
	}
	if strings.TrimSpace(health.header.Get("X-Request-Id")) == "" {
		t.Error("GET /health: X-Request-Id missing — the matched path lost its correlation id")
	}

	version := probeWire(t, client, http.MethodGet, baseURL+"/version", nil)
	if version.status != http.StatusOK {
		t.Errorf("GET /version: status %d, want 200", version.status)
	}
	if ct := version.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /version: Content-Type %q, want application/json", ct)
	}
	var identity map[string]any
	if err := json.Unmarshal([]byte(version.body), &identity); err != nil {
		t.Errorf("GET /version: body %q is not valid JSON: %v", version.body, err)
	}

	agents := probeWire(t, client, http.MethodGet, baseURL+"/agents", nil)
	if agents.status != http.StatusOK {
		t.Errorf("GET /agents: status %d, want 200 (matched route, auth disabled)", agents.status)
	}
	if !json.Valid([]byte(agents.body)) {
		t.Errorf("GET /agents: body %q is not valid JSON", agents.body)
	}
}

// syncBuffer is a goroutine-safe buffer: the access-log line is written by
// the server's own goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureServerLogs repoints the process-wide slog default logger at a
// buffer AFTER the server has booted. run() installs its own handler through
// initLogger, but middleware.Logging calls the package-level slog.Info, which
// resolves slog.Default() at call time — so swapping it here captures the
// running server's access-log lines in-process, with no stderr pipe and no
// child process.
func captureServerLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// waitForLogLine polls the captured logs for a line containing needle. The
// access-log write is deferred until the fallback handler returns, so it can
// land just after the client sees the response.
func waitForLogLine(t *testing.T, logs *syncBuffer, needle string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, needle) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no access-log line containing %q within 3s; captured logs:\n%s", needle, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMistypedPathIsCorrelatedAndLogged is the parity half of DF-CRIER-213:
// gorilla applies r.Use(…) only to matched handlers, so before this change a
// mistyped path produced no X-Request-Id and no access-log line. Both are
// asserted here against the live server, on the same request: the response
// header and the logged request_id must be the same id.
func TestMistypedPathIsCorrelatedAndLogged(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	logs := captureServerLogs(t)

	const inbound = "df213-logged-correlation-id"
	got := probeWire(t, client, http.MethodGet, baseURL+"/mispelled-path", map[string]string{"X-Request-Id": inbound})
	assertJSONErrorEnvelope(t, "GET /mispelled-path", got, http.StatusNotFound, "not found")

	line := waitForLogLine(t, logs, "path=/mispelled-path")
	for _, want := range []string{"msg=request", "method=GET", "path=/mispelled-path", "status=404", "request_id=" + inbound} {
		if !strings.Contains(line, want) {
			t.Errorf("access-log line %q is missing %q", line, want)
		}
	}
}

// TestWrongMethodIsLoggedWith405: the 405 fallback is wrapped in Logging
// too, so a wrong-method request leaves an access-log line carrying the
// status the client actually received.
func TestWrongMethodIsLoggedWith405(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	logs := captureServerLogs(t)

	got := probeWire(t, client, http.MethodDelete, baseURL+"/health", nil)
	assertJSONErrorEnvelope(t, "DELETE /health", got, http.StatusMethodNotAllowed, "method not allowed")

	line := waitForLogLine(t, logs, "path=/health")
	for _, want := range []string{"msg=request", "method=DELETE", "status=405"} {
		if !strings.Contains(line, want) {
			t.Errorf("access-log line %q is missing %q", line, want)
		}
	}
}
