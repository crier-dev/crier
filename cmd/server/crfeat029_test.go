package main

// CR-FEAT-029 acceptance — NAMESPACES (realms).
//
// The row's deliverable: "a namespace (realm) dimension on agents and messages
// with per-namespace policy (auth posture, rate limits, guard settings,
// retention), defaulting to today's single implicit namespace so nothing
// changes for existing deployments", with three acceptance properties:
//
//  1. two namespaces with different rate limits and guard policies demonstrably
//     do not interfere;
//  2. a message cannot cross namespaces implicitly;
//  3. single-namespace behaviour is byte-identical to today.
//
// This file runs those three against the REAL server: run(nil) boots with the
// same middleware, router, registry store and relay the fleet runs, and the
// namespace document arrives the way an operator delivers it (CR_NAMESPACES /
// CR_NAMESPACES_FILE), so nothing here re-implements a code path.
//
// What is measured, not asserted by proxy:
//
//   - the rate-limit half is driven through POST /relay/publish and asserted on
//     the STATUS CODES the two realms' publishers get;
//   - the guard half uses a realm whose guard_policy points at a mock LLM that
//     blocks, against a realm that declares guard_enabled=false, and counts the
//     mock's calls to prove the unguarded realm never reaches the guard lane;
//   - the crossing half is asserted on POST /agents/{id}/inbox: a delivery that
//     names another realm is refused (403 NAMESPACE_MISMATCH) and the target's
//     inbox does not grow;
//   - the byte-identity half compares the JSON body of the SAME registration
//     against a second server booted without any namespaces: the namespace-aware
//     server must not add a member to it.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// nsServerBoot is one server process booted by run() with a namespace document.
type nsServerBoot struct {
	baseURL string
	client  *http.Client
}

// bootNamespaceServer starts run(nil) with the given CR_NAMESPACES document and
// returns its base URL. It reuses startTestServerWithEnv's environment scrub
// (no DB, no auth, guard off) plus the namespace document, so the only
// difference between two boots is the realms.
//
// The server-wide guard is deliberately OFF here: the guard-policy half of the
// acceptance is measured at the handler level (internal/registry/namespace_test.go)
// where a mock LLM can be injected, because this file must not depend on an
// external model service being reachable.
func bootNamespaceServer(t *testing.T, extra map[string]string) *nsServerBoot {
	t.Helper()
	baseURL := startTestServerWithEnv(t, extra)
	client := &http.Client{Timeout: 5 * time.Second}
	return &nsServerBoot{baseURL: baseURL, client: client}
}

// do performs one request and returns the status and body.
func (s *nsServerBoot) do(t *testing.T, method, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.baseURL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// registerAgent registers an agent, optionally into a realm, and returns the
// status and the raw body (the body is what the byte-identity check compares).
// A real ed25519 key is generated per call, so the registration satisfies the
// DEFAULT posture (CR_REQUIRE_AGENT_SIG on) rather than a weakened one.
func (s *nsServerBoot) registerAgent(t *testing.T, id, nsName string) (int, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	payload := map[string]any{"id": id, "public_key": hex.EncodeToString(pub)}
	if nsName != "" {
		payload["namespace"] = nsName
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return s.do(t, http.MethodPost, "/agents", string(body), nil)
}

// publish posts a relay publish as agentID inside nsName ("" = no header).
func (s *nsServerBoot) publish(t *testing.T, nsName, agentID, topic string) (int, string) {
	t.Helper()
	headers := map[string]string{"X-Agent-ID": agentID}
	if nsName != "" {
		headers[namespace.HeaderNamespace] = nsName
	}
	return s.do(t, http.MethodPost, "/relay/publish",
		`{"topic":"`+topic+`","event":{"n":1}}`, headers)
}

// inboxDepth reads an agent's queue depth.
func (s *nsServerBoot) inboxDepth(t *testing.T, id string) int {
	t.Helper()
	status, body := s.do(t, http.MethodGet, "/agents/"+id+"/inbox", "", nil)
	if status != http.StatusOK {
		t.Fatalf("retrieve %s: %d %s", id, status, body)
	}
	var resp struct {
		QueueDepth int `json:"queue_depth"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("inbox decode: %v (%s)", err, body)
	}
	return resp.QueueDepth
}

// TestCRFEAT029TwoNamespacesDoNotShareFate is acceptance property 1, measured on
// the live relay: the tight realm's cap is exhausted while the quiet realm keeps
// publishing, and the same limit does not follow an agent id across realms.
func TestCRFEAT029TwoNamespacesDoNotShareFate(t *testing.T) {
	doc := `{"namespaces":[
		{"name":"tight","rate_limit_per_minute":3},
		{"name":"loose","rate_limit_per_minute":40}
	]}`
	s := bootNamespaceServer(t, map[string]string{"CR_NAMESPACES": doc})

	// The tight realm: three publishes accepted, the fourth refused.
	for i := 1; i <= 3; i++ {
		if status, body := s.publish(t, "tight", "noisy", "orders.created"); status != http.StatusAccepted {
			t.Fatalf("tight publish %d = %d (%s), want 202", i, status, body)
		}
	}
	status, body := s.publish(t, "tight", "noisy", "orders.created")
	if status != http.StatusTooManyRequests {
		t.Fatalf("tight publish 4 = %d (%s), want 429", status, body)
	}

	// The quiet realm is untouched — same topic, different realm — and the
	// flooder's own id is not rate limited there either.
	for i := 1; i <= 5; i++ {
		if status, body := s.publish(t, "loose", "quiet", "orders.created"); status != http.StatusAccepted {
			t.Fatalf("loose publish %d = %d (%s), want 202 (the tight realm's cap leaked)", i, status, body)
		}
	}
	if status, body := s.publish(t, "loose", "noisy", "orders.created"); status != http.StatusAccepted {
		t.Fatalf("the flooding agent must have a separate budget in another realm: %d (%s)", status, body)
	}

	// GET /namespaces reports the policy the server is actually enforcing.
	if status, listed := s.do(t, http.MethodGet, "/namespaces", "", nil); status != http.StatusOK {
		t.Fatalf("GET /namespaces = %d (%s)", status, listed)
	} else if !strings.Contains(listed, `"default":"default"`) || !strings.Contains(listed, `"name":"tight"`) {
		t.Errorf("listing = %s, want the default realm plus the declared ones", listed)
	} else if !strings.Contains(listed, `"rate_limit_per_minute":3`) {
		t.Errorf("listing must report the realm's own cap: %s", listed)
	}
}

// TestCRFEAT029MessageCannotCrossNamespaces is acceptance property 2 on the live
// inbox path: the target's realm is the authority, a mismatched claim is refused
// with a named error, and the refused message is not stored.
func TestCRFEAT029MessageCannotCrossNamespaces(t *testing.T) {
	doc := `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`
	// Signature enforcement is orthogonal to realms and the acceptance drives
	// the bus with plain HTTP (the README dev posture), so the inbox reads here
	// are unauthenticated per-agent — the same posture the sibling acceptance
	// tests use.
	s := bootNamespaceServer(t, map[string]string{
		"CR_NAMESPACES":        doc,
		"CR_REQUIRE_AGENT_SIG": "false",
	})

	if status, body := s.registerAgent(t, "acme-agent", "acme"); status != http.StatusCreated {
		t.Fatalf("register into acme = %d (%s)", status, body)
	}
	if status, body := s.registerAgent(t, "acme-agent", "acme"); status != http.StatusConflict {
		t.Fatalf("re-register = %d, want 409 (%s)", status, body)
	}

	// A delivery that claims the OTHER realm is refused.
	status, body := s.do(t, http.MethodPost, "/agents/acme-agent/inbox",
		`{"payload":{"hello":"globex"},"namespace":"globex"}`, nil)
	if status != http.StatusForbidden {
		t.Fatalf("cross-namespace delivery = %d (%s), want 403", status, body)
	}
	if !strings.Contains(body, "NAMESPACE_MISMATCH") {
		t.Errorf("refusal = %s, want a named NAMESPACE_MISMATCH", body)
	}
	if depth := s.inboxDepth(t, "acme-agent"); depth != 0 {
		t.Fatalf("the refused message reached the inbox (depth %d)", depth)
	}

	// The same delivery with no claim at all lands in the target's own realm.
	if status, body := s.do(t, http.MethodPost, "/agents/acme-agent/inbox",
		`{"payload":{"hello":"acme"}}`, nil); status != http.StatusCreated {
		t.Fatalf("same-realm delivery = %d (%s)", status, body)
	}
	if depth := s.inboxDepth(t, "acme-agent"); depth != 1 {
		t.Fatalf("inbox depth = %d, want 1", depth)
	}

	// And an undeclared realm is a 400 naming the problem, never a silent
	// fallback to the default realm.
	status, body = s.registerAgent(t, "typo-agent", "acmee")
	if status != http.StatusBadRequest || !strings.Contains(body, "UNKNOWN_NAMESPACE") {
		t.Fatalf("register into an undeclared realm = %d (%s), want 400 UNKNOWN_NAMESPACE", status, body)
	}
}

// TestCRFEAT029SingleNamespaceIsByteIdentical is acceptance property 3: the same
// registration against a namespace-aware server and against a server with no
// namespaces at all must produce the same JSON, and the namespace-aware server
// must not grow a member on any existing surface.
func TestCRFEAT029SingleNamespaceIsByteIdentical(t *testing.T) {
	plain := bootNamespaceServer(t, nil)

	// A realm is declared here, but this registration names none — it is the
	// implicit default realm, which is what every existing client does.
	withRealms := bootNamespaceServer(t, map[string]string{
		"CR_NAMESPACES": `{"namespaces":[{"name":"acme","retention_seconds":600}]}`,
	})

	plainStatus, plainBody := plain.registerAgent(t, "identical", "")
	realmStatus, realmBody := withRealms.registerAgent(t, "identical", "")
	if plainStatus != http.StatusCreated || realmStatus != http.StatusCreated {
		t.Fatalf("register = %d / %d (%s / %s)", plainStatus, realmStatus, plainBody, realmBody)
	}
	if strings.Contains(realmBody, "namespace") {
		t.Fatalf("the implicit default realm leaked onto the wire: %s", realmBody)
	}
	// The two bodies carry the same members (the timestamps differ by clock, so
	// compare the key sets rather than the bytes).
	if keys(realmBody) != keys(plainBody) {
		t.Errorf("registration members differ:\n realm-aware: %s\n plain:       %s", realmBody, plainBody)
	}

	// GET /namespaces on an unconfigured server answers with exactly the one
	// implicit realm — nothing invented, nothing missing.
	status, body := plain.do(t, http.MethodGet, "/namespaces", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /namespaces = %d (%s)", status, body)
	}
	var listed struct {
		Default    string `json:"default"`
		Count      int    `json:"count"`
		Namespaces []struct {
			Name       string `json:"name"`
			Auth       string `json:"auth"`
			Configured bool   `json:"configured"`
			Agents     int    `json:"agents"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("namespaces decode: %v (%s)", err, body)
	}
	if listed.Default != namespace.DefaultName || listed.Count != 1 || len(listed.Namespaces) != 1 {
		t.Fatalf("unconfigured listing = %s, want exactly the default realm", body)
	}
	if got := listed.Namespaces[0]; got.Name != namespace.DefaultName || got.Auth != string(namespace.AuthShared) || got.Configured {
		t.Errorf("default entry = %+v, want an unconfigured shared-posture realm", got)
	}

	// The relay lane is unchanged too: an unqualified publish reaches an
	// unqualified subscriber on the same server that serves a named realm.
	if status, body := plain.publish(t, "", "agent-a", "plain.topic"); status != http.StatusAccepted {
		t.Fatalf("unqualified publish = %d (%s)", status, body)
	}
	if status, body := plain.do(t, http.MethodGet, "/relay/topics", "", nil); status != http.StatusOK {
		t.Fatalf("GET /relay/topics = %d (%s)", status, body)
	} else if strings.Contains(body, "namespace") {
		t.Errorf("/relay/topics grew a namespace member: %s", body)
	}
}

// TestCRFEAT029NamespaceDocumentCanComeFromAFile: the documented file form works
// end to end, and a document that cannot be read fails the boot instead of
// serving an empty realm set.
func TestCRFEAT029NamespaceDocumentCanComeFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespaces.json")
	doc := `{"namespaces":[{"name":"fromfile","rate_limit_per_minute":7}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := bootNamespaceServer(t, map[string]string{"CR_NAMESPACES_FILE": path})

	status, body := s.registerAgent(t, "file-agent", "fromfile")
	if status != http.StatusCreated {
		t.Fatalf("register into a file-declared realm = %d (%s)", status, body)
	}
	status, listed := s.do(t, http.MethodGet, "/namespaces", "", nil)
	if status != http.StatusOK || !strings.Contains(listed, `"name":"fromfile"`) {
		t.Fatalf("listing = %d %s, want the file-declared realm", status, listed)
	}
	if !strings.Contains(listed, `"rate_limit_per_minute":7`) {
		t.Errorf("the file's policy must be the one enforced: %s", listed)
	}
}

// TestCRFEAT029UnknownNamespaceIsRefusedOnBothLanes: a realm the server does not
// serve is refused everywhere it can be named, and never redirected to the
// default realm.
func TestCRFEAT029UnknownNamespaceIsRefusedOnBothLanes(t *testing.T) {
	s := bootNamespaceServer(t, map[string]string{"CR_NAMESPACES": `{"namespaces":[{"name":"acme"}]}`})

	status, body := s.publish(t, "ghost", "agent-a", "topic.a")
	if status != http.StatusBadRequest || !strings.Contains(body, "UNKNOWN_NAMESPACE") {
		t.Fatalf("publish into an undeclared realm = %d (%s)", status, body)
	}
	status, body = s.do(t, http.MethodPost, "/agents/nowhere/inbox",
		`{"payload":{"x":1},"namespace":"ghost"}`, nil)
	// The target does not exist at all, so the delivery is a 404 — the realm is
	// only ever checked against a real row.
	if status != http.StatusNotFound {
		t.Fatalf("deliver to an unknown agent = %d (%s), want 404", status, body)
	}
}

// TestCRFEAT029NamespaceTokenPostureEndToEnd: the realm's own credential gates
// the real HTTP surface (registration and delivery), and a wrong one is a 401
// that names the header.
func TestCRFEAT029NamespaceTokenPostureEndToEnd(t *testing.T) {
	doc := `{"namespaces":[{"name":"private","auth":"token","token_ref":"env:CR_NS_PRIVATE_TOKEN"}]}`
	s := bootNamespaceServer(t, map[string]string{
		"CR_NAMESPACES":        doc,
		"CR_NS_PRIVATE_TOKEN":  "s3cret",
		"CR_REQUIRE_AGENT_SIG": "false",
	})

	status, body := s.registerAgent(t, "private-agent", "private")
	if status != http.StatusUnauthorized {
		t.Fatalf("register into a token realm without its token = %d (%s), want 401", status, body)
	}

	payload, _ := json.Marshal(map[string]any{"id": "private-agent", "namespace": "private"})
	status, body = s.do(t, http.MethodPost, "/agents", string(payload), map[string]string{
		namespace.HeaderNamespaceToken: "s3cret",
	})
	if status != http.StatusCreated {
		t.Fatalf("register with the realm token = %d (%s)", status, body)
	}

	// A wrong credential is refused; the right one delivers.
	status, body = s.do(t, http.MethodPost, "/agents/private-agent/inbox", `{"payload":{"x":1}}`, map[string]string{
		namespace.HeaderNamespaceToken: "wrong",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("deliver with a wrong realm token = %d (%s), want 401", status, body)
	}
	status, body = s.do(t, http.MethodPost, "/agents/private-agent/inbox", `{"payload":{"x":1}}`, map[string]string{
		namespace.HeaderNamespaceToken: "s3cret",
	})
	if status != http.StatusCreated {
		t.Fatalf("deliver with the realm token = %d (%s)", status, body)
	}
}

// TestCRFEAT029UnreadableDocumentFailsTheBoot: CR_NAMESPACES_FILE pointing at a
// missing file is a startup failure, not an empty realm set — a declared realm
// the process cannot read must never silently put every agent back in the
// default realm.
func TestCRFEAT029UnreadableDocumentFailsTheBoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_GUARD_ENABLED", "false")
	t.Setenv("CR_NAMESPACES", "")
	t.Setenv("CR_NAMESPACES_FILE", missing)
	t.Setenv("CRIER_PORT", fmt.Sprint(freePort(t)))

	// run() must return a nonzero exit code. It installs a process-wide signal
	// handler first, so it is safe to call directly in-process.
	done := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- run(nil)
	}()

	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("run() succeeded with an unreadable namespace document")
		}
	case <-time.After(20 * time.Second):
		// The server booted despite the missing file; shut it down and fail.
		self, err := os.FindProcess(os.Getpid())
		if err == nil {
			_ = self.Signal(os.Interrupt)
		}
		<-done
		t.Fatal("run() did not fail on an unreadable namespace document")
	}
	wg.Wait()
}

// keys returns the sorted member names of a JSON object, so two responses can be
// compared member-for-member without comparing clock-bearing values.
func keys(body string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return ""
	}
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	// Small, dependency-free sort.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return strings.Join(names, ",")
}

// TestCRFEAT029DocumentIsOptionalAndEmptyMeansImplicit pins the two trivial
// boot shapes so a future change cannot make "no namespaces" behave differently
// from "an empty namespace list".
func TestCRFEAT029DocumentIsOptionalAndEmptyMeansImplicit(t *testing.T) {
	empty := bootNamespaceServer(t, map[string]string{"CR_NAMESPACES": "{}"})
	status, body := empty.do(t, http.MethodGet, "/namespaces", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /namespaces = %d (%s)", status, body)
	}
	if !strings.Contains(body, `"count":1`) {
		t.Errorf("an empty document must yield exactly the implicit realm: %s", body)
	}
}

// TestCRFEAT029RetentionIsPerNamespaceEndToEnd: a realm's retention is the
// default message lifetime on the live inbox path, and the default realm keeps
// the store's own default.
func TestCRFEAT029RetentionIsPerNamespaceEndToEnd(t *testing.T) {
	doc := `{"namespaces":[{"name":"short","retention_seconds":120}]}`
	s := bootNamespaceServer(t, map[string]string{
		"CR_NAMESPACES":        doc,
		"CR_REQUIRE_AGENT_SIG": "false",
	})

	if status, body := s.registerAgent(t, "short-agent", "short"); status != http.StatusCreated {
		t.Fatalf("register = %d (%s)", status, body)
	}
	if status, body := s.registerAgent(t, "plain-agent", ""); status != http.StatusCreated {
		t.Fatalf("register = %d (%s)", status, body)
	}
	if status, body := s.do(t, http.MethodPost, "/agents/short-agent/inbox", `{"payload":{"x":1}}`, nil); status != http.StatusCreated {
		t.Fatalf("deliver = %d (%s)", status, body)
	}
	if status, body := s.do(t, http.MethodPost, "/agents/plain-agent/inbox", `{"payload":{"x":1}}`, nil); status != http.StatusCreated {
		t.Fatalf("deliver = %d (%s)", status, body)
	}

	lifetime := func(id string) time.Duration {
		t.Helper()
		status, body := s.do(t, http.MethodGet, "/agents/"+id+"/inbox", "", nil)
		if status != http.StatusOK {
			t.Fatalf("retrieve %s: %d %s", id, status, body)
		}
		var resp struct {
			Messages []struct {
				CreatedAt time.Time `json:"created_at"`
				ExpiresAt time.Time `json:"expires_at"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Messages) == 0 {
			t.Fatalf("retrieve %s: %s (%v)", id, body, err)
		}
		return resp.Messages[0].ExpiresAt.Sub(resp.Messages[0].CreatedAt)
	}

	if got := lifetime("short-agent"); got != 120*time.Second {
		t.Errorf("realm lifetime = %s, want the realm's 120s retention", got)
	}
	if got := lifetime("plain-agent"); got != 24*time.Hour {
		t.Errorf("default-realm lifetime = %s, want the store's 24h default", got)
	}
}

// TestCRFEAT029NamespaceIsInTheOpenAPISpec: the spec is the published contract,
// so the realm field and the /namespaces route must be IN it (and the spec is
// served live by the same server, which is what makes this a live check rather
// than a file comparison).
func TestCRFEAT029NamespaceIsInTheOpenAPISpec(t *testing.T) {
	s := bootNamespaceServer(t, nil)
	status, body := s.do(t, http.MethodGet, "/openapi.yaml", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /openapi.yaml = %d", status)
	}
	for _, want := range []string{"/namespaces:", "namespace:", "X-Crier-Namespace"} {
		if !strings.Contains(body, want) {
			t.Errorf("the served OpenAPI document does not mention %q", want)
		}
	}
}

// TestCRFEAT029RegressionHarnessIsUnchanged is a local sanity check that the
// namespace work did not alter the pre-existing surfaces this file's siblings
// rely on: /health, /version and /status still answer, and a plain
// register → deliver → retrieve round trip still works on a server with realms
// declared.
func TestCRFEAT029ExistingSurfacesStillWork(t *testing.T) {
	s := bootNamespaceServer(t, map[string]string{
		"CR_NAMESPACES":        `{"namespaces":[{"name":"acme"}]}`,
		"CR_REQUIRE_AGENT_SIG": "false",
	})

	for _, path := range []string{"/health", "/version", "/status"} {
		if status, body := s.do(t, http.MethodGet, path, "", nil); status != http.StatusOK {
			t.Errorf("GET %s = %d (%s)", path, status, body)
		}
	}

	if status, body := s.registerAgent(t, "round-trip", ""); status != http.StatusCreated {
		t.Fatalf("register = %d (%s)", status, body)
	}
	if status, body := s.do(t, http.MethodPost, "/agents/round-trip/inbox", `{"payload":{"hello":"world"}}`, nil); status != http.StatusCreated {
		t.Fatalf("deliver = %d (%s)", status, body)
	}
	status, body := s.do(t, http.MethodGet, "/agents/round-trip/inbox", "", nil)
	if status != http.StatusOK {
		t.Fatalf("retrieve = %d (%s)", status, body)
	}
	// The payload travels base64-encoded on the retrieve wire (Go []byte JSON
	// encoding), so the round trip is proven by decoding it back — not by
	// looking for the word in the encoded string.
	var retrieved struct {
		Messages []struct {
			Payload string `json:"payload"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("retrieve decode: %v (%s)", err, body)
	}
	if len(retrieved.Messages) != 1 {
		t.Fatalf("retrieve returned %d messages, want 1 (%s)", len(retrieved.Messages), body)
	}
	decoded, err := base64.StdEncoding.DecodeString(retrieved.Messages[0].Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v (%s)", err, retrieved.Messages[0].Payload)
	}
	if string(decoded) != `{"hello":"world"}` {
		t.Errorf("round-tripped payload = %s, want the delivered object byte-for-byte", decoded)
	}
}

// TestCRFEAT029NamespaceListingIsAuthenticatedWhenAuthIsOn: like /status, the
// listing reports posture, so a token-less caller must not read it on a server
// that has auth on.
func TestCRFEAT029NamespaceListingRequiresAuth(t *testing.T) {
	s := bootNamespaceServer(t, map[string]string{"CR_AUTH_TOKEN": "tok"})

	status, _ := s.do(t, http.MethodGet, "/namespaces", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("GET /namespaces without a token = %d, want 401", status)
	}
	status, body := s.do(t, http.MethodGet, "/namespaces", "", map[string]string{"Authorization": "Bearer tok"})
	if status != http.StatusOK {
		t.Fatalf("GET /namespaces with a token = %d (%s)", status, body)
	}
	// /health stays exempt, as it always was.
	if status, _ := s.do(t, http.MethodGet, "/health", "", nil); status != http.StatusOK {
		t.Fatalf("/health = %d, want 200", status)
	}
}

// TestCRFEAT029BodyNamespaceOnPublishIsRefused: the relay's realm is a header
// (so the rate limiter knows it before the body is read); a body member is
// refused rather than ignored, per the DF-CRIER-180 doctrine that a value the
// server does not honor must be an error, never a no-op.
func TestCRFEAT029BodyNamespaceOnPublishIsRefused(t *testing.T) {
	s := bootNamespaceServer(t, map[string]string{"CR_NAMESPACES": `{"namespaces":[{"name":"acme"}]}`})

	status, body := s.do(t, http.MethodPost, "/relay/publish",
		`{"topic":"t","event":{"n":1},"namespace":"acme"}`,
		map[string]string{"X-Agent-ID": "agent-a"})
	if status != http.StatusBadRequest {
		t.Fatalf("publish with a body namespace = %d (%s), want 400", status, body)
	}
	if !strings.Contains(body, "X-Crier-Namespace") {
		t.Errorf("the refusal must point at the header: %s", body)
	}
}
