package main

// CR-FEAT-026 end to end, against the REAL server (run(nil) — middleware,
// router, registry store, exactly as cmd/server builds them).
//
// The unit-level tests live with the package (internal/registry/capability_test.go);
// this one exists because the capability route is only worth anything if the
// SERVER ships it, and because the row's acceptance is a live round-trip:
//
//   - deliver to a capability lands on a HOLDER, the accept names it, and the
//     message is retrievable and ackable from the inbox the accept named;
//   - the rotation moves across the holders of that capability, one holder per
//     delivery;
//   - a capability nobody holds is the NAMED 404 refusal, and nothing is stored;
//   - delivering by agent id is untouched — the same body on the by-id route
//     still yields the same accept, with no routing fields on it.
//
// Signature enforcement is off for this boot (the README dev posture), so the
// agent-scoped retrieve and ack can be driven with plain HTTP; nothing else
// about the wiring is stubbed.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// e2eRegisterAgent registers an agent advertising caps through the real
// registry route and asserts the documented 201.
func e2eRegisterAgent(t *testing.T, client *http.Client, baseURL, id string, capabilities ...string) {
	t.Helper()
	caps, _ := json.Marshal(capabilities)
	body := fmt.Sprintf(`{"id":%q,"capabilities":%s}`, id, caps)
	resp, err := client.Post(baseURL+"/agents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s = %d, want 201: %s", id, resp.StatusCode, raw)
	}
}

// e2eCapabilityDeliver POSTs a delivery to the capability route and returns the
// status plus the decoded accept.
func e2eCapabilityDeliver(t *testing.T, client *http.Client, baseURL, capability, payload string) (int, map[string]any, string) {
	t.Helper()
	body := fmt.Sprintf(`{"payload":%s,"sender":"alice"}`, payload)
	resp, err := client.Post(baseURL+"/capabilities/"+capability+"/inbox", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("deliver to capability %s: %v", capability, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode accept for %s: %v (body %s)", capability, err, raw)
	}
	return resp.StatusCode, decoded, string(raw)
}

// TestCapabilityRoutedDeliveryEndToEnd is the live proof of the row's
// acceptance criterion: deliver-to-capability lands on a holder, is retrievable
// and ackable there, the rotation moves across holders, a zero-holder dial is
// the named refusal with nothing stored, and the by-id route is unchanged.
func TestCapabilityRoutedDeliveryEndToEnd(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": "false"})
	client := &http.Client{Timeout: 30 * time.Second}

	const capability = "pokemon-solver"
	holders := []string{"solver-a", "solver-b", "solver-c"}
	for _, id := range holders {
		e2eRegisterAgent(t, client, base, id, capability)
	}
	// A holder of a DIFFERENT capability, so the routing below cannot be
	// explained by a single-agent registry.
	e2eRegisterAgent(t, client, base, "unrelated", "translator")

	// Three deliveries over a three-holder pool: the documented rotation is
	// agent-id ascending, so the targets are exactly a, b, c.
	for i, want := range holders {
		status, accept, raw := e2eCapabilityDeliver(t, client, base, capability, fmt.Sprintf(`{"job":%d}`, i))
		if status != http.StatusCreated {
			t.Fatalf("delivery %d = %d, want 201: %s", i, status, raw)
		}
		if accept["transport"] != "inbox" {
			t.Errorf("delivery %d transport = %v, want inbox", i, accept["transport"])
		}
		if accept["capability"] != capability {
			t.Errorf("delivery %d accept capability = %v, want %q", i, accept["capability"], capability)
		}
		if accept["target"] != want {
			t.Fatalf("delivery %d landed on %v, want %s (round-robin over the live holders)", i, accept["target"], want)
		}

		// The message is retrievable from the inbox the accept NAMED, which is
		// the whole point of reporting the target, and ackable from there.
		status, body, _ := e2eGet(t, client, base+"/agents/"+want+"/inbox")
		if status != http.StatusOK {
			t.Fatalf("retrieve %s = %d: %s", want, status, body)
		}
		var read struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			LeaseID string `json:"lease_id"`
		}
		if err := json.Unmarshal([]byte(body), &read); err != nil {
			t.Fatalf("decode retrieve %s: %v (%s)", want, err, body)
		}
		if len(read.Messages) != 1 || read.LeaseID == "" {
			t.Fatalf("%s holds %d message(s) with lease %q, want exactly the one routed to it", want, len(read.Messages), read.LeaseID)
		}
		ackBody := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, read.LeaseID, read.Messages[0].ID)
		resp, err := client.Post(base+"/agents/"+want+"/inbox/ack", "application/json", strings.NewReader(ackBody))
		if err != nil {
			t.Fatalf("ack %s: %v", want, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("ack %s = %d, want 204", want, resp.StatusCode)
		}
		_, after, _ := e2eGet(t, client, base+"/agents/"+want+"/inbox")
		if !strings.Contains(after, `"messages":[]`) {
			t.Errorf("%s's inbox is not empty after the ack: %s", want, after)
		}
	}

	// Zero holders: the NAMED refusal, and nothing stored anywhere. The
	// capability is genuinely unheld (the four registrations above advertise
	// `pokemon-solver` and `translator` only).
	status, refusal, raw := e2eCapabilityDeliver(t, client, base, "nobody-holds-this", `{"job":"orphan"}`)
	if status != http.StatusNotFound {
		t.Fatalf("zero-holder delivery = %d, want 404: %s", status, raw)
	}
	if refusal["error"] != "NO_CAPABLE_AGENT" {
		t.Errorf("zero-holder error = %v, want NO_CAPABLE_AGENT: %s", refusal["error"], raw)
	}
	if refusal["capability"] != "nobody-holds-this" {
		t.Errorf("refusal names capability %v, want the one dialed", refusal["capability"])
	}
	if _, body, _ := e2eGet(t, client, base+"/agents/unrelated/inbox"); !strings.Contains(body, `"messages":[]`) {
		t.Errorf("the refusal stored something in an unrelated inbox: %s", body)
	}

	// The by-id route is untouched: same body, same accept, and no routing
	// fields on it.
	resp, err := client.Post(base+"/agents/solver-a/inbox", "application/json", strings.NewReader(`{"payload":{"job":9},"sender":"alice"}`))
	if err != nil {
		t.Fatalf("by-id deliver: %v", err)
	}
	rawByID, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("by-id deliver = %d: %s", resp.StatusCode, rawByID)
	}
	if strings.Contains(string(rawByID), "capability") || strings.Contains(string(rawByID), `"target"`) {
		t.Errorf("by-id accept gained routing fields: %s", rawByID)
	}

	// And a holder that never claimed the by-id message still has exactly one
	// message waiting: the capability deliveries were acked, this one was not.
	if _, body, _ := e2eGet(t, client, base+"/agents/solver-a/inbox"); !strings.Contains(body, `"queue_depth":1`) {
		t.Errorf("solver-a queue_depth is not 1 after one by-id delivery: %s", body)
	}
}
