package harness_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// stubAdapter is a minimal Adapter for registry tests: it records deliveries
// and never runs a process.
type stubAdapter struct {
	name     string
	delivers atomic.Int64
	lastSess harness.SessionRef
	lastMsg  harness.Message
}

func (s *stubAdapter) Name() string { return s.name }

func (s *stubAdapter) Probe(context.Context) (harness.Capability, error) {
	return harness.Capability{
		Available: true, Version: "stub", Features: map[string]bool{"continue": true},
	}, nil
}

func (s *stubAdapter) Deliver(_ context.Context, sess harness.SessionRef, msg harness.Message) (harness.Receipt, error) {
	s.delivers.Add(1)
	s.lastSess = sess
	s.lastMsg = msg
	return harness.Receipt{
		Harness: s.name, SessionID: sess.ID, State: harness.StateDispatched, At: time.Now().UTC(),
	}, nil
}

func TestRegistryRefusesWhenDisabled(t *testing.T) {
	stub := &stubAdapter{name: "stub"}
	reg := harness.NewRegistry(harness.RegistryOptions{Enabled: false, Default: "stub"})
	reg.Register(stub)

	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Message: harness.Message{ID: "m", SessionID: "s", Text: "x"},
	})
	if rc.State != harness.StateFailed || rc.Error != harness.CodeDisabled {
		t.Fatalf("want failed/%s, got %+v", harness.CodeDisabled, rc)
	}
	if stub.delivers.Load() != 0 {
		t.Fatal("a disabled registry must not reach the adapter")
	}
}

func TestRegistryUnknownHarness(t *testing.T) {
	reg := harness.NewRegistry(harness.RegistryOptions{Enabled: true, Default: "hermes"})
	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Harness: "pi", Message: harness.Message{ID: "m", SessionID: "s"},
	})
	if rc.State != harness.StateFailed || rc.Error != harness.CodeUnknown {
		t.Fatalf("want failed/%s, got %+v", harness.CodeUnknown, rc)
	}
}

func TestRegistryResolvesDefaultHarness(t *testing.T) {
	stub := &stubAdapter{name: "stub"}
	reg := harness.NewRegistry(harness.RegistryOptions{Enabled: true, Default: "stub"})
	reg.Register(stub)

	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		SessionRef: harness.SessionRef{ID: "ses_1"},
		Message:    harness.Message{ID: "m", SessionID: "s"},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("want dispatched, got %+v", rc)
	}
	if rc.Harness != "stub" {
		t.Fatalf("receipt must name the resolved adapter: %q", rc.Harness)
	}
	if stub.lastSess.Harness != "stub" {
		t.Fatalf("adapter must receive its own name on the session ref: %+v", stub.lastSess)
	}
	if reg.DeliveredCount() != 1 {
		t.Fatalf("delivered count: %d", reg.DeliveredCount())
	}
}

func TestRegistryProbeUnknown(t *testing.T) {
	reg := harness.NewRegistry(harness.RegistryOptions{Enabled: true})
	if _, err := reg.Probe(context.Background(), "nope"); err == nil {
		t.Fatal("probing an unregistered adapter must return an error")
	}
}

func TestCapabilitiesCoversEveryRegisteredAdapter(t *testing.T) {
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled:          true,
		KnownUnavailable: map[string]string{"pi": "later row"},
	})
	reg.Register(&stubAdapter{name: "stub"})
	caps := reg.Capabilities(context.Background())
	if len(caps) != 2 {
		t.Fatalf("want 2 capabilities, got %d: %v", len(caps), caps)
	}
	if !caps["stub"].Available {
		t.Fatal("stub must be available")
	}
	if caps["pi"].Available || caps["pi"].Reason != "later row" {
		t.Fatalf("pi must report unavailable with its reason: %+v", caps["pi"])
	}
}
