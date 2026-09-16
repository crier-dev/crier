package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/gorilla/mux"
)

// ---- logging capture ------------------------------------------------------

// logRecord is one captured slog record with its attributes flattened.
type logRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// captureHandler is an slog.Handler that records everything it receives, so
// tests can assert the exact registration outcome lines without touching the
// global logger or parsing stderr.
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, logRecord{Level: r.Level, Message: r.Message, Attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) all() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]logRecord, len(h.records))
	copy(out, h.records)
	return out
}

// find returns the first record at level whose message contains substr.
func (h *captureHandler) find(level slog.Level, substr string) (logRecord, bool) {
	for _, rec := range h.all() {
		if rec.Level == level && strings.Contains(rec.Message, substr) {
			return rec, true
		}
	}
	return logRecord{}, false
}

// joined drains every record into one text blob (level + message + attrs),
// for assertions that only care that something was said at all.
func (h *captureHandler) joined() string {
	var b strings.Builder
	for _, rec := range h.all() {
		b.WriteString(rec.Level.String())
		b.WriteString(" ")
		b.WriteString(rec.Message)
		for k, v := range rec.Attrs {
			b.WriteString(" ")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(v)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ---- identity fixtures ----------------------------------------------------

// newBridgeTestServer spins the real registry handler in signed mode over an
// in-memory store with the routes a bridge touches.
func newBridgeTestServer(t *testing.T) (*httptest.Server, *registry.MemoryStore) {
	t.Helper()
	store := registry.NewMemoryStore()
	h := registry.NewHandler(store)
	h.SetRequireAgentSig(true)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store
}

// bridgeKey mints an ed25519 keypair and returns the public half plus a
// RemoteStore signed with the private half.
func bridgeKey(t *testing.T, baseURL, agentID string) (ed25519.PublicKey, *registry.RemoteStore) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, registry.NewRemoteStore(baseURL, agentID, "", registry.WithSigningKey(priv))
}

// ---- ensureBridgeIdentity -------------------------------------------------

// TestEnsureBridgeIdentityLogsCreated covers the happy path on a fresh
// server: the identity is created and the outcome is an info line naming the
// agent id and the key source.
func TestEnsureBridgeIdentityLogsCreated(t *testing.T) {
	srv, store := newBridgeTestServer(t)
	pub, rs := bridgeKey(t, srv.URL, "bridge-1")
	h := &captureHandler{}
	id := bridgeIdentity{Remote: true, AgentID: "bridge-1", PublicKey: pub, KeySource: keySourceEphemeral}

	ensureBridgeIdentity(rs, id, slog.New(h))

	rec, ok := h.find(slog.LevelInfo, "registered bridge identity")
	if !ok {
		t.Fatalf("no info line about registration; got:\n%s", h.joined())
	}
	if rec.Attrs["agent_id"] != "bridge-1" {
		t.Fatalf("agent_id attr = %q, want bridge-1", rec.Attrs["agent_id"])
	}
	if rec.Attrs["key_source"] != keySourceEphemeral {
		t.Fatalf("key_source attr = %q, want %q", rec.Attrs["key_source"], keySourceEphemeral)
	}
	if _, err := store.Get("bridge-1"); err != nil {
		t.Fatalf("agent not on the server after registration: %v", err)
	}
}

// TestEnsureBridgeIdentityLogsExistedMatch covers the idempotent re-run: the
// identity is already there with the same key, so the log must say no action
// was needed and no error must be logged.
func TestEnsureBridgeIdentityLogsExistedMatch(t *testing.T) {
	srv, store := newBridgeTestServer(t)
	pub, rs := bridgeKey(t, srv.URL, "bridge-2")
	if err := store.Register(&registry.Agent{ID: "bridge-2", PublicKey: registry.HexKey(pub)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &captureHandler{}

	ensureBridgeIdentity(rs, bridgeIdentity{Remote: true, AgentID: "bridge-2", PublicKey: pub, KeySource: keySourceFile}, slog.New(h))

	if _, ok := h.find(slog.LevelInfo, "already registered"); !ok {
		t.Fatalf("no info line about the existing identity; got:\n%s", h.joined())
	}
	if _, ok := h.find(slog.LevelError, ""); ok {
		t.Fatalf("unexpected error line for a matching key:\n%s", h.joined())
	}
}

// TestEnsureBridgeIdentityLogsKeyMismatch is the case the brief calls out: the
// server holds a stale key. The caller must log an ERROR naming the
// consequence (401s) and BOTH remedies, and must not touch the registration.
func TestEnsureBridgeIdentityLogsKeyMismatch(t *testing.T) {
	srv, store := newBridgeTestServer(t)
	stalePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate stale key: %v", err)
	}
	if err := store.Register(&registry.Agent{ID: "bridge-3", PublicKey: registry.HexKey(stalePub)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pub, rs := bridgeKey(t, srv.URL, "bridge-3")
	h := &captureHandler{}

	ensureBridgeIdentity(rs, bridgeIdentity{Remote: true, AgentID: "bridge-3", PublicKey: pub, KeySource: keySourceEphemeral}, slog.New(h))

	rec, ok := h.find(slog.LevelError, "DIFFERENT public key")
	if !ok {
		t.Fatalf("no error line for the key mismatch; got:\n%s", h.joined())
	}
	if rec.Attrs["agent_id"] != "bridge-3" {
		t.Fatalf("agent_id attr = %q, want bridge-3", rec.Attrs["agent_id"])
	}
	if !strings.Contains(rec.Message, "401") {
		t.Fatalf("error message does not state the consequence: %q", rec.Message)
	}
	remedy := rec.Attrs["remedy"]
	for _, want := range []string{"CRIER_AGENT_PRIVATE_KEY_FILE", "DELETE /agents/bridge-3"} {
		if !strings.Contains(remedy, want) {
			t.Fatalf("remedy %q does not name %q", remedy, want)
		}
	}
	// Not repaired silently: the stale registration is untouched.
	got, err := store.Get("bridge-3")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if hex.EncodeToString(got.PublicKey) != hex.EncodeToString(stalePub) {
		t.Fatal("the bridge overwrote a mismatched registration, want it left alone")
	}
}

// TestEnsureBridgeIdentityLogsTransportFailure: an unreachable/failing server
// must produce a loud error naming the cause and must not panic or abort.
func TestEnsureBridgeIdentityLogsTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	pub, rs := bridgeKey(t, srv.URL, "bridge-4")
	h := &captureHandler{}

	ensureBridgeIdentity(rs, bridgeIdentity{Remote: true, AgentID: "bridge-4", PublicKey: pub, KeySource: keySourceFile}, slog.New(h))

	rec, ok := h.find(slog.LevelError, "registration FAILED")
	if !ok {
		t.Fatalf("no error line for the transport failure; got:\n%s", h.joined())
	}
	if rec.Attrs["error"] == "" {
		t.Fatalf("error line carries no cause: %+v", rec)
	}
	if !strings.Contains(rec.Attrs["error"], "500") {
		t.Fatalf("cause %q does not mention the status", rec.Attrs["error"])
	}
	if !strings.Contains(rec.Message, "agent not found") {
		t.Fatalf("error message does not name the consequence: %q", rec.Message)
	}
}

// TestEnsureBridgeIdentitySkipsNonRemote: the in-memory/Postgres backends and
// a missing agent id must be silent no-ops (no spurious server calls).
func TestEnsureBridgeIdentitySkipsNonRemote(t *testing.T) {
	h := &captureHandler{}
	logger := slog.New(h)

	ensureBridgeIdentity(registry.NewMemoryStore(), bridgeIdentity{AgentID: "bridge-5", KeySource: keySourceFile}, logger)
	ensureBridgeIdentity(registry.NewMemoryStore(), bridgeIdentity{Remote: true, KeySource: keySourceFile}, logger)
	if got := h.joined(); got != "" {
		t.Fatalf("expected silence, got:\n%s", got)
	}
}

// ---- run() wiring ---------------------------------------------------------

// TestRunSelfRegistersBridgeIdentity drives the real binary in remote mode
// against a signed test server: after startup the bridge identity must exist
// on the server (no operator step) and the stderr log must say so. This is
// the in-repo form of the DF-CRIER-28 live probe: it fails if run() stops
// calling ensureBridgeIdentity.
func TestRunSelfRegistersBridgeIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	srv, store := newBridgeTestServer(t)

	bin := filepath.Join(t.TempDir(), "crier-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CRIER_HTTP_URL="+srv.URL,
		"CRIER_AGENT_ID=bridge-run",
		"CRIER_AGENT_PRIVATE_KEY_FILE=",
		"CR_AUTH_TOKEN=",
		"CR_LOG_LEVEL=info",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// The registration happens before Serve, so the identity is on the
	// server as soon as the initialize response comes back.
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n")); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	stderrCh := make(chan string, 1)
	go func() {
		// The ephemeral-key notice is logged before the registration line,
		// so scan until the registration outcome appears.
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "registered bridge identity") {
				stderrCh <- sc.Text()
				return
			}
		}
		stderrCh <- ""
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := store.Get("bridge-run"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge identity was never registered on the server (agent %q missing)", "bridge-run")
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case line := <-stderrCh:
		if line == "" {
			t.Fatal("no registration line on stderr")
		}
		if !strings.Contains(line, "registered bridge identity") || !strings.Contains(line, "bridge-run") {
			t.Fatalf("stderr line %q does not announce the registration of bridge-run", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no registration line on stderr")
	}
}

// TestRunRegistersWithSuppliedKeyFile: with CRIER_AGENT_PRIVATE_KEY_FILE set
// the identity advertises the file key (key_source=file), and the public half
// on the server is that key's — not an ephemeral one.
func TestRunRegistersWithSuppliedKeyFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	srv, store := newBridgeTestServer(t)
	dir := t.TempDir()
	keyPath, priv := writeAgentKeyPEM(t, dir)
	wantHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))

	bin := filepath.Join(t.TempDir(), "crier-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CRIER_HTTP_URL="+srv.URL,
		"CRIER_AGENT_ID=bridge-file",
		"CRIER_AGENT_PRIVATE_KEY_FILE="+keyPath,
		"CR_AUTH_TOKEN=",
		"CR_LOG_LEVEL=info",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	stderrCh := make(chan string, 1)
	go func() {
		// The resolver's token INFO (DF-CRIER-195) is logged before the
		// registration line, so scan until the registration outcome
		// appears rather than reading only the first line.
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "registered bridge identity") {
				stderrCh <- sc.Text()
				return
			}
		}
		stderrCh <- ""
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if agent, err := store.Get("bridge-file"); err == nil {
			if hex.EncodeToString(agent.PublicKey) != wantHex {
				t.Fatalf("registered key = %s, want the file key %s", hex.EncodeToString(agent.PublicKey), wantHex)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge identity was never registered on the server")
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case line := <-stderrCh:
		if !strings.Contains(line, "key_source=file") || !strings.Contains(line, "bridge-file") {
			t.Fatalf("stderr line %q does not name bridge-file with key_source=file", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no registration line on stderr")
	}
}

// TestRunSurvivesRegistrationFailure: an unreachable server must NOT abort
// startup (the harness may already be running); the failure is logged at
// error level and initialize still answers. Pointing at a closed port plus a
// short client timeout keeps this fast.
func TestRunSurvivesRegistrationFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	// Reserve a port, then close it: nothing is listening.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	bin := filepath.Join(t.TempDir(), "crier-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CRIER_HTTP_URL="+deadURL,
		"CRIER_AGENT_ID=bridge-dead",
		"CRIER_AGENT_PRIVATE_KEY_FILE=",
		"CR_AUTH_TOKEN=",
		"CR_LOG_LEVEL=info",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	stderrCh := make(chan string, 1)
	go func() {
		var lines []string
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if strings.Contains(sc.Text(), "registration FAILED") {
				stderrCh <- strings.Join(lines, "\n")
				return
			}
		}
		stderrCh <- strings.Join(lines, "\n")
	}()

	select {
	case got := <-stderrCh:
		if !strings.Contains(got, "registration FAILED") {
			t.Fatalf("no loud registration failure on stderr:\n%s", got)
		}
		if !strings.Contains(got, "bridge-dead") {
			t.Fatalf("failure line does not name the agent id:\n%s", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the registration failure line")
	}

	// Startup was not aborted: the server still answers JSON-RPC.
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n")); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	lineCh := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadBytes('\n')
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		var resp struct {
			Result *json.RawMessage `json:"result"`
			Error  any              `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshal initialize response %q: %v", line, err)
		}
		if resp.Result == nil {
			t.Fatalf("initialize returned no result after a failed registration: %s", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("crier-mcp did not answer initialize after a registration failure")
	}
}
