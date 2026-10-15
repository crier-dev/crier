package registry

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is a real cross-language HTTP round trip, not a mock of the Go verifier
// or Python signer. It talks only to this test's disposable loopback server.
func TestServiceDeliveryPythonInterop(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable: service client interoperability requires Python")
	}
	h, keys := serviceHarnessWithPolicy(t, "scheduler>router,scheduler>router:service,router>scheduler")
	h.register(t, "router:service")
	script := `
import os, sys
sys.path.insert(0, "../../clients/python")
from crier_client import Crier, CrierError
c = Crier(os.environ["SERVICE_TEST_URL"], agent_id="scheduler", key=os.environ["SERVICE_TEST_SEED"])
c.deliver("router", {"task": "python signed delivery"}, sender="scheduler")
c.deliver("router:service", {"task": "python signed delivery"}, sender="scheduler")
try:
    c.deliver("router", {"task": "impersonation"}, sender="outsider")
except CrierError as e:
    assert e.status == 401, e
else:
    raise AssertionError("unsigned impersonation was accepted")
`
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(), "SERVICE_TEST_URL="+h.server.URL, "SERVICE_TEST_SEED="+hex.EncodeToString(keys["scheduler"].Seed()))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	for _, target := range []string{"router", "router:service"} {
		q, l, _, err := h.store.Stats(target)
		require.NoError(t, err)
		require.Equal(t, 1, q+l)
		entries, _, err := h.store.Retrieve(target, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, "scheduler", entries[0].Sender)
		require.JSONEq(t, `{"task":"python signed delivery"}`, string(entries[0].Payload))
		// The unsigned impersonation could not create an extra stored entry.
		raw, err := json.Marshal(entries[0])
		require.NoError(t, err)
		require.Contains(t, string(raw), `"sender":"scheduler"`)
	}
}
