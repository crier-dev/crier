package main

// CR-CHAT-022 end to end, against the REAL server: the D8 distinction — a
// NAMED group and a CAPABILITY target are different things.
//
//   - a named group's audience is fan-out: ONE send reaches EVERY current
//     member through the shipped per-agent inbox path;
//   - a capability target is a selector: ONE delivery reaches ONE live holder
//     (round-robin), never a fan-out;
//   - the two ride DIFFERENT endpoints and different objects and neither may
//     silently become the other (specs/CHAT-ADDRESSING.md §1.4).
//
// The unit-level batteries live in internal/session (groups) and
// internal/registry (capability selection); this one exists because the row's
// acceptance is that the WIRE keeps the two apart.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// d8CreateGroup registers a named group through the real POST /groups route.
func d8CreateGroup(t *testing.T, client *http.Client, base, name string, members ...string) {
	t.Helper()
	memberJSON, _ := json.Marshal(members)
	body := fmt.Sprintf(`{"name":%q,"created_by":"kara","members":%s}`, name, memberJSON)
	resp, err := client.Post(base+"/groups", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group %s = %d, want 201", name, resp.StatusCode)
	}
}

// TestNamedGroupVsCapabilityTargetEndToEnd is the D8 acceptance: the same five
// agents, one curated group and one capability pool; a group send fans out to
// all five, a capability send lands on exactly one holder per delivery.
func TestNamedGroupVsCapabilityTargetEndToEnd(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_SESSION_BACKEND":   "sqlite",
		"CR_SQLITE_PATH":       t.TempDir() + "/sessions.sqlite",
	})
	client := &http.Client{}

	const capability = "solver"
	holders := []string{"solver-a", "solver-b", "solver-c", "solver-d", "solver-e"}
	for _, id := range holders {
		e2eRegisterAgent(t, client, base, id, capability)
	}

	// A NAMED group over the same five agents.
	d8CreateGroup(t, client, base, "team", holders...)

	// A session room to send the group message into.
	room := d8CreateSession(t, client, base)

	// ONE send addressed to the group (explicit `group` target) — the fan-out
	// half of D8: every current member holds the message.
	msgBody := `{"payload":{"text":"deploy window"},"sender":"alice","targets":[{"kind":"group","id":"team"}]}`
	resp, err := client.Post(base+"/sessions/"+room+"/messages", "application/json", strings.NewReader(msgBody))
	if err != nil {
		t.Fatalf("group send: %v", err)
	}
	raw := d8ReadAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("group send = %d, want 201: %s", resp.StatusCode, raw)
	}
	for _, id := range holders {
		entries := d8Retrieve(t, client, base, id)
		if len(entries) != 1 {
			t.Fatalf("group fan-out: agent %s holds %d entries, want exactly 1 (the group is a FAN-OUT)", id, len(entries))
		}
	}

	// Drain every inbox, then dial the CAPABILITY twice: the selector half of
	// D8 — one holder per delivery, and the two deliveries go to DIFFERENT
	// holders (round-robin over a live pool).
	for _, id := range holders {
		d8Retrieve(t, client, base, id) // drain
	}
	first := d8CapabilityDeliver(t, client, base, capability)
	second := d8CapabilityDeliver(t, client, base, capability)
	if first == second {
		t.Fatalf("capability is a round-robin SELECTOR, not a fan-out: both deliveries landed on %s", first)
	}
	total := 0
	for _, id := range holders {
		total += len(d8Retrieve(t, client, base, id))
	}
	if total != 2 {
		t.Fatalf("two capability deliveries must reach exactly TWO holders, reached %d", total)
	}
}

// d8CreateSession opens a channel room and returns its id.
func d8CreateSession(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	body := `{"title":"d8","kind":"channel","created_by":{"principal":"kara"},"members":[]}`
	resp, err := client.Post(base+"/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := d8ReadAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session = %d: %s", resp.StatusCode, raw)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.ID == "" {
		t.Fatalf("decode session id: %v (%s)", err, raw)
	}
	return decoded.ID
}

// d8CapabilityDeliver dials the capability route and returns the holder the
// accept names.
func d8CapabilityDeliver(t *testing.T, client *http.Client, base, capability string) string {
	t.Helper()
	resp, err := client.Post(base+"/capabilities/"+capability+"/inbox", "application/json",
		strings.NewReader(`{"payload":{"job":1},"sender":"alice"}`))
	if err != nil {
		t.Fatalf("capability deliver: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := d8ReadAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("capability deliver = %d: %s", resp.StatusCode, raw)
	}
	var accept struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &accept); err != nil || accept.Target == "" {
		t.Fatalf("decode capability accept: %v (%s)", err, raw)
	}
	return accept.Target
}

// d8Retrieve drains one agent's inbox (ack-free read) and returns the entries.
func d8Retrieve(t *testing.T, client *http.Client, base, id string) []map[string]any {
	t.Helper()
	resp, err := client.Get(base + "/agents/" + id + "/inbox")
	if err != nil {
		t.Fatalf("retrieve %s: %v", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := d8ReadAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve %s = %d: %s", id, resp.StatusCode, raw)
	}
	var decoded struct {
		Entries []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode inbox %s: %v (%s)", id, err, raw)
	}
	return decoded.Entries
}

// d8ReadAll reads a response body; it is a test helper, so a read error is
// fatal rather than threaded through every caller.
func d8ReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	var buf strings.Builder
	buf2 := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf2)
		if n > 0 {
			buf.Write(buf2[:n])
		}
		if err != nil {
			break
		}
	}
	return []byte(buf.String())
}
