package session

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

func TestProtectedRecipientRejectsSessionFanoutBypass(t *testing.T) {
	base := registry.NewMemoryStore()
	if err := base.Register(&registry.Agent{ID: "router", PublicKey: registry.HexKey(make(ed25519.PublicKey, ed25519.PublicKeySize))}); err != nil {
		t.Fatal(err)
	}
	policy, _ := registry.ParseServiceDeliveryAllowlist("scheduler>router")
	secured := registry.NewAuthorityStore(base, policy, "relay-a")
	msg := &Message{ID: "m1", SessionID: "s1", ThreadID: "t1", Payload: json.RawMessage(`{"x":1}`)}
	outcomes, err := Fanout(context.Background(), secured, msg, []string{"router"}, nil)
	if err != nil || len(outcomes) != 1 || outcomes[0].Outcome != OutcomeRefused {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
	depth, leased, _, err := base.Stats("router")
	if err != nil || depth+leased != 0 {
		t.Fatalf("protected inbox depth=%d leased=%d err=%v", depth, leased, err)
	}
}
