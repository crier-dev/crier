package main

// CR-FEAT-023 end to end, against the REAL server (run(nil) — middleware,
// router, registry store, mesh and the wired pinger, exactly as cmd/server
// builds them).
//
// The unit-level tests live with the packages (internal/registry and
// internal/mesh); this one exists because the two surfaces are only worth
// anything if the SERVER ships them together:
//
//   - GET /agents/{id}/inbox?wait_seconds= returns early on a delivery and
//     times out empty with the poll-only body, while the poll-only read stays
//     byte-identical to what the endpoint has always answered;
//   - a delivery into an agent that connected to the mesh with
//     ?inbox_notify=1 produces one INBOX_NOTIFY frame on that very socket,
//     naming the message the accept reported.
//
// Signature enforcement is off for this boot (the README dev posture) so the
// agent-scoped retrieve can be driven with plain HTTP; nothing else about the
// wiring is stubbed.

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

// pollOnlyBodyE2E is the frozen body of a poll-only read on an inbox with
// nothing to claim.
const pollOnlyBodyE2E = "{\"messages\":[],\"lease_id\":\"\",\"queue_depth\":0,\"leased_count\":0}\n"

func e2eGet(t *testing.T, client *http.Client, url string) (int, string, time.Duration) {
	t.Helper()
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(raw), time.Since(start)
}

func e2eDeliver(t *testing.T, client *http.Client, url, sender string) string {
	t.Helper()
	body := fmt.Sprintf(`{"payload":{"hello":"world"},"sender":%q}`, sender)
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: status %d, want 201 (body %s)", url, resp.StatusCode, raw)
	}
	var accept struct {
		ID        string `json:"id"`
		Transport string `json:"transport"`
	}
	if err := json.Unmarshal(raw, &accept); err != nil {
		t.Fatalf("decode accept %s: %v", raw, err)
	}
	if accept.Transport != "inbox" {
		t.Fatalf("accept transport = %q, want inbox (body %s)", accept.Transport, raw)
	}
	return accept.ID
}

func TestInboxLongPollEndToEnd(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": "false"})
	client := &http.Client{Timeout: 30 * time.Second}
	registerAgent(t, client, base, "poller")
	registerAgent(t, client, base, "waiter")

	// (1) The poll-only read is unchanged: the frozen body, immediately.
	status, body, elapsed := e2eGet(t, client, base+"/agents/poller/inbox")
	if status != http.StatusOK || body != pollOnlyBodyE2E {
		t.Fatalf("poll-only read: status %d body %q, want 200 %q", status, body, pollOnlyBodyE2E)
	}
	if elapsed >= time.Second {
		t.Errorf("poll-only read took %s — it must never park", elapsed)
	}

	// (2) The long-poll returns EARLY: a delivery 300ms into a 5s budget.
	delivered := make(chan string, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		resp, err := http.Post(base+"/agents/poller/inbox", "application/json",
			strings.NewReader(`{"payload":{"hello":"world"},"sender":"e2e"}`))
		if err != nil {
			delivered <- ""
			return
		}
		defer resp.Body.Close()
		var accept struct {
			ID string `json:"id"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &accept)
		delivered <- accept.ID
	}()

	start := time.Now()
	status, body, _ = e2eGet(t, client, base+"/agents/poller/inbox?wait_seconds=5")
	parkedFor := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("long-poll: status %d (body %s)", status, body)
	}
	if parkedFor >= 2*time.Second {
		t.Errorf("long-poll returned after %s — it must answer as soon as the delivery lands", parkedFor)
	}
	var retrieved struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("decode long-poll body %s: %v", body, err)
	}
	if len(retrieved.Messages) != 1 || retrieved.LeaseID == "" {
		t.Fatalf("long-poll claim = %d message(s) lease %q, want 1 and a lease (body %s)",
			len(retrieved.Messages), retrieved.LeaseID, body)
	}
	if id := <-delivered; id != "" && id != retrieved.Messages[0].ID {
		t.Errorf("long-poll claimed %q, want the delivered id %q", retrieved.Messages[0].ID, id)
	}

	// (3) An empty long-poll times out with the SAME body a poll-only read
	// gives — measured on its own agent, since the batch above is claimed.
	start = time.Now()
	status, body, _ = e2eGet(t, client, base+"/agents/waiter/inbox?wait_seconds=1")
	timedOutFor := time.Since(start)
	if status != http.StatusOK || body != pollOnlyBodyE2E {
		t.Fatalf("timed-out long-poll: status %d body %q, want 200 %q", status, body, pollOnlyBodyE2E)
	}
	if timedOutFor < 900*time.Millisecond || timedOutFor > 3*time.Second {
		t.Errorf("timed-out long-poll held the request for %s, want ~1s", timedOutFor)
	}

	// (4) A rejected budget is answered, not silently ignored.
	status, body, _ = e2eGet(t, client, base+"/agents/waiter/inbox?wait_seconds=999")
	if status != http.StatusBadRequest || !strings.Contains(body, "wait_seconds") {
		t.Fatalf("wait_seconds=999: status %d body %q, want 400 naming the parameter", status, body)
	}
}

func TestInboxPingReachesTheMeshSocketEndToEnd(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": "false"})
	client := &http.Client{Timeout: 30 * time.Second}
	registerAgent(t, client, base, "pinger")

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/pinger?inbox_notify=1"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", wsURL, err, status)
	}
	defer conn.Close()

	// The mesh accepts the peer asynchronously with the upgrade: wait until the
	// server itself reports it, so the delivery below cannot race the connect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/mesh/peers")
		if err != nil {
			t.Fatalf("GET /mesh/peers: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(raw), "pinger") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mesh never listed the connected peer: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}

	messageID := e2eDeliver(t, client, base+"/agents/pinger/inbox", "e2e-sender")

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no INBOX_NOTIFY frame arrived for the delivery: %v", err)
	}
	var frame struct {
		Type           string `json:"type"`
		Version        int    `json:"version"`
		MessageID      string `json:"message_id"`
		AgentID        string `json:"agent_id"`
		InboxMessageID string `json:"inbox_message_id"`
		Sender         string `json:"sender"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode INBOX_NOTIFY %s: %v", data, err)
	}
	if frame.Type != "INBOX_NOTIFY" || frame.Version != 1 {
		t.Errorf("frame = %s, want an INBOX_NOTIFY v1 envelope", data)
	}
	if frame.AgentID != "pinger" {
		t.Errorf("frame agent_id = %q, want pinger", frame.AgentID)
	}
	if frame.InboxMessageID != messageID {
		t.Errorf("frame inbox_message_id = %q, want the accepted id %q", frame.InboxMessageID, messageID)
	}
	if frame.Sender != "e2e-sender" {
		t.Errorf("frame sender = %q, want e2e-sender", frame.Sender)
	}
	if frame.MessageID == "" {
		t.Error("frame carries no envelope message_id")
	}
	if strings.Contains(string(data), "payload") {
		t.Errorf("the ping carried a payload — it must be a tick: %s", data)
	}

	// The message the ping named is retrievable on the durable lane — the ping
	// is a notification about a WRITE, never a substitute for it.
	deadline = time.Now().Add(2 * time.Second)
	for {
		status, body, _ := e2eGet(t, client, base+"/agents/pinger/inbox")
		if status == http.StatusOK && strings.Contains(body, messageID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pinged message is not retrievable: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
