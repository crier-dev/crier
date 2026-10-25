package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// With ACL disabled existing config permits equal tokens. That configuration
// remains legal, but it MUST NOT turn the reach credential into a trust writer.
func TestTrustEqualCredentialsFailClosed(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{"CR_AUTH_TOKEN": "same-secret", "CR_PERMISSIONS_ADMIN_TOKEN": "same-secret", "CR_PERMISSIONS_ENABLED": "false"})
	client := &http.Client{Timeout: 5 * time.Second}
	for _, method := range []string{"GET", "POST"} {
		for _, path := range []string{"/trust/anchors", "/trust/delegations"} {
			status, body := df297Request(t, client, method, base+path, "same-secret", "", `{}`)
			require.Equal(t, 403, status, body)
		}
	}
	// The prior auth/reach posture is unchanged, only the new trust gate closes.
	status, _ := df297Request(t, client, "GET", base+"/relay/topics", "same-secret", "", "")
	require.Equal(t, 200, status)
}
