package main

// CR-FEAT-024 — presence, end to end, against the REAL server (run(nil):
// middleware, router, registry store, mesh and the wired heartbeat sink, exactly
// as cmd/server builds them).
//
// The acceptance criterion is a LIFE-CYCLE observation, not a field check: an
// agent connects and heartbeats (so its registry row carries live evidence), the
// agent then DIES mid-session — the socket goes away with no close frame, no
// unregister and no PATCH, exactly like a crashed process — and the registry row
// must go `stale` within the documented window, with the transition captured
// live.
//
// The window is set to 2s for this boot so the test can watch the same rule the
// shipped default (90s, registry.DefaultStalenessWindow) applies; nothing else
// about the wiring is stubbed. Signature enforcement is off (the README dev
// posture) so the agent-scoped routes can be driven with plain HTTP.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// presenceWindowS is the documented staleness window this boot runs with. Small
// on purpose: the assertion is about the RULE, and a 90s default would make the
// transition untestable (the default itself is pinned by the docs gate and by
// config tests).
const presenceWindowS = 2

// presenceAgent is the agent the test brings up, kills, and then watches.
const presenceAgent = "presence-walker"

// agentRow is the part of the registry's agent body this test reads.
type agentRow struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen"`
}

// fetchAgent reads GET /agents/{id} and returns the decoded row.
func fetchAgent(t *testing.T, client *http.Client, baseURL, id string) agentRow {
	t.Helper()
	resp, err := client.Get(baseURL + "/agents/" + id)
	if err != nil {
		t.Fatalf("GET /agents/%s: %v", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents/%s = %d: %s", id, resp.StatusCode, raw)
	}
	var row agentRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode agent row %s: %v", raw, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, row.LastSeen); err != nil {
		t.Fatalf("last_seen %q is not RFC 3339: %v", row.LastSeen, err)
	}
	return row
}

// parseLastSeen decodes the row's last_seen (already validated by fetchAgent).
func parseLastSeen(t *testing.T, row agentRow) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, row.LastSeen)
	if err != nil {
		t.Fatalf("parse last_seen %q: %v", row.LastSeen, err)
	}
	return ts
}

// awaitPeer waits until the server itself reports the connected peer, so the
// heartbeat below cannot race the accept.
func awaitPeer(t *testing.T, client *http.Client, baseURL, want string, present bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/mesh/peers")
		if err != nil {
			t.Fatalf("GET /mesh/peers: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(raw), want) == present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /mesh/peers never %s %s: %s", map[bool]string{true: "listed", false: "dropped"}[present], want, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPresenceGoesStaleWhenTheAgentDiesEndToEnd is the CR-FEAT-024 acceptance
// criterion, measured live.
func TestPresenceGoesStaleWhenTheAgentDiesEndToEnd(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":      "false",
		"CR_PRESENCE_STALE_AFTER_S": fmt.Sprint(presenceWindowS),
	})
	client := &http.Client{Timeout: 5 * time.Second}

	// The window in force is the one the operator asked for — a `stale` row is
	// only readable if the API says what it was judged by.
	resp, err := client.Get(base + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	statusRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var posture struct {
		PresenceStaleAfterS int `json:"presence_stale_after_s"`
	}
	if err := json.Unmarshal(statusRaw, &posture); err != nil {
		t.Fatalf("decode /status %s: %v", statusRaw, err)
	}
	if posture.PresenceStaleAfterS != presenceWindowS {
		t.Fatalf("GET /status presence_stale_after_s = %d, want %d", posture.PresenceStaleAfterS, presenceWindowS)
	}

	// (1) The agent registers: its row carries the registration instant as its
	// only liveness evidence so far.
	registerAgent(t, client, base, presenceAgent)
	registered := fetchAgent(t, client, base, presenceAgent)
	if registered.Status != "online" {
		t.Fatalf("a just-registered row reports %q, want online", registered.Status)
	}
	registeredSeen := parseLastSeen(t, registered)

	// (2) The agent comes up on the mesh. Sleep first so the evidence that the
	// heartbeat records is measurably LATER than the registration stamp —
	// otherwise "the heartbeat advanced last_seen" could be true by luck.
	time.Sleep(25 * time.Millisecond)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + presenceAgent
	conn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if wsResp != nil {
			status = wsResp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", wsURL, err, status)
	}
	awaitPeer(t, client, base, presenceAgent, true)

	// (3) A heartbeat is evidence: it must ADVANCE the row's last_seen, which is
	// the wiring this whole row is about — the mesh traffic already flowed, it
	// simply never reached the registry.
	heartbeat := []byte(`{"type":"KEEPALIVE","version":1,"message_id":"ka-presence","timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `","lease_id":"","agent_id":"` + presenceAgent + `"}` + "\n")
	if err := conn.WriteMessage(websocket.TextMessage, heartbeat); err != nil {
		t.Fatalf("write KEEPALIVE: %v", err)
	}
	evidenceAt := time.Now()

	var live agentRow
	deadline := time.Now().Add(2 * time.Second)
	for {
		live = fetchAgent(t, client, base, presenceAgent)
		if parseLastSeen(t, live).After(registeredSeen) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the KEEPALIVE did not advance last_seen: still %s (registered %s) — mesh heartbeats are not reaching the registry",
				live.LastSeen, registered.LastSeen)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if live.Status != "online" {
		t.Fatalf("a heartbeating agent reports %q, want online", live.Status)
	}

	// (4) Kill the agent mid-session. Close() drops the TCP connection without a
	// close handshake — a crash, not a graceful shutdown: no unregister, no
	// PATCH, nothing that could refresh the row.
	if err := conn.Close(); err != nil {
		t.Fatalf("close the agent's socket: %v", err)
	}

	// The mesh notices the dead socket...
	awaitPeer(t, client, base, presenceAgent, false)

	// Baseline for (6), sampled AFTER the socket is gone and the peer is off the
	// list. A heartbeat can legitimately land in the instant between the (3)
	// snapshot and the close, so the invariant this test owns is the one that
	// holds from here on: a DEAD agent produces no evidence, and nothing that
	// merely READS the row manufactures any.
	postKill := fetchAgent(t, client, base, presenceAgent)

	// (5) ...and the REGISTRY row must stop claiming online, within the
	// documented window, with the transition captured live. Every observation is
	// recorded so the failure message can show what was actually seen rather
	// than only the final state.
	type observation struct {
		at     time.Duration
		status string
	}
	var seen []observation
	var flip *observation
	pollDeadline := evidenceAt.Add(presenceWindowS*time.Second + 2*time.Second)
	for time.Now().Before(pollDeadline) {
		row := fetchAgent(t, client, base, presenceAgent)
		seen = append(seen, observation{at: time.Since(evidenceAt), status: row.Status})
		if row.Status == "stale" {
			flip = &seen[len(seen)-1]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	describe := func() string {
		var b strings.Builder
		for _, o := range seen {
			fmt.Fprintf(&b, " +%.2fs=%s", o.at.Seconds(), o.status)
		}
		return b.String()
	}

	if flip == nil {
		t.Fatalf("the dead agent's row never went stale within the %ds window (observations:%s)",
			presenceWindowS, describe())
	}
	// Both live sides of the transition are in hand: the row read `online` while
	// the agent was ALIVE (the read in step 3, before the kill — asserted there),
	// and `flip` is the first `stale` observation of this poll.
	//
	// The poll's own first sample is deliberately NOT required to be online: on
	// a loaded host it can land past the window, and failing there would measure
	// the machine's scheduling instead of the registry. What is asserted is the
	// pair plus the timing band below — a row may not go stale early, and it
	// must go stale inside the window.
	if len(seen) > 1 && seen[0].status == "online" {
		t.Logf("transition observed in one poll sequence: online from +%.2fs up to the first stale sample",
			seen[0].at.Seconds())
	} else {
		t.Logf("the poll's first sample was already %q at +%.2fs (the host delayed the first read past the window); the online side of the transition was the live read before the kill",
			seen[0].status, seen[0].at.Seconds())
	}
	// A row may not be declared stale before its window has passed — that would
	// flap a healthy but quiet agent.
	if flip.at < presenceWindowS*time.Second-100*time.Millisecond {
		t.Errorf("the row went stale %s after its last heartbeat, before the %ds window elapsed:%s",
			flip.at.Round(10*time.Millisecond), presenceWindowS, describe())
	}
	// ...and it must go stale within the window, not eventually.
	if flip.at > presenceWindowS*time.Second+2*time.Second {
		t.Errorf("the row took %s to go stale, past the %ds window:%s",
			flip.at.Round(10*time.Millisecond), presenceWindowS, describe())
	}
	t.Logf("presence transition captured live: heartbeat at t=0, row went stale at %s (window %ds); observations:%s",
		flip.at.Round(10*time.Millisecond), presenceWindowS, describe())

	// (6) The staleness is DERIVED, not manufactured: the stored row still
	// carries the evidence instant it had once the agent was gone, so the
	// message that arrives when the agent comes back refreshes the same row
	// instead of a rewritten one.
	dead := fetchAgent(t, client, base, presenceAgent)
	if !parseLastSeen(t, dead).Equal(parseLastSeen(t, postKill)) {
		t.Errorf("last_seen moved while the agent was dead: %s → %s (a dead agent produces no evidence)",
			postKill.LastSeen, dead.LastSeen)
	}

	// (7) The listing a dashboard actually reads agrees with the detail read.
	listResp, err := client.Get(base + "/agents")
	if err != nil {
		t.Fatalf("GET /agents: %v", err)
	}
	listRaw, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	var list struct {
		Agents []agentRow `json:"agents"`
	}
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decode /agents %s: %v", listRaw, err)
	}
	found := false
	for _, row := range list.Agents {
		if row.ID != presenceAgent {
			continue
		}
		found = true
		if row.Status != "stale" {
			t.Errorf("GET /agents reports %q for a dead agent, want stale (body %s)", row.Status, listRaw)
		}
	}
	if !found {
		t.Fatalf("GET /agents did not list %s: %s", presenceAgent, listRaw)
	}

	// (8) Coming back refreshes the SAME row: a reconnect is liveness evidence,
	// so the dashboard sees online again without any manual intervention.
	conn2, wsResp2, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if wsResp2 != nil {
			status = wsResp2.StatusCode
		}
		t.Fatalf("re-dial %s: %v (status %d)", wsURL, err, status)
	}
	defer conn2.Close()
	awaitPeer(t, client, base, presenceAgent, true)
	revived := fetchAgent(t, client, base, presenceAgent)
	if revived.Status != "online" {
		t.Errorf("a reconnected agent reports %q, want online (the accept is liveness evidence)", revived.Status)
	}
}
