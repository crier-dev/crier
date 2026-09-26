// status_test.go — DF-CRIER-113: GET /status reports the EFFECTIVE runtime
// posture of a live server (auth, per-agent signing, guard switch, registry
// backend, federation/webhook/inspection modes, build identity) and never a
// credential.
//
// Three layers, because the endpoint has three separate failure modes:
//
//  1. the DECISION SEAM — which backend serves is decided by
//     registryBackendForURL, the same helper run() keys the store construction
//     off, so the advertised backend cannot drift from the answering store;
//  2. the PURE BUILDER — buildStatusResponse over a fully-populated
//     config.Config, which is where secret non-disclosure and the exact wire
//     schema are pinned (no database, no sockets, every field populated);
//  3. the LIVE SERVER — the real router with the real middleware, which is the
//     only way to prove /status is reachable unauthenticated when auth is off,
//     401 without a Bearer token when auth is on, and that /health and /version
//     stayed exempt while /status did not.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/buildinfo"
)

// statusScrubbedEnv is the environment every test in this file neutralizes
// before it boots or loads configuration. The assertions are about the
// EFFECTIVE posture, so an ambient CR_* value inherited from the runner's
// environment (or left behind by an earlier test in this package) must never
// decide the answer.
var statusScrubbedEnv = []string{
	"CR_AUTH_TOKEN",
	"CR_DATABASE_URL", "DATABASE_URL", "CRIER_DATABASE_URL",
	"CR_GUARD_ENABLED", "CR_GUARD_DEEPSEEK_BASE_URL", "CR_GUARD_MODEL", "CR_GUARD_KANBAN_URL",
	"CR_REQUIRE_AGENT_SIG",
	// DF-CRIER-287: the mesh posture is read from these, so an ambient value
	// (inherited from the runner or left behind by a sibling test) must not
	// decide what /status reports.
	"CR_REQUIRE_MESH_AUTH", "CR_MESH_AUTH_TIMEOUT_S", "CR_MESH_ALLOWED_ORIGINS",
	// CR-FEAT-024: the presence posture is read from this one, for the same
	// reason — presence_stale_after_s must report the DEFAULT window unless the
	// case under test sets it explicitly.
	"CR_PRESENCE_STALE_AFTER_S",
	"CR_WEBHOOK_SECRET",
	"CR_FED_LINKS", "CR_FED_TOKEN", "CR_FED_QUEUE_FILE",
	"CR_ENABLE_METRICS", "CR_ENABLE_PPROF",
	"CR_LOG_LEVEL", "CR_LOG_FORMAT", "CR_RATE_LIMIT_PER_MINUTE",
	// CR-FEAT-035: the global inbox-ingest budget is read from this one, for
	// the same reason — a case must see the budget it asked for (or none), not
	// whatever the runner's environment happened to carry.
	"CR_RATE_LIMIT_GLOBAL_PER_MINUTE",
}

// statusTopLevelKeys is the documented wire schema of GET /status. It is
// asserted EXACTLY (not as a subset) so the shape is a contract: a field added
// or renamed without touching docs/openapi.yaml fails here instead of shipping
// as an undocumented key.
var statusTopLevelKeys = []string{
	"auth_enabled",
	"build",
	"federation_enabled",
	"federation_hold_queue",
	// CR-FEAT-035: the effective global inbox-ingest budget (posture) and the
	// LIVE store-wide queue measurement. Both are documented in
	// docs/openapi.yaml's /status schema; adding one here without adding it
	// there (or vice versa) is what this exact comparison exists to catch.
	"global_rate_limit_per_minute",
	"guard_enabled",
	"log_format",
	"log_level",
	"mesh_auth_required",
	"mesh_origin_policy",
	"metrics_enabled",
	"pprof_enabled",
	// CR-FEAT-024: the staleness window a row's derived status is judged by.
	"presence_stale_after_s",
	// CR-FEAT-035: {pending, leased, oldest_age_s} from the serving store, or
	// null for a store that cannot report a depth.
	"queue_depth",
	"rate_limit_per_minute",
	"registry_backend",
	"require_agent_signature",
	"webhook_signing",
}

// statusSentinel* are the distinctive values this file puts in every secret
// slot. If any of them reaches the wire, the non-disclosure assertions name it.
const (
	statusSentinelAuthToken   = "STATUS-SENTINEL-AUTH-TOKEN-3f9c"
	statusSentinelDBUser      = "status-sentinel-db-user"
	statusSentinelDBPassword  = "STATUS-SENTINEL-DB-PASSWORD-7ab1"
	statusSentinelDBURL       = "postgres://" + statusSentinelDBUser + ":" + statusSentinelDBPassword + "@status-sentinel-db-host.invalid:5432/crier"
	statusSentinelWebhook     = "STATUS-SENTINEL-WEBHOOK-SECRET-5e02"
	statusSentinelFedToken    = "STATUS-SENTINEL-FED-TOKEN-c48d"
	statusSentinelGuardURL    = "https://status-sentinel-guard-host.invalid/v1"
	statusSentinelQueueFile   = "/tmp/status-sentinel-federation-queue.json"
	statusSentinelFedLink     = "http://status-sentinel-link.invalid:8767"
	statusSentinelWrongBearer = "STATUS-SENTINEL-WRONG-BEARER-11aa"
)

// statusNoLeakValues is every value that must not appear anywhere in a /status
// response body, whatever the configuration. It includes the credential
// components (user, host, database path) rather than only the whole URL, so a
// redaction that keeps a recognizable fragment still fails.
var statusNoLeakValues = []string{
	statusSentinelAuthToken,
	statusSentinelDBURL,
	statusSentinelDBUser,
	statusSentinelDBPassword,
	"status-sentinel-db-host.invalid",
	statusSentinelWebhook,
	statusSentinelFedToken,
	statusSentinelGuardURL,
	"status-sentinel-guard-host.invalid",
	statusSentinelQueueFile,
}

// httptestRecorder drives the REAL handler (newStatusHandler, exactly as run()
// constructs it) over a recorder and returns the raw body. It is the seam that
// keeps the builder tests free of sockets and of the environment: the handler
// is handed an explicit Config and the backend that was selected for it.
func httptestRecorder(t *testing.T, cfg config.Config, backend string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	newStatusHandler(cfg, backend)(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status (recorder): status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /status (recorder): Content-Type %q, want application/json", ct)
	}
	return rec.Body.Bytes()
}

func scrubStatusEnv(t *testing.T) {
	t.Helper()
	for _, k := range statusScrubbedEnv {
		t.Setenv(k, "")
	}
}

// bootStatusServer boots the real server in-process via run(nil) with the
// scrubbed environment plus the case's overrides, waits until it answers
// /health, and registers the same SIGTERM shutdown the package's other live
// tests use. It returns the base URL.
func bootStatusServer(t *testing.T, env map[string]string) string {
	t.Helper()

	scrubStatusEnv(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	port := freePort(t)
	t.Setenv("CRIER_PORT", strconv.Itoa(port))

	done := make(chan struct{})
	// exitCode carries run()'s return value (CI-018). The send happens BEFORE
	// the deferred close(done) a waiter observes, so whenever this server has
	// exited, its code is already buffered here.
	exitCode := make(chan int, 1)
	go func() {
		defer close(done)
		exitCode <- run(nil)
	}()

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find own process: %v", err)
	}
	t.Cleanup(func() {
		_ = self.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("server did not shut down within 10s of SIGTERM")
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}

	// /health is public on every configuration, so it is the readiness probe.
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 20s: %v", err)
		}
		select {
		case <-done:
			t.Fatalf("server exited before answering /health on port %d: run() exit code %d (last error: %v)", port, runExitCode(exitCode), err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	return baseURL
}

// getStatus performs GET /status with an optional Bearer token and returns the
// status code, the Content-Type and the raw body. The raw bytes are returned
// (not just the decoded value) because the non-disclosure assertions are about
// what is ON THE WIRE.
func getStatus(t *testing.T, baseURL, bearer string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/status", nil)
	if err != nil {
		t.Fatalf("build GET /status request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /status body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// decodeStatus decodes a /status body and fails the test on invalid JSON.
func decodeStatus(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET /status: body %q is not valid JSON: %v", body, err)
	}
	return got
}

// assertNoSentinelLeak fails when any sentinel value appears in the body. It is
// asserted on the RAW body, so a leak cannot hide behind JSON escaping.
func assertNoSentinelLeak(t *testing.T, body []byte) {
	t.Helper()
	raw := string(body)
	for _, secret := range statusNoLeakValues {
		if strings.Contains(raw, secret) {
			t.Errorf("GET /status leaked %q; body:\n%s", secret, raw)
		}
	}
}

// assertStatusSchema checks the exact top-level key set and returns the decoded
// map for field assertions.
func assertStatusSchema(t *testing.T, body []byte) map[string]any {
	t.Helper()
	got := decodeStatus(t, body)

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string(nil), statusTopLevelKeys...)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("GET /status top-level keys = %v, want %v (the documented schema; update docs/openapi.yaml and this list together)", keys, want)
	}
	return got
}

// assertStatusBuild checks the nested build object against the resolved
// identity GET /version serves — the two endpoints must not disagree.
func assertStatusBuild(t *testing.T, got map[string]any) {
	t.Helper()
	build, ok := got["build"].(map[string]any)
	if !ok {
		t.Fatalf("GET /status: build = %#v, want an object", got["build"])
	}
	want := buildinfo.Resolve()
	for _, field := range []struct {
		key  string
		want any
	}{
		{"version", want.Version},
		{"commit", want.Commit},
		{"build_time", want.BuildTime},
		{"modified", want.Modified},
	} {
		if build[field.key] != field.want {
			t.Errorf("GET /status: build.%s = %#v, want %#v (the identity GET /version serves)", field.key, build[field.key], field.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. the decision seam
// ---------------------------------------------------------------------------

// TestRegistryBackendForURLIsTheSelectionRule pins the helper run() selects the
// store with AND the helper GET /status reports through. One input, one answer:
// an empty URL means the in-memory store, any URL means PostgreSQL. Because
// both call sites read this value, a change here changes what is built and what
// is advertised together — the property that makes the reported backend
// trustworthy without a live database.
func TestRegistryBackendForURLIsTheSelectionRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"unset selects the in-memory store", "", registryBackendMemory},
		{"CR_DATABASE_URL selects PostgreSQL", statusSentinelDBURL, registryBackendPostgres},
		{"any non-empty URL selects PostgreSQL", "postgresql://other-host/other-db", registryBackendPostgres},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryBackendForURL(tc.url); got != tc.want {
				t.Errorf("registryBackendForURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestStatusReportsTheSelectedBackend proves the value the builder serializes is
// the one it was GIVEN — i.e. the one run() selected the store with — not a
// value recomputed from a different input. A postgres URL with a postgres
// backend and a memory URL with a memory backend are both covered, and the
// postgres case needs no database because it never opens one.
func TestStatusReportsTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"memory", ""},
		{"postgres", statusSentinelDBURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Database: config.DatabaseConfig{URL: tc.url}}
			backend := registryBackendForURL(cfg.Database.URL)

			rec := httptestRecorder(t, cfg, backend)
			got := assertStatusSchema(t, rec)
			if got["registry_backend"] != backend {
				t.Errorf("registry_backend = %#v, want %q (the backend the store was selected with)", got["registry_backend"], backend)
			}
			assertNoSentinelLeak(t, rec)
		})
	}
}

// ---------------------------------------------------------------------------
// 2. the pure builder: defaults, modes, and non-disclosure
// ---------------------------------------------------------------------------

// TestStatusDefaultsFromConfigLoad is the default-posture claim, taken from the
// production defaults rather than from a hand-written Config literal: with a
// scrubbed environment, auth is DISABLED (no token), per-agent signatures are
// REQUIRED (secure by default), the guard is ON, the registry is in-memory,
// federation and webhook signing are off, and neither inspection surface is
// registered.
func TestStatusDefaultsFromConfigLoad(t *testing.T) {
	scrubStatusEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	backend := registryBackendForURL(cfg.Database.URL)
	body := httptestRecorder(t, cfg, backend)
	got := assertStatusSchema(t, body)

	for _, want := range []struct {
		key  string
		want any
	}{
		{"auth_enabled", false},
		{"require_agent_signature", true},
		// DF-CRIER-287: the mesh default is the DOCUMENTED permissive one —
		// authentication off, every origin allowed — and the endpoint has to
		// say so, because that is the posture an operator auditing a live
		// server must not have to guess.
		{"mesh_auth_required", false},
		{"mesh_origin_policy", config.MeshOriginPolicyAllowAll},
		{"guard_enabled", true},
		{"registry_backend", registryBackendMemory},
		{"rate_limit_per_minute", float64(100)},
		{"log_level", "info"},
		{"log_format", "text"},
		{"webhook_signing", false},
		{"federation_enabled", false},
		{"federation_hold_queue", federationHoldQueueNone},
		{"metrics_enabled", false},
		{"pprof_enabled", false},
	} {
		if got[want.key] != want.want {
			t.Errorf("default posture: %s = %#v, want %#v", want.key, got[want.key], want.want)
		}
	}
	assertStatusBuild(t, got)
	assertNoSentinelLeak(t, body)
}

// TestStatusFederationHoldQueueModes covers the three reachable durability
// modes of the federation hold path (DF-CRIER-7). The PATH itself is never in
// the body — only the mode.
func TestStatusFederationHoldQueueModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		links    []string
		queueFie string
		wantMode string
		wantOn   bool
	}{
		{"no links configured", nil, "", federationHoldQueueNone, false},
		{"links without a queue file hold in memory", []string{statusSentinelFedLink}, "", federationHoldQueueMemory, true},
		{"links with a queue file hold durably", []string{statusSentinelFedLink}, statusSentinelQueueFile, federationHoldQueueFile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Federation: config.FederationConfig{
				Links:     tc.links,
				Token:     statusSentinelFedToken,
				QueueFile: tc.queueFie,
			}}
			body := httptestRecorder(t, cfg, registryBackendMemory)
			got := assertStatusSchema(t, body)

			if got["federation_enabled"] != tc.wantOn {
				t.Errorf("federation_enabled = %#v, want %v", got["federation_enabled"], tc.wantOn)
			}
			if got["federation_hold_queue"] != tc.wantMode {
				t.Errorf("federation_hold_queue = %#v, want %q", got["federation_hold_queue"], tc.wantMode)
			}
			assertNoSentinelLeak(t, body)
		})
	}
}

// TestStatusNeverSerializesSecrets is the non-disclosure gate: a configuration
// with EVERY secret-bearing field populated still produces a body that reports
// the effective booleans (so the test cannot pass on an empty config) and
// contains no credential, connection string, token, secret, base URL or config
// path anywhere in it.
func TestStatusNeverSerializesSecrets(t *testing.T) {
	cfg := config.Config{
		Port:               8767,
		AuthToken:          statusSentinelAuthToken,
		RequireAgentSig:    true,
		RequireMeshAuth:    true,
		MeshAllowedOrigins: "https://status-sentinel-console.invalid",
		RateLimitPerMinute: 100,
		LogLevel:           "debug",
		LogFormat:          "json",
		Database: config.DatabaseConfig{
			URL: statusSentinelDBURL,
		},
		Webhook: config.WebhookConfig{Secret: statusSentinelWebhook},
		Federation: config.FederationConfig{
			Links:     []string{statusSentinelFedLink},
			Token:     statusSentinelFedToken,
			QueueFile: statusSentinelQueueFile,
		},
		Guard: config.GuardConfig{
			Enabled:         true,
			DeepSeekBaseURL: statusSentinelGuardURL,
			Model:           "status-sentinel-guard-model",
		},
		Observability: config.ObservabilityConfig{EnableMetrics: true, EnablePProf: true},
	}

	body := httptestRecorder(t, cfg, registryBackendForURL(cfg.Database.URL))
	got := assertStatusSchema(t, body)

	// The effective posture IS reported…
	for _, want := range []struct {
		key  string
		want any
	}{
		{"auth_enabled", true},
		{"require_agent_signature", true},
		{"mesh_auth_required", true},
		{"mesh_origin_policy", config.MeshOriginPolicyAllowlist},
		{"guard_enabled", true},
		{"registry_backend", registryBackendPostgres},
		{"webhook_signing", true},
		{"federation_enabled", true},
		{"federation_hold_queue", federationHoldQueueFile},
		{"metrics_enabled", true},
		{"pprof_enabled", true},
		{"log_level", "debug"},
		{"log_format", "json"},
	} {
		if got[want.key] != want.want {
			t.Errorf("%s = %#v, want %#v", want.key, got[want.key], want.want)
		}
	}
	// …and nothing else is.
	assertNoSentinelLeak(t, body)
}

// ---------------------------------------------------------------------------
// 3. the live server: routing, defaults, and auth
// ---------------------------------------------------------------------------

// TestStatusEndpointServed is the end-to-end gate: on a running server with the
// production defaults, GET /status answers 200 application/json, reports the
// default posture (auth off, signatures required, guard on, in-memory registry)
// and carries the same build identity /version serves.
func TestStatusEndpointServed(t *testing.T) {
	baseURL := bootStatusServer(t, nil)

	status, ctype, body := getStatus(t, baseURL, "")
	if status != http.StatusOK {
		t.Fatalf("GET /status: status %d, want 200 (auth is disabled on this boot)", status)
	}
	if ctype != "application/json" {
		t.Errorf("GET /status: Content-Type %q, want application/json", ctype)
	}

	got := assertStatusSchema(t, body)
	for _, want := range []struct {
		key  string
		want any
	}{
		{"auth_enabled", false},
		{"require_agent_signature", true},
		{"guard_enabled", true},
		{"registry_backend", registryBackendMemory},
		{"federation_hold_queue", federationHoldQueueNone},
	} {
		if got[want.key] != want.want {
			t.Errorf("live default posture: %s = %#v, want %#v", want.key, got[want.key], want.want)
		}
	}
	assertStatusBuild(t, got)
	assertNoSentinelLeak(t, body)
}

// TestStatusAuthRequired is the auth-contract gate: with CR_AUTH_TOKEN set,
// /status is NOT exempt — no Bearer is 401, a wrong Bearer is 401, the right
// Bearer is 200 and reports auth_enabled=true. The same boot proves /health and
// /version are still public, i.e. the change did not touch the exempt list.
func TestStatusAuthRequired(t *testing.T) {
	const token = statusSentinelAuthToken

	baseURL := bootStatusServer(t, map[string]string{
		"CR_AUTH_TOKEN":     token,
		"CR_WEBHOOK_SECRET": statusSentinelWebhook,
		"CR_FED_TOKEN":      statusSentinelFedToken,
	})

	status, _, _ := getStatus(t, baseURL, "")
	if status != http.StatusUnauthorized {
		t.Errorf("GET /status without a Bearer token: status %d, want 401 (not auth-exempt)", status)
	}
	status, _, _ = getStatus(t, baseURL, statusSentinelWrongBearer)
	if status != http.StatusUnauthorized {
		t.Errorf("GET /status with a wrong Bearer token: status %d, want 401", status)
	}

	status, ctype, body := getStatus(t, baseURL, token)
	if status != http.StatusOK {
		t.Fatalf("GET /status with the configured Bearer token: status %d, want 200; body:\n%s", status, body)
	}
	if ctype != "application/json" {
		t.Errorf("GET /status: Content-Type %q, want application/json", ctype)
	}

	got := assertStatusSchema(t, body)
	if got["auth_enabled"] != true {
		t.Errorf("auth_enabled = %#v, want true (CR_AUTH_TOKEN is set)", got["auth_enabled"])
	}
	if got["webhook_signing"] != true {
		t.Errorf("webhook_signing = %#v, want true (CR_WEBHOOK_SECRET is set)", got["webhook_signing"])
	}
	assertStatusBuild(t, got)
	assertNoSentinelLeak(t, body)

	// The exempt list is unchanged: both public surfaces still answer a
	// token-less request on an auth-enabled server.
	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/health", "/version"} {
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a token: status %d, want 200 (%s is auth-exempt)", path, resp.StatusCode, path)
		}
	}
}

// TestStatusLiveNeverExposesGuardBaseURL drives the live path with the guard's
// provider configuration set: the guard switch is reported, the base URL and
// the queue path are not, and the CR_GUARD_MODEL value (which is deliberately
// not part of the schema) does not appear either.
func TestStatusLiveNeverExposesGuardBaseURL(t *testing.T) {
	baseURL := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":           "true",
		"CR_GUARD_DEEPSEEK_BASE_URL": statusSentinelGuardURL,
		"CR_GUARD_MODEL":             "status-sentinel-guard-model",
		"CR_GUARD_KANBAN_URL":        "http://status-sentinel-kanban.invalid:9090",
		"CR_FED_QUEUE_FILE":          statusSentinelQueueFile,
	})

	status, _, body := getStatus(t, baseURL, "")
	if status != http.StatusOK {
		t.Fatalf("GET /status: status %d, want 200", status)
	}
	got := assertStatusSchema(t, body)
	if got["guard_enabled"] != true {
		t.Errorf("guard_enabled = %#v, want true", got["guard_enabled"])
	}
	// The hold queue is unconfigured here (no CR_FED_LINKS), so the mode is
	// "none" even though a queue path is set — the path is not posture.
	if got["federation_hold_queue"] != federationHoldQueueNone {
		t.Errorf("federation_hold_queue = %#v, want %q (no links configured)", got["federation_hold_queue"], federationHoldQueueNone)
	}
	assertNoSentinelLeak(t, body)
	for _, forbidden := range []string{"status-sentinel-guard-model", "status-sentinel-kanban.invalid"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("GET /status leaked %q; body:\n%s", forbidden, body)
		}
	}
}
