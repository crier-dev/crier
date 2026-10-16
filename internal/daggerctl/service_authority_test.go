package daggerctl

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

func TestProtectedRecipientRejectsDaggerWaitAndFanoutBypass(t *testing.T) {
	base := registry.NewMemoryStore()
	if err := base.Register(&registry.Agent{ID: "router", PublicKey: registry.HexKey(make(ed25519.PublicKey, ed25519.PublicKeySize))}); err != nil {
		t.Fatal(err)
	}
	policy, _ := registry.ParseServiceDeliveryAllowlist("scheduler>router")
	secured := registry.NewAuthorityStore(base, policy, "relay-a")
	if err := InboxDeliverer(secured).DeliverToInbox("router", []byte(`{"done":true}`)); err == nil {
		t.Fatal("dagger notification bypass succeeded")
	}

	waits, err := NewWaiterRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{bridge: authorityTestBridge{}, store: NewMemoryStore(), waits: waits, inbox: secured}
	_, err = svc.DeliverAndWait(context.Background(), WaitRequest{AgentID: "router", Payload: json.RawMessage(`{"work":1}`), IdempotencyKey: "wait-key", TimeoutSeconds: MinDeliveryWaitSeconds})
	if err == nil {
		t.Fatal("dagger wait bypass succeeded")
	}
	depth, leased, _, statErr := base.Stats("router")
	if statErr != nil || depth+leased != 0 {
		t.Fatalf("protected inbox depth=%d leased=%d err=%v", depth, leased, statErr)
	}
}

type authorityTestBridge struct{}

func (authorityTestBridge) CreateRun(context.Context, CreateRunRequest) (*RunView, error) {
	return &RunView{}, nil
}
func (authorityTestBridge) RunStatus(context.Context, string) (*RunView, error) {
	return &RunView{}, nil
}
func (authorityTestBridge) Cancel(context.Context, string) (*RunView, error) { return &RunView{}, nil }
func (authorityTestBridge) Resume(context.Context, string) (*RunView, error) { return &RunView{}, nil }
func (authorityTestBridge) Rewind(context.Context, string, string) (*RunView, error) {
	return &RunView{}, nil
}
func (authorityTestBridge) RunSkill(context.Context, RunSkillRequest) (*RunView, error) {
	return &RunView{}, nil
}
