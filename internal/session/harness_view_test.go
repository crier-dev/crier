package session

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSessionViewCarriesTheHarnessBinding pins CRIER-HARNESS §4.3 at the HTTP
// boundary: the session object a client reads must SAY which agent session a
// push will reach. Without this the UI cannot show a harness badge, because the
// field would never arrive — an absent binding is the absence of a claim, and a
// binding that exists must be visible, not inferable only from the log.
func TestSessionViewCarriesTheHarnessBinding(t *testing.T) {
	h := &Handler{now: time.Now}

	bound := &State{Session: &Session{
		ID:               "s_bound",
		Namespace:        "default",
		Kind:             KindChannel,
		Title:            "bound",
		CreatedAt:        time.Now().UTC(),
		CreatedBy:        AuthorRef{Agent: "operator"},
		State:            SessionOpen,
		Harness:          "pi",
		HarnessSessionID: "crier-joint-1",
	}}
	v := h.viewOf(bound)
	if v.Harness != "pi" || v.HarnessSessionID != "crier-joint-1" {
		t.Fatalf("bound view = harness %q id %q, want pi / crier-joint-1", v.Harness, v.HarnessSessionID)
	}

	// On the wire, both fields must be present and non-empty.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	if wire["harness"] != "pi" || wire["harness_session_id"] != "crier-joint-1" {
		t.Fatalf("wire view = %v, want harness/harness_session_id present", wire)
	}

	// A store-only session must OMIT them rather than send empty strings, so a
	// client can distinguish "no binding" from "binding with no id".
	plain := &State{Session: &Session{
		ID:        "s_plain",
		Namespace: "default",
		Kind:      KindChannel,
		CreatedAt: time.Now().UTC(),
		CreatedBy: AuthorRef{Agent: "operator"},
		State:     SessionOpen,
	}}
	raw, err = json.Marshal(h.viewOf(plain))
	if err != nil {
		t.Fatalf("marshal plain view: %v", err)
	}
	wire = nil
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal plain view: %v", err)
	}
	if _, ok := wire["harness"]; ok {
		t.Fatalf("store-only view sent a harness field: %v", wire)
	}
	if _, ok := wire["harness_session_id"]; ok {
		t.Fatalf("store-only view sent a harness_session_id field: %v", wire)
	}
}
