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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func federation391HTML(t *testing.T, status []byte, want string) {
	t.Helper()
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to execute the shipped posture renderer")
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
			port := freePort(t)
			base := "http://127.0.0.1:" + strconv.Itoa(port)
			cmd := exec.Command(os.Args[0], "-test.run=^TestFederation391BootChild$")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "CR_") && !strings.HasPrefix(value, "CRIER_") && !strings.HasPrefix(value, "CRIER391_") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "CRIER391_CHILD=1", "CRIER_PORT="+strconv.Itoa(port),
				"CR_AUTH_TOKEN=posture-test-access", "CR_REQUIRE_AGENT_SIG=false", "CR_GUARD_ENABLED=false",
				"CR_REGISTRY_BACKEND=memory", "CR_SESSION_BACKEND=jsonl", "CR_SESSION_LOG_ROOT="+filepath.Join(dir, "sessions"),
				"CR_GROUP_ROOT="+filepath.Join(dir, "groups"), "CR_PIDFILE="+filepath.Join(dir, "server.pid"),
				"CR_LOG_FORMAT=text", "CR_LOG_LEVEL=info", "CR_FED_REQUIRE_PEER="+strconv.FormatBool(tc.required),
				"CR_FED_RECORDS_FILE="+records, "CR_FED_SELF_KEY_FILE="+keyfile)
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
			client := &http.Client{Timeout: 2 * time.Second}
			deadline := time.Now().Add(20 * time.Second)
			for {
				resp, err := client.Get(base + "/health")
				if err == nil {
					_ = resp.Body.Close()
					break
				}
				select {
				case err := <-done:
					t.Fatalf("server exited: %v (log %s)", err, logPath)
				default:
				}
				require.True(t, time.Now().Before(deadline), "server not ready: %v (log %s)", err, logPath)
				time.Sleep(25 * time.Millisecond)
			}
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
