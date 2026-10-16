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
c.audience = "relay-a"
r = Crier(os.environ["SERVICE_TEST_URL"], agent_id="router", key=os.environ["ROUTER_TEST_SEED"], audience="relay-a")
accepted = c.deliver("router", {"task": "python signed delivery"}, sender="scheduler", idempotency_key="py-dispatch")
dispatch = r.retrieve()
assert len(dispatch) == 1, dispatch
dispatch_id = dispatch[0].id
r.ack(dispatch)
r.deliver("scheduler", {"result": "done"}, sender="router", reply_to=dispatch_id, idempotency_key="py-reply")
reply = c.retrieve()
assert len(reply) == 1 and reply[0].reply_to == dispatch_id, reply
c.ack(reply)
c.deliver("router:service", {"task": "python signed delivery"}, sender="scheduler", idempotency_key="py-escaped")
try:
    c.deliver("router", {"task": "impersonation"}, sender="outsider", idempotency_key="py-forged")
except CrierError as e:
    assert e.status == 401, e
else:
    raise AssertionError("unsigned impersonation was accepted")
`
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(), "SERVICE_TEST_URL="+h.server.URL, "SERVICE_TEST_SEED="+hex.EncodeToString(keys["scheduler"].Seed()), "ROUTER_TEST_SEED="+hex.EncodeToString(keys["router"].Seed()))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	for _, target := range []string{"router", "scheduler", "router:service"} {
		q, l, _, err := h.store.Stats(target)
		require.NoError(t, err)
		want := 1
		if target != "router:service" {
			want = 0
		}
		require.Equal(t, want, q+l)
		if want == 0 {
			continue
		}
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
