package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/federation"
)

// A separate OS process exercises run's real logger and composition root, not a
// posture helper substituted for the middleware that actually receives traffic.
func TestFederation391BootChild(t *testing.T) {
	if os.Getenv("CRIER391_CHILD") != "1" {
		return
	}
	os.Exit(run(nil))
}

// federation391EphemeralPort is the ONLY port this file ever asks a child to
// bind: 0 makes the kernel hand the child its own port, and the parent learns
// the number from the pidfile the child writes only after that bind succeeded
// (waitForChildBoundPort). A number chosen here would sit in a probe-then-rebind
// window: any process on the host can take it, win the bind before the child
// does, and then answer /health with 200 — so every assertion below would be
// graded against a stranger (INT-CI-022, QA-CRIER-068).
const federation391EphemeralPort = 0

// federation391ScrubbedEnv is the parent environment minus every crier knob: a
// child gets exactly what federation391BootEnv lists, never a value inherited
// from the host or from a sibling test's t.Setenv.
func federation391ScrubbedEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "CR_") && !strings.HasPrefix(value, "CRIER_") && !strings.HasPrefix(value, "CRIER391_") {
			env = append(env, value)
		}
	}
	return env
}

// federation391PidPath is the one place the child's pidfile is named: the child
// writes it after its own listen succeeded and the handshake reads the port
// back out of the same file.
func federation391PidPath(dir string) string { return filepath.Join(dir, "server.pid") }

// federation391BootEnv is the environment a posture child boots with. The
// production boot and the blocked-boot probe build it here, so the probe proves
// something about the SAME boot shape the production test grades.
func federation391BootEnv(dir string, port int, required bool, records, keyfile string) []string {
	return []string{
		"CRIER391_CHILD=1",
		"CRIER_PORT=" + strconv.Itoa(port),
		"CR_AUTH_TOKEN=posture-test-access",
		"CR_REQUIRE_AGENT_SIG=false",
		"CR_GUARD_ENABLED=false",
		"CR_REGISTRY_BACKEND=memory",
		"CR_SESSION_BACKEND=jsonl",
		"CR_SESSION_LOG_ROOT=" + filepath.Join(dir, "sessions"),
		"CR_GROUP_ROOT=" + filepath.Join(dir, "groups"),
		"CR_PIDFILE=" + federation391PidPath(dir),
		"CR_LOG_FORMAT=text",
		"CR_LOG_LEVEL=info",
		"CR_FED_REQUIRE_PEER=" + strconv.FormatBool(required),
		"CR_FED_RECORDS_FILE=" + records,
		"CR_FED_SELF_KEY_FILE=" + keyfile,
	}
}

func federation391HTML(t *testing.T, status []byte, want string) {
	t.Helper()
	node := nodeOrSkip(t)
	source := string(chatClientHTML)
	start := strings.Index(source, "function renderPosture(b) {")
	end := strings.Index(source, "async function refresh() {")
	require.Greater(t, end, start)
	require.NotEqual(t, -1, start)
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`const assert = require('node:assert/strict');
const rendered = {};
const notice = (id, text) => { rendered[id] = text; };
` + source[start:end] + "\nrenderPosture(" + string(status) + ");\n" +
		fmt.Sprintf("assert.ok(rendered.posture.includes(%q), rendered.posture); console.log(rendered.posture);", want))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Logf("rendered posture: %s", strings.TrimSpace(string(output)))
}

func TestFederation391HTMLPosture(t *testing.T) {
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
	for _, tc := range []struct{ status, want string }{
		{`{"federation_require_peer_configured":true,"federation_require_peer_effective":false,"federation_peer_count":0,"federation_token_only_accepted":true}`, "Federation: require-peer configured but inert (0 peers) — token-only inbound accepted"},
		{`{"federation_require_peer_configured":true,"federation_require_peer_effective":true,"federation_peer_count":1,"federation_token_only_accepted":false}`, "Federation: strict (1 peer)"},
		{`{}`, "Federation: unknown"},
	} {
		t.Run(tc.want, func(t *testing.T) { federation391HTML(t, []byte(tc.status), tc.want) })
	}
}

func TestFederation391StatusWithoutGateCannotClaimEffective(t *testing.T) {
	cfg := config.Config{}
	cfg.Federation.RequirePeer = true
	// A configured URL is not a lifecycle record or a wired refusal gate.
	cfg.Federation.Links = []string{"https://unrelated.example"}
	w := httptest.NewRecorder()
	newStatusHandler(cfg, "memory")(w, httptest.NewRequest("GET", "/status", nil))
	var status map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status))
	require.Equal(t, true, status["federation_require_peer_configured"])
	require.Equal(t, false, status["federation_require_peer_effective"])
	require.Equal(t, float64(0), status["federation_peer_count"])
	require.Equal(t, true, status["federation_token_only_accepted"])
}

// ch:trace row=RELEASE-025 spec=none:release-flake-acceptance wave=none:serial-fallback test=cmd/server/federation_posture_test.go::TestFederation391ProductionBoots doc=none evidence=none:hosted-CI-pending witness=none:hosted-CI-pending-until-foreman verdict=none:foreman-judge commit=none:worker-sha-pending memory=none:foreman-write
func TestFederation391ProductionBoots(t *testing.T) {
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
	for _, tc := range []struct {
		name, state                string
		required, store, effective bool
	}{
		{"A_empty", "", true, true, false},
		{"B_linked", "linked", true, true, true},
		{"pending_record_also_arms_gate", "pending", true, true, true},
		{"revoked_record_also_arms_gate", "revoked", true, true, true},
		{"flag_off_with_peer", "linked", false, true, false},
		{"store_disabled", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			der, err := x509.MarshalPKCS8PrivateKey(key)
			require.NoError(t, err)
			keyfile, records := filepath.Join(dir, "self.pem"), filepath.Join(dir, "peers.json")
			require.NoError(t, os.WriteFile(keyfile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
			peers := 0
			if tc.state != "" {
				// Persist a valid lifecycle snapshot before boot. No external peer or
				// handshake network is needed to establish this gate's precondition.
				doc := map[string]any{"version": 1, "peers": []federation.PeerRecord{{
					ID: "unrelated", PublicKey: hex.EncodeToString(pub), KeyID: federation.KeyID(pub),
					URL: "https://unrelated.example", Direction: "both", State: tc.state,
				}}}
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(records, raw, 0600))
				peers = 1
			}
			if !tc.store {
				records, keyfile = "", ""
			}
			// QA-CRIER-068 (INT-CI-022): the child binds its OWN ephemeral
			// port and publishes it in the pidfile it writes only after that
			// bind succeeded, so this test never hands out a number a stranger
			// can steal between a probe and the child's listen. The pidfile
			// also names the child's pid, so waitForChildBoundPort can refuse
			// a port it cannot prove is ours — the assertions below can never
			// be answered by a foreign listener that returns 200 on /health.
			pidPath := federation391PidPath(dir)
			cmd := exec.Command(os.Args[0], "-test.run=^TestFederation391BootChild$")
			cmd.Env = append(federation391ScrubbedEnv(),
				federation391BootEnv(dir, federation391EphemeralPort, tc.required, records, keyfile)...)
			logPath := filepath.Join(dir, "boot.log")
			logfile, err := os.Create(logPath)
			require.NoError(t, err)
			cmd.Stdout, cmd.Stderr = logfile, logfile
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				_ = cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
				_ = logfile.Close()
			})
			// The port comes from the child's own pidfile — written only after
			// its listen succeeded — and the handshake refuses a port that does
			// not name THIS child, so the base URL cannot point at a stranger.
			port := waitForChildBoundPort(t, pidPath, cmd.Process.Pid, logPath)
			base := "http://127.0.0.1:" + strconv.Itoa(port)
			t.Logf("child pid %d bound port %d (from %s)", cmd.Process.Pid, port, pidPath)
			client := &http.Client{Timeout: 2 * time.Second}
			deadline := time.Now().Add(20 * time.Second)
			retry := time.NewTicker(25 * time.Millisecond)
			defer retry.Stop()
			for {
				resp, err := client.Get(base + "/health")
				if err == nil {
					_ = resp.Body.Close()
					break
				}
				select {
				case err := <-done:
					t.Fatalf("server exited: %v (log %s)", err, logPath)
				case <-retry.C:
				}
				require.True(t, time.Now().Before(deadline), "server not ready: %v (log %s)", err, logPath)
			}
			// /health proves that the listener is serving. Read the live posture
			// before exercising inbox delivery so lifecycle records (including
			// pending records) are observed as the gate's synchronization event,
			// rather than relying on startup timing.
			gateReq, err := http.NewRequest("GET", base+"/status", nil)
			require.NoError(t, err)
			gateReq.Header.Set("Authorization", "Bearer posture-test-access")
			gateResp, err := client.Do(gateReq)
			require.NoError(t, err)
			gateRaw, err := io.ReadAll(gateResp.Body)
			_ = gateResp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, gateResp.StatusCode, string(gateRaw))
			var gateStatus map[string]any
			require.NoError(t, json.Unmarshal(gateRaw, &gateStatus))
			require.Equal(t, tc.effective, gateStatus["federation_require_peer_effective"], string(gateRaw))
			require.Equal(t, float64(peers), gateStatus["federation_peer_count"], string(gateRaw))

			call := func(method, path, body string) (int, http.Header, []byte) {
				req, err := http.NewRequest(method, base+path, strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer posture-test-access")
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				raw, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				return resp.StatusCode, resp.Header, raw
			}
			code, _, raw := call("POST", "/agents", `{"id":"target"}`)
			require.Equal(t, 201, code, string(raw))
			code, header, raw := call("POST", "/agents/target/inbox", `{"payload":{"text":"inbound"}}`)
			wantCode, wantVerdict := 201, "FED_TOKEN_ONLY"
			if tc.effective {
				wantCode, wantVerdict = 403, "FED_NO_LINK"
			}
			require.Equal(t, wantCode, code, string(raw))
			require.Equal(t, wantVerdict, header.Get("X-Crier-Fed-Inbound-Verdict"))
			t.Logf("inbox status=%d X-Crier-Fed-Inbound-Verdict=%s", code, wantVerdict)
			if tc.effective {
				require.Contains(t, string(raw), "FED_NO_LINK")
			}
			code, _, raw = call("GET", "/status", "")
			require.Equal(t, 200, code)
			var status map[string]any
			require.NoError(t, json.Unmarshal(raw, &status))
			require.Equal(t, tc.required, status["federation_require_peer_configured"])
			require.Equal(t, tc.effective, status["federation_require_peer_effective"])
			require.Equal(t, float64(peers), status["federation_peer_count"])
			require.Equal(t, !tc.effective, status["federation_token_only_accepted"])
			// Legacy field stays a configuration alias, never silently renamed.
			require.Equal(t, tc.required, status["federation_require_peer"])
			require.NotContains(t, string(raw), "posture-test-access")
			require.NotContains(t, string(raw), hex.EncodeToString(pub))
			logs, err := os.ReadFile(logPath)
			require.NoError(t, err)
			fields := fmt.Sprintf("require_peer_configured=%t require_peer_effective=%t peers=%d token_only_accepted=%t", tc.required, tc.effective, peers, !tc.effective)
			require.Contains(t, string(logs), fields)
			for _, line := range bytes.Split(logs, []byte{'\n'}) {
				if bytes.Contains(line, []byte("federation inbound posture")) {
					t.Log(string(line))
				}
			}
			wantWords := "Federation: token-only inbound accepted (1 peer)"
			if tc.effective {
				wantWords = "Federation: strict (1 peer)"
			} else if tc.required {
				wantWords = "Federation: require-peer configured but inert (0 peers) — token-only inbound accepted"
			}
			federation391HTML(t, raw, wantWords)
			if tc.name == "A_empty" {
				// /status must sample the live gate, not freeze the boot's empty
				// count. The first operator-created pending record already arms it.
				body := fmt.Sprintf(`{"id":"first","public_key":%q,"url":"https://first.example","direction":"both"}`, hex.EncodeToString(pub))
				req, err := http.NewRequest("POST", base+"/fed/peers", strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer posture-test-access")
				req.Header.Set("X-Crier-Fed-Operator", "posture-test-access")
				resp, err := client.Do(req)
				require.NoError(t, err)
				_ = resp.Body.Close()
				require.Equal(t, 201, resp.StatusCode)
				code, _, raw = call("GET", "/status", "")
				require.Equal(t, 200, code)
				require.NoError(t, json.Unmarshal(raw, &status))
				require.Equal(t, true, status["federation_require_peer_effective"])
				require.Equal(t, float64(1), status["federation_peer_count"])
				code, header, raw = call("POST", "/agents/target/inbox", `{"payload":{"text":"after first peer"}}`)
				require.Equal(t, 403, code, string(raw))
				require.Equal(t, "FED_NO_LINK", header.Get("X-Crier-Fed-Inbound-Verdict"))
			}
		})
	}
}

// TestFederation391BlockedBootProbe is the EXPECTED-TO-FAIL half of
// TestFederation391ForeignListenerCannotBeGraded. It holds a port with a
// squatter that answers 200 on every path — exactly what a stolen port looks like
// from a parent — boots the production child against that port, and then runs the
// real handshake. The handshake must refuse, naming the child's own bind error,
// instead of returning a base URL for the squatter.
//
// Nothing here runs in a normal suite: the body returns unless the re-executing
// parent set CRIER391_BLOCKED_PROBE, and its failure IS the evidence the parent
// asserts on (an in-process t.Fatalf cannot be observed without failing the
// suite itself).
func TestFederation391BlockedBootProbe(t *testing.T) {
	if os.Getenv("CRIER391_BLOCKED_PROBE") != "1" {
		return
	}
	dir := t.TempDir()
	squat := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"federation_require_peer_effective":true,"store":"foreign-squatter"}`)
	})}
	// The child listens on ":<port>", so the stranger must hold every interface.
	squatLn, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	go func() { _ = squat.Serve(squatLn) }()
	t.Cleanup(func() { _ = squat.Close() })
	port := squatLn.Addr().(*net.TCPAddr).Port
	heldBase := "http://127.0.0.1:" + strconv.Itoa(port)
	// Premise 1: the squatter answers the readiness poll a trusting parent makes.
	resp, err := http.Get(heldBase + "/health")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Premise 2: nobody else can take the port either, so the child's bind fails.
	if other, err := net.Listen("tcp", ":"+strconv.Itoa(port)); err == nil {
		_ = other.Close()
		t.Fatalf("held port %d is still bindable on this host, so this probe cannot force a blocked child", port)
	}
	t.Logf("PROBE: squatter /health answered %d on held port %d, so a parent that trusted this number would have graded a stranger",
		resp.StatusCode, port)

	pidPath := federation391PidPath(dir)
	logPath := filepath.Join(dir, "blocked.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFederation391BootChild$")
	cmd.Env = append(federation391ScrubbedEnv(), federation391BootEnv(dir, port, true, "", "")...)
	logfile, err := os.Create(logPath)
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		var waitErr error
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case waitErr = <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-done
		}
		_ = waitErr // cleanup stop is best-effort; the probe's verdict is the handshake, not the exit status
		_ = logfile.Close()
	})
	// The assertion the probe exists for: the handshake must FAIL. The trailing
	// Fatalf is load-bearing — a handshake that returned here would hand the
	// parent a base URL for the squatter, which is the defect this replaces.
	waitForChildBoundPort(t, pidPath, cmd.Process.Pid, logPath)
	t.Fatalf("the handshake returned a port for a child that could not bind, so every later assertion "+
		"would be graded against the squatter on port %d", port)
}

// TestFederation391ForeignListenerCannotBeGraded is this file's red-first guard
// (QA-CRIER-068, INT-CI-022). It proves two independent things about the
// production boot:
//
//   - behaviour: a child that CANNOT bind — its port is held by a squatter that
//     answers /health with 200 — is reported as a NAMED bind failure instead of
//     being silently replaced by that listener. The guard runs the real handshake
//     in a re-executed binary that is EXPECTED to fail, because that failure is
//     the evidence.
//   - shape: the production boot asks for an ephemeral port and waits for the
//     child's own pidfile. A number this file chose is the defect: it can be
//     stolen between the probe and the child's bind, and the readiness poll then
//     grades whoever took it.
func TestFederation391ForeignListenerCannotBeGraded(t *testing.T) {
	t.Run("blocked_child_is_named_never_graded", func(t *testing.T) {
		probe := exec.Command(os.Args[0], "-test.run=^TestFederation391BlockedBootProbe$", "-test.v", "-test.timeout=180s")
		probe.Env = append(federation391ScrubbedEnv(), "CRIER391_BLOCKED_PROBE=1")
		out, err := probe.CombinedOutput()
		require.Error(t, err,
			"the blocked-boot probe PASSED, so a child that could not bind was silently replaced by a foreign "+
				"listener and every later assertion would grade it (INT-CI-022)\n%s", out)
		require.Contains(t, string(out), "PROBE: squatter /health answered 200",
			"the probe never established that the readiness poll was answerable by a stranger\n%s", out)
		require.Contains(t, string(out), "is gone before it published",
			"the refusal must be the handshake's named diagnosis, not a bare read error\n%s", out)
		require.Contains(t, string(out), "bind: address already in use",
			"the refusal must carry the child's own bind error, so the operator sees the cause\n%s", out)
	})
	t.Run("production_boot_asks_for_an_ephemeral_port", func(t *testing.T) {
		require.Equal(t, 0, federation391EphemeralPort,
			"the child's port request must be 0: only the kernel may pick it")
		body := federation391FuncBody(t, federation391Source(t), "TestFederation391ProductionBoots")
		require.Contains(t, body, "federation391EphemeralPort",
			"the production boot must ask for the ephemeral port: a port this test picks can be taken before the child binds")
		require.Contains(t, body, "port := waitForChildBoundPort(",
			"the production boot must take the child's port from its own pidfile — the one place it publishes the bind it won")
	})
}

// federation391Source returns this file's own source so the shape arm can assert
// what the production boot actually does. go test runs with the package dir as
// cwd, so the basename candidate resolves even for a -trimpath build; a source
// file that cannot be found FAILS the guard rather than silently passing.
func federation391Source(t *testing.T) string {
	t.Helper()
	candidates := []string{"federation_posture_test.go", filepath.Join("cmd", "server", "federation_posture_test.go")}
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		candidates = append([]string{thisFile}, candidates...)
	}
	for _, path := range candidates {
		if raw, err := os.ReadFile(path); err == nil {
			return string(raw)
		}
	}
	t.Fatalf("cannot read this file's own source (tried %v): the bound-port guard refuses to run unverified", candidates)
	return ""
}

// federation391FuncBody returns a top-level test function's source, from its
// `func` line to the first closing brace in column 0 (gofmt puts every inner
// brace inside the body), so an assertion about the production boot cannot be
// satisfied by an unrelated function in this file.
func federation391FuncBody(t *testing.T, source, name string) string {
	t.Helper()
	head := "func " + name + "(t *testing.T) {"
	start := strings.Index(source, head)
	require.NotEqual(t, -1, start, "%s is gone from this file: rename it and update the bound-port guard", name)
	rest := source[start:]
	end := strings.Index(rest, "\n}\n")
	require.NotEqual(t, -1, end, "%s has no column-0 closing brace, so the guard cannot delimit it", name)
	return rest[:end]
}
