package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/stretchr/testify/require"
)

// Two independent OS processes run production run(), exit on real SIGTERM,
// and share ONLY the registry file. No in-process store can satisfy this test.
func TestSQLiteTrustProcessRestart(t *testing.T) {
	rootDir := t.TempDir()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	leaf := ed25519.NewKeyFromSeed(seed)
	a := trust.ChainAnchor{ID: "restart-anchor", PublicKey: hex.EncodeToString(root.Public().(ed25519.PublicKey)), Label: "Restart operator"}
	d := trust.ChainDelegation{Voucher: a.ID, SubjectKey: hex.EncodeToString(leaf.Public().(ed25519.PublicKey)), Scope: []string{"message-signing"}, IssuedAt: time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.FixedZone("signed-offset", 3600))}
	trust.SignChainDelegation(&d, root)
	var firstPID int
	var originalAnchor trust.ChainAnchor
	for _, phase := range []string{"seed", "reopen"} {
		t.Run(phase, func(t *testing.T) {
			port := freePort(t)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteTrustServerChild$", "-test.v", "-test.timeout=40s")
			cmd.Env = append(os.Environ(), "CRIER368_ROOT="+rootDir, "CRIER368_PORT="+strconv.Itoa(port))
			log, err := os.Create(filepath.Join(rootDir, phase+".log"))
			require.NoError(t, err)
			defer func() { _ = log.Close() }()
			cmd.Stdout, cmd.Stderr = log, log
			require.NoError(t, cmd.Start())
			if phase == "seed" {
				firstPID = cmd.Process.Pid
			} else {
				require.NotEqual(t, firstPID, cmd.Process.Pid)
			}
			stopped := false
			defer func() {
				if !stopped {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			client := &http.Client{Timeout: time.Second}
			base := fmt.Sprintf("http://127.0.0.1:%d", port)
			require.Eventually(t, func() bool {
				resp, err := client.Get(base + "/health")
				if err != nil {
					return false
				}
				_ = resp.Body.Close()
				return resp.StatusCode == 200
			}, 20*time.Second, 25*time.Millisecond)
			call := func(method, path, token string, body any, want int) string {
				t.Helper()
				raw := ""
				if body != nil {
					b, err := json.Marshal(body)
					require.NoError(t, err)
					raw = string(b)
				}
				code, response := df297Request(t, client, method, base+path, token, "", raw)
				require.Equal(t, want, code, response)
				t.Logf("%s pid=%d %s %s -> %d %s", phase, cmd.Process.Pid, method, path, code, response)
				return response
			}
			if phase == "seed" {
				require.NoError(t, json.Unmarshal([]byte(call("POST", "/trust/anchors", df297AdminToken, a, 201)), &originalAnchor))
				call("POST", "/trust/delegations", df297AdminToken, d, 201)
			}
			var anchors struct {
				Anchors []trust.ChainAnchor `json:"anchors"`
			}
			require.NoError(t, json.Unmarshal([]byte(call("GET", "/trust/anchors", df297AdminToken, nil, 200)), &anchors))
			require.Equal(t, []trust.ChainAnchor{originalAnchor}, anchors.Anchors)
			var held struct {
				Delegations []trust.ChainDelegation `json:"delegations"`
			}
			require.NoError(t, json.Unmarshal([]byte(call("GET", "/trust/delegations?subject="+d.SubjectKey, df297AdminToken, nil, 200)), &held))
			wantRaw, err := json.Marshal([]trust.ChainDelegation{d})
			require.NoError(t, err)
			gotRaw, err := json.Marshal(held.Delegations)
			require.NoError(t, err)
			require.JSONEq(t, string(wantRaw), string(gotRaw))
			require.Equal(t, 123456789, held.Delegations[0].IssuedAt.Nanosecond())
			response := call("POST", "/trust/verify", df297MessageToken, map[string]any{"signer": d.SubjectKey, "scope": d.Scope, "chain": held.Delegations}, 200)
			require.JSONEq(t, `{"verdict":"TRUST_VERIFIED","anchor_id":"restart-anchor","depth":1}`, response)
			status := call("GET", "/status", df297MessageToken, nil, 200)
			require.Contains(t, status, `"registry_backend":"sqlite"`)
			// Target the CHILD PID explicitly, never broadcast to this test
			// process (the generic run-scoped lifecycle guard forbids that).
			require.NotEqual(t, os.Getpid(), cmd.Process.Pid)
			require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGTERM))
			err = cmd.Wait()
			stopped = true
			require.NoError(t, err)
			logs, err := os.ReadFile(filepath.Join(rootDir, phase+".log"))
			require.NoError(t, err)
			require.Contains(t, string(logs), "signal=terminated")
			require.Contains(t, string(logs), "signing trust tree enabled")
			require.Contains(t, string(logs), "registry_backend=sqlite")
			t.Logf("%s pid=%d SIGTERM -> exit 0", phase, cmd.Process.Pid)
		})
	}
}

func TestSQLiteTrustServerChild(t *testing.T) {
	root := os.Getenv("CRIER368_ROOT")
	if root == "" {
		return
	}
	for key, value := range map[string]string{
		"CRIER_PORT": os.Getenv("CRIER368_PORT"), "CR_REGISTRY_BACKEND": "sqlite", "CR_REGISTRY_SQLITE_PATH": filepath.Join(root, "trust.db"),
		"CR_DATABASE_URL": "", "DATABASE_URL": "", "CRIER_DATABASE_URL": "", "CR_GUARD_ENABLED": "false",
		"CR_AUTH_TOKEN": df297MessageToken, "CR_PERMISSIONS_ADMIN_TOKEN": df297AdminToken, "CR_PERMISSIONS_ENABLED": "false",
		"CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": filepath.Join(root, "sessions"), "CR_GROUP_ROOT": filepath.Join(root, "groups"), "CR_PIDFILE": "",
	} {
		t.Setenv(key, value)
	}
	require.Equal(t, 0, run(nil))
}
