package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

// Black-box production composition: no stream, direct store injection or signature
// verifier stub. Every refusal must leave an acknowledged task unanswered.
func TestA2APublishIdentityHTTP(t *testing.T) {
	parallelBoot(t)
	for _, tc := range []struct {
		name, signature, refusal, basis string
		enforce                         bool
	}{
		{name: "enforced spoof", enforce: true, signature: "other", refusal: "signature verification failed"},
		{name: "enforced missing", enforce: true, refusal: "missing agent signature headers"},
		{name: "enforced verified", enforce: true, signature: "owner", basis: "publisher-identity-verified"},
		{name: "enforced body bound", enforce: true, signature: "bound", basis: "publisher-identity-verified"},
		{name: "optional spoof", signature: "other", refusal: "signature verification failed"},
		{name: "optional verified", signature: "owner", basis: "publisher-identity-verified"},
		{name: "optional claimed", basis: "publisher-identity-claimed"},
		{name: "enforced stale signature", enforce: true, signature: "stale", refusal: "request timestamp outside allowed window"},
		{name: "optional empty signature", signature: "empty", refusal: "X-Agent-Sig is present but empty"},
		{name: "optional partial signature", signature: "partial", refusal: "missing agent signature headers"},
		{name: "tampered bound body", enforce: true, signature: "tampered", refusal: "body digest mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true", "CR_REQUIRE_AGENT_SIG": strconv.FormatBool(tc.enforce)})
			client := &http.Client{Timeout: 5 * time.Second}
			const owner = "identity-owner-372"
			priv := registerA2AAgent(t, base, owner, true, nil)
			other := registerA2AAgent(t, base, "identity-other-372", true, nil)
			task := sentTask(t, rpcResult(t, mustA2AOK(t, client, base, sendMessageBody(owner, a2a.MethodSendMessage, "identity-372"))))
			id := taskIDOf(t, task)
			code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+owner+"/inbox", owner, priv, nil)
			require.Equal(t, http.StatusOK, code, string(raw))
			var inbox struct {
				LeaseID string `json:"lease_id"`
			}
			require.NoError(t, json.Unmarshal(raw, &inbox))
			code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+owner+"/inbox/ack", owner, priv, []byte(fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, inbox.LeaseID, id)))
			require.Equal(t, http.StatusNoContent, code, string(raw))
			topic, ok := a2a.TaskTopic(id)
			require.True(t, ok)
			event := outputEnvelope(id, "identity answer")
			// Payload claims must never upgrade unsigned publisher evidence.
			event["metadata"] = map[string]any{"crier": map[string]any{"publisher_identity_basis": "publisher-identity-verified"}}
			body, err := json.Marshal(map[string]any{"topic": topic, "event": event})
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPost, base+"/relay/publish", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set(registry.HeaderAgentID, owner)
			req.Header.Set("Content-Type", "application/json")
			if tc.signature != "" {
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				if tc.signature == "stale" {
					ts = strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
				}
				payload := http.MethodPost + "\n/relay/publish\n" + ts
				req.Header.Set(registry.HeaderAgentTS, ts)
				if tc.signature == "bound" || tc.signature == "tampered" {
					sum := sha256.Sum256(body)
					if tc.signature == "tampered" {
						sum = sha256.Sum256([]byte("different body"))
					}
					digest := hex.EncodeToString(sum[:])
					req.Header.Set(registry.HeaderAgentBodySHA256, digest)
					payload += "\nsha256:" + digest
				}
				key := priv
				if tc.signature == "other" {
					key = other
				}
				sig := hex.EncodeToString(ed25519.Sign(key, []byte(payload)))
				if tc.signature == "empty" {
					sig = " "
				}
				req.Header.Set(registry.HeaderAgentSig, sig)
				if tc.signature == "partial" {
					req.Header.Del(registry.HeaderAgentTS)
				}
			}
			var conn *websocket.Conn
			if tc.refusal != "" {
				var wsResp *http.Response
				conn, wsResp, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/relay/subscribe/"+topic, nil)
				require.NoError(t, err)
				if wsResp != nil {
					require.NoError(t, wsResp.Body.Close())
				}
				t.Cleanup(func() { _ = conn.Close() })
				// Wait for the real subscription, not merely the upgrade response.
				require.Eventually(t, func() bool {
					resp, err := client.Get(base + "/relay/topics")
					if err != nil {
						return false
					}
					defer func() { _ = resp.Body.Close() }()
					var topics struct {
						Topics []struct {
							Name        string `json:"name"`
							Subscribers int    `json:"subscribers"`
						} `json:"topics"`
					}
					return json.NewDecoder(resp.Body).Decode(&topics) == nil && len(topics.Topics) == 1 && topics.Topics[0].Name == topic && topics.Topics[0].Subscribers == 1
				}, 5*time.Second, 10*time.Millisecond)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			raw, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			got := rpcResult(t, mustA2AOK(t, client, base, getTaskBody(owner, id, `"historyLength":0`)))
			meta := got["metadata"].(map[string]any)["crier"].(map[string]any)
			if tc.refusal != "" {
				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
				assert.Contains(t, string(raw), tc.refusal)
				assert.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, got))
				assert.Equal(t, false, meta["answered"])
				assert.NotContains(t, got, "artifacts")
				t.Logf("spoof/refusal HTTP %d %s; GetTask %s", resp.StatusCode, strings.TrimSpace(string(raw)), mustJSON361(t, got))
			} else {
				assert.Equal(t, http.StatusAccepted, resp.StatusCode, string(raw))
				assert.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, got))
				assert.Equal(t, true, meta["answered"])
				assert.Equal(t, tc.basis, meta["publisher_identity_basis"])
				assert.Contains(t, terminalStatusText(t, got), tc.basis)
				artifacts, present := got["artifacts"].([]any)
				if assert.True(t, present) && assert.Len(t, artifacts, 1) {
					artifact := artifacts[0].(map[string]any)
					assert.Equal(t, tc.basis, artifact["metadata"].(map[string]any)["crier"].(map[string]any)["publisher_identity_basis"])
				}
				t.Logf("GetTask %s", mustJSON361(t, got))
			}
			listing := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(owner, `"includeArtifacts":true,"historyLength":0`)))
			if _, present := got["artifacts"]; !present {
				got["artifacts"] = []any{}
			}
			assert.Equal(t, []map[string]any{got}, rpcTasks(t, listing))
			if conn != nil {
				control, err := json.Marshal(map[string]any{"topic": topic, "event": outputEnvelope(id, "authorized control")})
				require.NoError(t, err)
				code, raw = signedAs(t, client, http.MethodPost, base, "/relay/publish", owner, priv, control)
				require.Equal(t, http.StatusAccepted, code, string(raw))
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, frame, err := conn.ReadMessage()
				require.NoError(t, err)
				assert.Contains(t, string(frame), "authorized control", "a refused publish must not fan out")
				controlTask := rpcResult(t, mustA2AOK(t, client, base, getTaskBody(owner, id, `"historyLength":0`)))
				assert.Contains(t, mustJSON361(t, controlTask), "authorized control", "a refused publish must not become immutable first output")
			}
		})
	}
}
