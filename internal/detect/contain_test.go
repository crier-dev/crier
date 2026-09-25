package detect

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

// stubPauser records the pause calls it receives.
type stubPauser struct {
	called  []string
	dropped int
	err     error
}

func (s *stubPauser) PauseAgent(agentID string) (int, error) {
	s.called = append(s.called, agentID)
	return s.dropped, s.err
}

// storeWithoutRevoke implements RegistryStore but NOT LeaseRevoker — the
// RemoteStore shape. Containment must report that honestly.
type storeWithoutRevoke struct{ agents map[string]*registry.Agent }

func (s *storeWithoutRevoke) Get(id string) (*registry.Agent, error) {
	a, ok := s.agents[id]
	if !ok {
		return nil, registry.ErrAgentNotFound
	}
	return a, nil
}

func (s *storeWithoutRevoke) Unregister(id string) error {
	if _, ok := s.agents[id]; !ok {
		return registry.ErrAgentNotFound
	}
	delete(s.agents, id)
	return nil
}

// TestContainDoesAllFourStepsInOneCall is the kill-switch's contract: ONE call
// performs every action, each with its own reported outcome — unregister,
// revoke the leases, pause the webhooks, quarantine — and a second call is
// idempotent rather than a new incident.
func TestContainDoesAllFourStepsInOneCall(t *testing.T) {
	dir := t.TempDir()
	store := registry.NewMemoryStore()
	for _, id := range []string{"attacker", "peer-1"} {
		if err := store.Register(&registry.Agent{ID: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// Give the attacker a leased message, so revoke_leases has real work.
	if err := store.Deliver("attacker", &registry.InboxEntry{ID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, leaseID, err := store.Retrieve("attacker", 0, 10); err != nil || leaseID == "" {
		t.Fatalf("lease the attacker's message: lease=%q err=%v", leaseID, err)
	}

	pauser := &stubPauser{dropped: 2}
	d, err := New(Config{
		LogPath:          filepath.Join(dir, "delivery.jsonl"),
		KeyPath:          filepath.Join(dir, "delivery.key"),
		FanoutMinTargets: 99, NewPeerMinTargets: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetStore(store)
	d.SetWebhooks(pauser)

	got := d.Contain("attacker", "fan-out to 5 new peers (ALERT-000001)")
	if !got.Contained {
		t.Fatalf("containment did not report success: %+v", got)
	}
	if len(got.Actions) != 4 {
		t.Fatalf("actions = %d, want 4 (%+v)", len(got.Actions), got.Actions)
	}
	wantOrder := []string{ActionPauseWebhooks, ActionRevokeLeases, ActionQuarantine, ActionUnregister}
	for i, want := range wantOrder {
		if got.Actions[i].Action != want {
			t.Fatalf("action %d = %q, want %q (the ORDER is the contract: pause, release, quarantine, remove)", i, got.Actions[i].Action, want)
		}
		if got.Actions[i].Status != ActionStatusOK {
			t.Fatalf("action %s status = %q, want ok", want, got.Actions[i].Status)
		}
	}
	if got.Actions[0].Count != 2 {
		t.Fatalf("pause_webhooks count = %d, want the 2 dropped deliveries", got.Actions[0].Count)
	}
	if got.Actions[1].Count != 1 {
		t.Fatalf("revoke_leases count = %d, want the 1 lease the attacker held", got.Actions[1].Count)
	}
	if len(pauser.called) != 1 || pauser.called[0] != "attacker" {
		t.Fatalf("pauser calls = %v, want exactly one for the attacker", pauser.called)
	}
	if !d.Quarantined("attacker") {
		t.Fatal("the detector does not consider the agent quarantined after the kill-switch")
	}
	if !d.Quarantined("attacker") || got.WasQuarantined() {
		t.Fatal("a first containment must not report itself as already-quarantined")
	}
	// The registry row is gone (so is its inbox — Unregister is the shipped
	// store contract), and the revoke_leases step is what recorded HOW MANY
	// messages the contained agent had claimed: that count is in the log line
	// and in the response, which is the forensic half of containment. The
	// release itself (messages claimable again) is proven at the store level
	// in internal/registry/lease_revoke_test.go, where the row is not removed.
	if _, err := store.Get("attacker"); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Fatalf("registry row still resolves after the kill-switch: %v", err)
	}

	// Idempotence: a second call is a clean, self-describing no-op.
	again := d.Contain("attacker", "re-run")
	if !again.Contained || !again.WasQuarantined() {
		t.Fatalf("second containment = %+v, want it to report already-quarantined and still contained", again)
	}

	// Containment is IN the log, and the log still verifies.
	records := d.Audit().Entries(0, KindContainment)
	if len(records) != 2 {
		t.Fatalf("containment records = %d, want 2 (one per call)", len(records))
	}
	if records[0].Verdict != "contained" {
		t.Fatalf("containment record verdict = %q, want contained", records[0].Verdict)
	}
	if !strings.Contains(records[0].Detail, ActionRevokeLeases+"=ok(1)") {
		t.Fatalf("containment record detail = %q, want the per-action outcome", records[0].Detail)
	}
	if rep := d.Audit().Verify(); !rep.OK {
		t.Fatalf("log does not verify after containment: %+v", rep)
	}
}

func errGet(err error) error { return err }

// TestContainReportsUnsupportedAndPartialHonestly: a store that cannot revoke
// leases (RemoteStore) and a driver that cannot pause must produce
// "unsupported" warnings, never a silent success.
func TestContainReportsUnsupportedAndPartialHonestly(t *testing.T) {
	store := &storeWithoutRevoke{agents: map[string]*registry.Agent{"attacker": {ID: "attacker"}}}
	d, err := New(Config{CanaryTokens: []string{"t"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetStore(store)
	// No webhooks wired at all.
	got := d.Contain("attacker", "test")
	if len(got.Actions) != 4 {
		t.Fatalf("actions = %d, want all four reported", len(got.Actions))
	}
	if got.Actions[0].Status != ActionStatusUnsupported {
		t.Fatalf("pause_webhooks = %q, want unsupported with no driver wired", got.Actions[0].Status)
	}
	if got.Actions[1].Status != ActionStatusUnsupported {
		t.Fatalf("revoke_leases = %q, want unsupported for a store without the capability", got.Actions[1].Status)
	}
	if got.Actions[2].Status != ActionStatusOK || got.Actions[3].Status != ActionStatusOK {
		t.Fatalf("quarantine/unregister must still succeed: %+v", got.Actions)
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("warnings = %v, want one per unsupported action", got.Warnings)
	}
	if !got.Contained {
		t.Fatal("an unsupported lease revoker must not make containment read as failed — the agent IS quarantined and unregistered; it is a warning")
	}
}

// TestContainUnregisteredAgentIsIdempotentNotAnError: an operator containing an
// agent whose row is already gone gets an honest "nothing to remove", not a 404.
func TestContainUnregisteredAgentIsIdempotentNotAnError(t *testing.T) {
	store := registry.NewMemoryStore()
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetStore(store)
	got := d.Contain("ghost", "test")
	var unregister ActionResult
	for _, a := range got.Actions {
		if a.Action == ActionUnregister {
			unregister = a
		}
	}
	if unregister.Status != ActionStatusOK {
		t.Fatalf("unregister of an absent agent = %q, want ok (idempotent)", unregister.Status)
	}
	if !strings.Contains(unregister.Detail, "not registered") {
		t.Fatalf("unregister detail = %q, want it to say the row was already absent", unregister.Detail)
	}
	if !got.Contained {
		t.Fatal("containing an absent agent must still be a successful containment")
	}
}

// TestQuarantineStateIsIndependentOfTheLog: with no log configured, containment
// and the quarantine answer still work (nothing about detection depends on the
// file being configured).
func TestQuarantineStateIsIndependentOfTheLog(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Audit() != nil {
		t.Fatal("a detector with no log path opened one anyway")
	}
	if d.Quarantined("x") {
		t.Fatal("nothing is quarantined before the kill-switch")
	}
	d.Contain("x", "")
	if !d.Quarantined("x") {
		t.Fatal("containment did not quarantine")
	}
	if got := d.Quarantines(); len(got) != 1 || got[0].AgentID != "x" {
		t.Fatalf("Quarantines() = %+v, want x", got)
	}
}
