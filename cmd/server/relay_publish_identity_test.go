package main

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Proof offered on an ordinary relay topic must not be ignored, even without
// the A2A option. This does not make proof mandatory for ordinary relay traffic.
func TestRelayPublishOfferedIdentityHTTP(t *testing.T) {
	for _, a2aEnabled := range []bool{false, true} {
		t.Run("a2a="+strconv.FormatBool(a2aEnabled), func(t *testing.T) {
			base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": strconv.FormatBool(a2aEnabled), "CR_REQUIRE_AGENT_SIG": "true"})
			owner := registerA2AAgent(t, base, "relay-owner-372", true, nil)
			other := registerA2AAgent(t, base, "relay-other-372", true, nil)
			body := []byte(`{"topic":"ordinary.topic","event":{"text":"ordinary event"}}`)
			code, raw := signedAs(t, http.DefaultClient, http.MethodPost, base, "/relay/publish", "relay-owner-372", other, body)
			assert.Equal(t, http.StatusUnauthorized, code, string(raw))
			assert.Contains(t, string(raw), "signature verification failed")
			code, raw = signedAs(t, http.DefaultClient, http.MethodPost, base, "/relay/publish", "relay-owner-372", owner, body)
			require.Equal(t, http.StatusAccepted, code, string(raw))
			resp, err := publishToRelay(base, string(body))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusAccepted, resp.StatusCode, "ordinary unsigned flow remains accepted")
		})
	}
}
