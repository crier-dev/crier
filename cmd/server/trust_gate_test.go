package main

import (
	"strconv"
	"testing"
	"time"
)

// With ACL disabled equal tokens used to be a LEGAL configuration whose only
// effect was the trust surface refusing its own writes (fail closed) — the
// admin token armed nothing on its own. DF-CRIER-515 changed that premise: the
// same token now also arms the registry's operator delete (DELETE
// /agents/{id} with the admin Bearer) independently of CR_PERMISSIONS_ENABLED,
// so the two-roles-on-one-secret collapse is no longer inert and DF-CRIER-297's
// boot refusal applies in every posture. The fail-closed outcome is therefore
// enforced one level earlier — the configuration cannot serve at all. (The
// trust surface's own fail-closed gate for DISTINCT-token deployments is
// unchanged and covered by the trust tests in internal/registry and the
// dual-token e2e below.)
func TestTrustEqualCredentialsFailClosed(t *testing.T) {
	const shared = "same-secret"
	t.Setenv("CR_AUTH_TOKEN", shared)
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", shared)
	t.Setenv("CR_PERMISSIONS_ENABLED", "false")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CRIER_PORT", strconv.Itoa(freePort(t)))

	exitCode := make(chan int, 1)
	go func() { exitCode <- run(nil) }()

	select {
	case code := <-exitCode:
		if code == 0 {
			t.Fatal("run() returned 0 with CR_PERMISSIONS_ADMIN_TOKEN == CR_AUTH_TOKEN and the ACL off; the boot must be refused (DF-CRIER-515: the token arms the registry's operator delete)")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run() did not return within 20s on the colliding-secret config (it must refuse the boot)")
	}
}
