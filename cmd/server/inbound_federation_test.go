package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
)

func TestInbound371ProductionPolicyStatusAndReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, required string
		configured     bool
		want           int
	}{
		{"compatibility", "false", true, 201},
		{"strict", "true", true, 403},
		{"strict_empty_store_legacy", "true", false, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			der, err := x509.MarshalPKCS8PrivateKey(key)
			require.NoError(t, err)
			keyfile := filepath.Join(dir, "self.pem")
			require.NoError(t, os.WriteFile(keyfile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
			records := filepath.Join(dir, "peers.json")
			store, err := federation.OpenPeerStore(records)
			require.NoError(t, err)
			if tc.configured {
				_, err = store.Create(federation.PeerRecord{ID: "other", PublicKey: hex.EncodeToString(pub), URL: "https://other.example", Direction: "both"})
				require.NoError(t, err)
			}
			s := bootNamespaceServer(t, map[string]string{
				"CR_AUTH_TOKEN": "deployment", "CR_REQUIRE_AGENT_SIG": "false",
				"CR_REGISTRY_BACKEND": "memory", "CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": filepath.Join(dir, "sessions"), "CR_GROUP_ROOT": filepath.Join(dir, "groups"), "CR_PIDFILE": "",
				"CR_FED_REQUIRE_PEER": tc.required, "CR_FED_RECORDS_FILE": records, "CR_FED_SELF_KEY_FILE": keyfile,
				"CR_FED_LINKS": "", "CR_FED_AUTH_FILE": "", "CR_FED_PEERS_FILE": "", "CR_NAMESPACES": "", "CR_NAMESPACES_FILE": "",
			})
			headers := map[string]string{"Authorization": "Bearer deployment"}
			status, body := s.do(t, "POST", "/agents", `{"id":"target"}`, headers)
			require.Equal(t, 201, status, body)
			status, body = s.do(t, "GET", "/status", "", headers)
			require.Equal(t, 200, status, body)
			var posture map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &posture))
			require.Equal(t, tc.required == "true", posture["federation_require_peer"], "live posture does not match enforcement")
			for _, hop := range []string{"", "1"} {
				headers[federation.HopHeader] = hop
				status, body = s.do(t, "POST", "/agents/target/inbox", `{"payload":{"text":"inbound"}}`, headers)
				require.Equal(t, tc.want, status, body)
				if tc.configured {
					var receipt map[string]any
					require.NoError(t, json.Unmarshal([]byte(body), &receipt))
					want := string(federation.FedTokenOnly)
					if tc.want == 403 {
						want = string(federation.FedNoLink)
					}
					require.Equal(t, want, receipt["verdict"])
				}
				t.Logf("posture=%s hop=%q status=%d receipt=%s", tc.name, hop, status, body)
			}
			if tc.want == 403 {
				status, body = s.do(t, "GET", "/agents/target/inbox", "", headers)
				require.Equal(t, 200, status, body)
				require.Contains(t, body, `"queue_depth":0`)
			}
		})
	}
}
