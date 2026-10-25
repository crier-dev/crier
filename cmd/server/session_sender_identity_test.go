package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Production boot, real registered keys and independent transcript/inbox reads.
// No verifier stub: deleting the composition-root wiring must fail this test.
func TestSessionSenderIdentityHTTP(t *testing.T) {
	parallelBoot(t)
	for _, tc := range []struct {
		name, signature, sender, refusal, basis string
		enforce                                 bool
	}{
		{name: "enforced unsigned", enforce: true, refusal: "SESSION_SENDER_SIGNATURE_REQUIRED"},
		{name: "enforced verified", enforce: true, signature: "valid", basis: "sender-identity-verified"},
		{name: "enforced body bound", enforce: true, signature: "bound", basis: "sender-identity-verified"},
		{name: "enforced forged", enforce: true, signature: "forged", refusal: "SESSION_SENDER_SIGNATURE_INVALID"},
		{name: "enforced tampered body", enforce: true, signature: "tampered", refusal: "SESSION_SENDER_SIGNATURE_INVALID"},
		{name: "signed sender mismatch", enforce: true, signature: "valid", sender: "other", refusal: "SESSION_SENDER_IDENTITY_MISMATCH"},
		{name: "deployment permits claim", basis: "sender-identity-claimed"},
		{name: "optional forged proof", signature: "forged", refusal: "SESSION_SENDER_SIGNATURE_INVALID"},
		{name: "optional partial proof", signature: "partial", refusal: "SESSION_SENDER_SIGNATURE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": strconv.FormatBool(tc.enforce), "CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(), "CR_GROUP_ROOT": t.TempDir()})
			key := registerA2AAgent(t, base, "sender", false, nil)
			other := registerA2AAgent(t, base, "other", false, nil)
			client := &http.Client{Timeout: 5 * time.Second}
			var room struct {
				ID string `json:"id"`
			}
			code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{"created_by": map[string]string{"agent": "sender"}, "members": []map[string]string{{"member_type": "agent", "member_id": "other"}}}, &room)
			require.Equal(t, 201, code, string(raw))
			endpoint := "/sessions/" + room.ID + "/messages"
			sender := tc.sender
			if sender == "" {
				sender = "sender"
			}
			body := []byte(`{"sender":"` + sender + `","payload":{"text":"identity test"}}`)
			req, err := http.NewRequest(http.MethodPost, base+endpoint, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tc.signature != "" {
				ts := strconv.FormatInt(time.Now().Unix(), 10)
				req.Header.Set("X-Agent-ID", "sender")
				req.Header.Set("X-Agent-Ts", ts)
				payload := http.MethodPost + "\n" + endpoint + "\n" + ts
				if tc.signature == "bound" || tc.signature == "tampered" {
					sum := sha256.Sum256(body)
					if tc.signature == "tampered" {
						sum = sha256.Sum256([]byte("different body"))
					}
					digest := hex.EncodeToString(sum[:])
					req.Header.Set("X-Agent-Body-SHA256", digest)
					payload += "\nsha256:" + digest
				}
				signingKey := key
				if tc.signature == "forged" {
					signingKey = other
				}
				req.Header.Set("X-Agent-Sig", hex.EncodeToString(ed25519.Sign(signingKey, []byte(payload))))
				if tc.signature == "partial" {
					req.Header.Del("X-Agent-Ts")
				}
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			wire, err := io.ReadAll(resp.Body)
			raw = string(wire)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			var transcript struct {
				Messages []map[string]any `json:"messages"`
			}
			code, read := sessionDo(t, client, http.MethodGet, base+endpoint, nil, &transcript)
			require.Equal(t, 200, code, string(read))
			if tc.refusal != "" {
				require.True(t, resp.StatusCode >= 400, string(raw))
				require.Contains(t, string(raw), tc.refusal)
				require.Empty(t, transcript.Messages, "refusal must write no transcript or branch")
				t.Logf("HTTP %d %s; transcript empty", resp.StatusCode, strings.TrimSpace(string(raw)))
			} else {
				require.Equal(t, 201, resp.StatusCode, string(raw))
				var sent map[string]any
				require.NoError(t, json.Unmarshal(wire, &sent))
				require.Equal(t, tc.basis, sent["sender_identity_basis"])
				require.Len(t, transcript.Messages, 1)
				require.Equal(t, tc.basis, transcript.Messages[0]["sender_identity_basis"])
				t.Logf("recorded %s; transcript basis %v", sent["id"], transcript.Messages[0]["sender_identity_basis"])
			}
			code, inbox := signedAs(t, client, http.MethodGet, base, "/agents/other/inbox", "other", other, nil)
			require.Equal(t, 200, code, string(inbox))
			var entries struct {
				Messages []any `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(inbox, &entries))
			if tc.refusal != "" {
				require.Empty(t, entries.Messages, "refusal must dispatch nothing")
			} else {
				require.Len(t, entries.Messages, 1)
			}
		})
	}
}
