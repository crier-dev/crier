package registry

// CR-CHAT-020: per-message delivery latency — recorded on the deliver path,
// carried by retrieve, and ABSENT (not zero) on entries no timed delivery
// produced.

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDeliveryMsRidesDeliverAndRetrieve(t *testing.T) {
	store := NewMemoryStore()

	if err := store.Register(&Agent{ID: "target", PublicKey: HexKey("aa"), Capabilities: []string{}}); err != nil {
		t.Fatalf("register: %v", err)
	}

	entry := &InboxEntry{Payload: []byte(`{"text":"hi"}`), DeliveryMs: 86}
	if err := store.Deliver("target", entry); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	msgs, _, err := store.Retrieve("target", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("retrieved %d messages, want 1", len(msgs))
	}
	if msgs[0].DeliveryMs != 86 {
		t.Errorf("DeliveryMs = %d, want 86 (the delivery's latency, carried by the read)", msgs[0].DeliveryMs)
	}

	// The wire shape: the field serializes as delivery_ms on the entry.
	wire, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := decoded["delivery_ms"].(float64); !ok || int64(got) != 86 {
		t.Errorf("wire delivery_ms = %#v, want 86", decoded["delivery_ms"])
	}

	// An entry written WITHOUT a timed delivery (a receipt, a hold release —
	// anything the store receives with DeliveryMs zero) reads back absent,
	// never zero: a fabricated 0ms delivery would be indistinguishable from
	// a measurement.
	if err := store.Deliver("target", &InboxEntry{Payload: []byte(`{"text":"receipt"}`)}); err != nil {
		t.Fatalf("deliver untimed: %v", err)
	}
	msgs, _, err = store.Retrieve("target", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("retrieve 2: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("retrieved %d messages on second read, want 1", len(msgs))
	}
	wire, err = json.Marshal(msgs[0])
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	var decoded2 map[string]any
	if err := json.Unmarshal(wire, &decoded2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if _, present := decoded2["delivery_ms"]; present {
		t.Errorf("untimed entry serializes delivery_ms = %#v, want the field ABSENT (not measured ≠ zero)", decoded2["delivery_ms"])
	}
}
