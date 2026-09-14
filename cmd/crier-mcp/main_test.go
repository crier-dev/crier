package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
)

// TestMCPServerInitialize is an entrypoint smoke test: it builds and starts
// crier-mcp as a subprocess, sends a JSON-RPC initialize request over stdin,
// and verifies the response carries a result with serverInfo. If someone
// breaks the wiring in main.go (config load, store init, mcp.New, Serve),
// this test fails immediately.
func TestMCPServerInitialize(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crier-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CR_AUTH_TOKEN=",
		"CR_LOG_LEVEL=error",
		// Force the in-memory store regardless of the dev env.
		"CR_DATABASE_URL=",
		"DATABASE_URL=",
		"CRIER_DATABASE_URL=",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start crier-mcp: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n"
	if _, err := stdin.Write([]byte(initReq)); err != nil {
		t.Fatalf("write initialize request: %v", err)
	}

	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		r := bufio.NewReader(stdout)
		line, err := r.ReadBytes('\n')
		readCh <- readResult{line, err}
	}()

	var line []byte
	select {
	case res := <-readCh:
		if res.err != nil {
			t.Fatalf("read initialize response: %v", res.err)
		}
		line = res.line
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for initialize response")
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		Result  *struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error any `json:"error"`
		ID    int `json:"id"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal initialize response %q: %v", line, err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %s", line)
	}
	if resp.Result == nil {
		t.Fatalf("initialize response missing result: %s", line)
	}
	if resp.Result.ServerInfo.Name != "crier-mcp" {
		t.Fatalf("result.serverInfo.name: got %q, want %q", resp.Result.ServerInfo.Name, "crier-mcp")
	}
	if resp.Result.ServerInfo.Version == "" {
		t.Fatal("result.serverInfo.version is empty")
	}
	if resp.Result.ProtocolVersion == "" {
		t.Fatal("result.protocolVersion is empty")
	}

	// Close stdin; Serve's scan loop should terminate and the process exit 0.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("crier-mcp exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crier-mcp did not exit after stdin close")
	}
}

// TestParseArgs exercises the CLI flag parsing directly (no exec, no server
// startup). The --help and --version paths are the CR-GAP-017 hard gate.
func TestParseArgs(t *testing.T) {
	envVars := []string{
		"CR_DATABASE_URL",
		"CR_AUTH_TOKEN",
		"CR_REQUIRE_AGENT_SIG",
		"CR_LOG_LEVEL",
		"CR_LOG_FORMAT",
	}

	t.Run("help exits 0 and documents env vars", func(t *testing.T) {
		for _, args := range [][]string{{"--help"}, {"-help"}, {"-h"}} {
			var out strings.Builder
			help, showVersion, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if !help {
				t.Fatalf("parseArgs(%v): help = false, want true", args)
			}
			if showVersion {
				t.Fatalf("parseArgs(%v): version = true, want false", args)
			}
			usage := out.String()
			for _, want := range append([]string{"Usage:", "crier-mcp [flags]", "-version"}, envVars...) {
				if !strings.Contains(usage, want) {
					t.Errorf("parseArgs(%v): usage output missing %q", args, want)
				}
			}
		}
	})

	t.Run("version", func(t *testing.T) {
		for _, args := range [][]string{{"--version"}, {"-version"}} {
			var out strings.Builder
			help, showVersion, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if help {
				t.Fatalf("parseArgs(%v): help = true, want false", args)
			}
			if !showVersion {
				t.Fatalf("parseArgs(%v): version = false, want true", args)
			}
		}
	})

	t.Run("no args starts normally", func(t *testing.T) {
		var out strings.Builder
		help, showVersion, err := parseArgs(nil, &out)
		if err != nil {
			t.Fatalf("parseArgs(nil) error: %v", err)
		}
		if help || showVersion {
			t.Fatalf("parseArgs(nil): help=%v version=%v, want both false", help, showVersion)
		}
	})

	t.Run("unknown flag errors", func(t *testing.T) {
		var out strings.Builder
		_, _, err := parseArgs([]string{"--bogus"}, &out)
		if err == nil {
			t.Fatal("parseArgs(--bogus): err = nil, want error")
		}
		if !strings.Contains(out.String(), "bogus") {
			t.Errorf("parseArgs(--bogus): output %q does not mention the unknown flag", out.String())
		}
	})
}

// TestMCPServerCLIFlags is the CR-GAP-017 acceptance test on the real binary:
// --help prints usage and exits 0 within 1s without starting the MCP server,
// and --version prints a version string and exits 0.
func TestMCPServerCLIFlags(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crier-mcp")
	// Read HEAD before building and again after: a sibling worker may commit
	// while the build runs, and either revision is a legitimate stamp.
	headBefore := gitShortHead(t)
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	t.Run("--help exits 0 within 1s with usage", func(t *testing.T) {
		start := time.Now()
		out, err := exec.Command(bin, "--help").CombinedOutput()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("--help took %v, want <= 1s", elapsed)
		}
		if err != nil {
			t.Fatalf("--help exited with error: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "Usage:") || !strings.Contains(string(out), "crier-mcp [flags]") {
			t.Fatalf("--help output missing usage text:\n%s", out)
		}
	})

	t.Run("--version exits 0 with version string", func(t *testing.T) {
		start := time.Now()
		out, err := exec.Command(bin, "--version").CombinedOutput()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("--version took %v, want <= 1s", elapsed)
		}
		if err != nil {
			t.Fatalf("--version exited with error: %v\n%s", err, out)
		}
		if !strings.HasPrefix(string(out), "crier-mcp ") {
			t.Fatalf("--version output %q does not start with %q", out, "crier-mcp ")
		}

		// DF-CRIER-127: the printed identity must carry a real commit, not
		// the "dev" placeholder that shipped before. Canonical form:
		// "crier-mcp v<version>-<commit>[-dirty]".
		identity := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "crier-mcp"))
		match := mcpIdentityRE.FindStringSubmatch(identity)
		if match == nil {
			t.Fatalf("--version identity %q is not the canonical form %q", identity, "v<version>-<commit>[-dirty]")
		}
		commit := match[1]
		switch {
		case headBefore == "":
			t.Logf("git unavailable: cannot tie commit %q to a repo revision", commit)
		case commit != headBefore:
			if headAfter := gitShortHead(t); commit != headAfter {
				t.Errorf("--version reports commit %q, which is neither HEAD (%s) nor the revision HEAD moved to (%s)",
					commit, headBefore, headAfter)
			}
		}
	})
}

// mcpIdentityRE matches the canonical identity printed by -version: a version
// segment (which may itself contain dashes, e.g. a git-describe string) and a
// shortened commit, with the optional dirty marker.
var mcpIdentityRE = regexp.MustCompile(`^v.+?-([0-9a-f]{8})(-dirty)?$`)

// gitShortHead returns the first 8 characters of the repository HEAD, or ""
// when git is unavailable (the caller then says so instead of failing).
func gitShortHead(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// writeAgentKeyPEM writes a PKCS#8 PEM ed25519 private key (the
// `openssl genpkey -algorithm ED25519` format) to a temp file and returns the
// path plus the key.
func writeAgentKeyPEM(t *testing.T, dir string) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	path := filepath.Join(dir, "agent.key")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path, priv
}

// TestInitStoreRemoteSigningKey covers the remote-mode startup wiring:
// CRIER_AGENT_PRIVATE_KEY_FILE loads a PKCS#8 PEM ed25519 key and enables
// signing; the variable's absence stays unsigned; and unreadable, malformed,
// non-PKCS#8, and non-ed25519 key files are explicit startup errors that
// never echo key material.
func TestInitStoreRemoteSigningKey(t *testing.T) {
	setEnv := func(t *testing.T, key, val string) {
		t.Helper()
		if val == "" {
			t.Setenv(key, "")
		} else {
			t.Setenv(key, val)
		}
	}

	t.Run("valid key file enables signing end-to-end", func(t *testing.T) {
		// Real registry handler in the secure default configuration; the
		// store produced by initStore must be able to retrieve against it.
		store := registry.NewMemoryStore()
		h := registry.NewHandler(store)
		h.SetRequireAgentSig(true)
		r := mux.NewRouter()
		r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
		r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
		r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
		r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
		r.HandleFunc("/agents/{id}/inbox/stats", h.HandleStats).Methods(http.MethodGet)
		r.HandleFunc("/agents/{id}", h.HandleUnregister).Methods(http.MethodDelete)
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)

		dir := t.TempDir()
		keyPath, priv := writeAgentKeyPEM(t, dir)
		pub := priv.Public().(ed25519.PublicKey)

		// Register the agent + public key (the README bootstrap step).
		body := fmt.Sprintf(`{"id":"mcp-agent","public_key":%q,"capabilities":["demo"]}`, hex.EncodeToString(pub))
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/agents", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build register request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register status = %d", resp.StatusCode)
		}

		setEnv(t, "CRIER_HTTP_URL", srv.URL)
		setEnv(t, "CRIER_AGENT_ID", "mcp-agent")
		setEnv(t, "CRIER_AUTH_TOKEN", "")
		setEnv(t, "CRIER_AGENT_PRIVATE_KEY_FILE", keyPath)

		rs, _, cleanup, err := initStore(config.Config{})
		if err != nil {
			t.Fatalf("initStore: %v", err)
		}
		defer cleanup()
		remote, ok := rs.(*registry.RemoteStore)
		if !ok {
			t.Fatalf("store type = %T, want *registry.RemoteStore", rs)
		}

		// Deliver then retrieve: retrieve is signature-gated, so a 200 with
		// the message proves the loaded key was actually wired in and every
		// request carries a verifying signature.
		if err := remote.Deliver("mcp-agent", &registry.InboxEntry{Payload: json.RawMessage(`{"kind":"ping"}`)}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		entries, leaseID, err := remote.Retrieve("mcp-agent", 30*time.Second, 5)
		if err != nil {
			t.Fatalf("signed Retrieve via initStore: %v", err)
		}
		if len(entries) != 1 || leaseID == "" {
			t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
		}
		if err := remote.Ack("mcp-agent", leaseID, []string{entries[0].ID}); err != nil {
			t.Fatalf("signed Ack via initStore: %v", err)
		}
	})

	t.Run("unset key file generates an ephemeral signing key that signs", func(t *testing.T) {
		// A signed server (the secure default). The bridge has no key file,
		// so it must mint an ephemeral key and sign with it.
		store := registry.NewMemoryStore()
		h := registry.NewHandler(store)
		h.SetRequireAgentSig(true)
		r := mux.NewRouter()
		r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
		r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
		r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
		r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)

		setEnv(t, "CRIER_HTTP_URL", srv.URL)
		setEnv(t, "CRIER_AGENT_ID", "ephemeral-agent")
		setEnv(t, "CRIER_AUTH_TOKEN", "")
		setEnv(t, "CRIER_AGENT_PRIVATE_KEY_FILE", "")

		got, identity, cleanup, err := initStore(config.Config{})
		if err != nil {
			t.Fatalf("initStore: %v", err)
		}
		defer cleanup()
		remote, ok := got.(*registry.RemoteStore)
		if !ok {
			t.Fatalf("store type = %T, want *registry.RemoteStore", got)
		}
		if !identity.Remote || identity.AgentID != "ephemeral-agent" {
			t.Fatalf("identity = %+v, want Remote=true AgentID=ephemeral-agent", identity)
		}
		if identity.KeySource != "ephemeral" {
			t.Fatalf("KeySource = %q, want %q", identity.KeySource, "ephemeral")
		}
		if len(identity.PublicKey) != ed25519.PublicKeySize {
			t.Fatalf("ephemeral PublicKey len = %d, want %d", len(identity.PublicKey), ed25519.PublicKeySize)
		}

		// Register the ephemeral public key, then exercise a signature-gated
		// route: a 200 proves the generated key is actually wired into the
		// store's signing path (not just held in the identity struct).
		res, err := remote.EnsureRegistered("ephemeral-agent", identity.PublicKey, nil)
		if err != nil || !res.Created {
			t.Fatalf("EnsureRegistered = %+v, %v", res, err)
		}
		if err := remote.Deliver("ephemeral-agent", &registry.InboxEntry{Payload: json.RawMessage(`{"kind":"ping"}`)}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		entries, leaseID, err := remote.Retrieve("ephemeral-agent", 30*time.Second, 5)
		if err != nil {
			t.Fatalf("signed Retrieve with the ephemeral key: %v", err)
		}
		if len(entries) != 1 || leaseID == "" {
			t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
		}

		// A second initStore mints a DIFFERENT key: that is the documented
		// ephemeral-key consequence, and it is asserted so it cannot silently
		// become stable (which would make the mismatch path unreachable).
		_, identity2, cleanup2, err := initStore(config.Config{})
		if err != nil {
			t.Fatalf("second initStore: %v", err)
		}
		defer cleanup2()
		if identity2.PublicKey.Equal(identity.PublicKey) {
			t.Fatal("two ephemeral identities share a public key, want a fresh key per run")
		}
	})

	t.Run("missing file is an explicit startup error", func(t *testing.T) {
		setEnv(t, "CRIER_HTTP_URL", "http://localhost:8767")
		setEnv(t, "CRIER_AGENT_ID", "mcp-agent")
		setEnv(t, "CRIER_AGENT_PRIVATE_KEY_FILE", filepath.Join(t.TempDir(), "nope.key"))

		_, _, _, err := initStore(config.Config{})
		if err == nil || !strings.Contains(err.Error(), "CRIER_AGENT_PRIVATE_KEY_FILE") {
			t.Fatalf("err = %v, want CRIER_AGENT_PRIVATE_KEY_FILE failure", err)
		}
	})

	t.Run("garbage file is an explicit startup error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "garbage.key")
		if err := os.WriteFile(path, []byte("not a pem at all"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		setEnv(t, "CRIER_HTTP_URL", "http://localhost:8767")
		setEnv(t, "CRIER_AGENT_ID", "mcp-agent")
		setEnv(t, "CRIER_AGENT_PRIVATE_KEY_FILE", path)

		_, _, _, err := initStore(config.Config{})
		if err == nil || !strings.Contains(err.Error(), "no PEM data") {
			t.Fatalf("err = %v, want no-PEM failure", err)
		}
	})

	t.Run("non-ed25519 pkcs8 key rejected with no key material leaked", func(t *testing.T) {
		dir := t.TempDir()
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rsa key: %v", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := filepath.Join(dir, "rsa.key")
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		setEnv(t, "CRIER_HTTP_URL", "http://localhost:8767")
		setEnv(t, "CRIER_AGENT_ID", "mcp-agent")
		setEnv(t, "CRIER_AGENT_PRIVATE_KEY_FILE", path)

		_, _, _, err = initStore(config.Config{})
		if err == nil || !strings.Contains(err.Error(), "ed25519") {
			t.Fatalf("err = %v, want non-ed25519 failure", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") && strings.Contains(err.Error(), "-----BEGIN") {
			t.Fatal("error echoed key material")
		}
	})
}

// TestHelpDocumentsRemoteMode pins the remote-mode onboarding in the binary's
// --help: all four remote env variables (CRIER_HTTP_URL, CRIER_AGENT_ID,
// CRIER_AUTH_TOKEN, CRIER_AGENT_PRIVATE_KEY_FILE) plus the signing rules, the
// automatic bridge registration (DF-CRIER-28), the ephemeral-key fallback,
// the key-mismatch remedies, and the key-material-never-logged guarantee.
func TestHelpDocumentsRemoteMode(t *testing.T) {
	var out strings.Builder
	if _, _, err := parseArgs([]string{"--help"}, &out); err != nil {
		t.Fatalf("parseArgs(--help): %v", err)
	}
	usage := out.String()
	for _, want := range []string{
		"CRIER_HTTP_URL",
		"CRIER_AGENT_ID",
		"CRIER_AUTH_TOKEN",
		"CRIER_AGENT_PRIVATE_KEY_FILE",
		"CR_REQUIRE_AGENT_SIG=true",
		"CR_REQUIRE_AGENT_SIG=false",
		"never logged",
		"openssl genpkey",
		// Bridge registration documentation (DF-CRIER-28).
		"registers CRIER_AGENT_ID",
		"ephemeral ed25519 key is generated",
		"key-mismatch ERROR",
		"DELETE /agents/{id}",
		"never aborts startup",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage output missing %q", want)
		}
	}
}
