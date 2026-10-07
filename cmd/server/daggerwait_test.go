package main

// CR-CHAT-034 wiring proof at the HTTP surface: the three delivery-wait
// routes are registered ONLY when CR_DAGGER_URL is set, a wait delivers
// through the durable inbox, an inbox ACK resolves it end to end, and an
// unbounded budget is refused with the named error.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
)

func TestDaggerDeliveryWaitEndToEnd(t *testing.T) {
	const (
		agent = "wait-e2e-agent"
		runID = "run-wait-e2e"
	)
	state := "running"
	stub := daggerStub(t, runID, func() string { return state })

	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_DAGGER_URL":        stub.URL,
		"CR_DAGGER_STORE_DIR":  t.TempDir(),
		"CR_DAGGER_POLL_S":     "0",
	})
	client := &http.Client{}

	if code, body := postJSON(t, client, baseURL+"/agents", fmt.Sprintf(`{"id":%q}`, agent)); code != http.StatusCreated {
		t.Fatalf("register agent: status %d (body %s)", code, body)
	}

	// Start the wait in a goroutine — the request BLOCKS until the ack.
	var wg sync.WaitGroup
	codes := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, body := postJSON(t, client, baseURL+"/dagger/wait",
			fmt.Sprintf(`{"agent_id":%q,"payload":{"node":"n1"},"idempotency_key":"e2e-key","timeout_seconds":15,"run_id":%q}`, agent, runID))
		if code != http.StatusOK {
			t.Errorf("wait answered %d, want 200 (body %s)", code, body)
		}
		codes <- code
	}()

	// Wait for the delivery to become claimable, then ACK it — the ordinary
	// inbox path, which is the resolution edge.
	deadline := time.Now().Add(10 * time.Second)
	var leaseID string
	var ids []string
	for leaseID == "" {
		if time.Now().After(deadline) {
			t.Fatal("the wait's delivery never became claimable")
		}
		resp, err := client.Get(baseURL + "/agents/" + agent + "/inbox?lease_seconds=30&limit=10")
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		var decoded struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			LeaseID string `json:"lease_id"`
		}
		err = json.NewDecoder(resp.Body).Decode(&decoded)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("decode retrieve: %v", err)
		}
		if decoded.LeaseID != "" && len(decoded.Messages) > 0 {
			leaseID = decoded.LeaseID
			for _, m := range decoded.Messages {
				ids = append(ids, m.ID)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ackBody := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, leaseID, ids[0])
	if code, body := postJSON(t, client, baseURL+"/agents/"+agent+"/inbox/ack", ackBody); code != http.StatusNoContent {
		t.Fatalf("ack: status %d (body %s)", code, body)
	}

	wg.Wait()
	if code := <-codes; code != http.StatusOK {
		t.Fatalf("wait final code = %d, want 200 after ack", code)
	}

	// Idempotency over HTTP: the SAME key joins the resolved wait and
	// delivers nothing new — the resolution is returned immediately.
	code, body := postJSON(t, client, baseURL+"/dagger/wait",
		fmt.Sprintf(`{"agent_id":%q,"payload":{"node":"n1"},"idempotency_key":"e2e-key","timeout_seconds":5}`, agent))
	if code != http.StatusOK {
		t.Fatalf("replayed wait answered %d, want 200 (body %s)", code, body)
	}
	var res daggerctl.WaitResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode replay: %v (%s)", err, body)
	}
	if res.State != "resolved" {
		t.Fatalf("replayed wait state = %q, want resolved", res.State)
	}

	// The timeout case: a fresh key with a 1s budget answers the NAMED error.
	code, body = postJSON(t, client, baseURL+"/dagger/wait",
		fmt.Sprintf(`{"agent_id":%q,"payload":{"node":"n2"},"idempotency_key":"e2e-timeout","timeout_seconds":1}`, agent))
	if code != http.StatusGatewayTimeout {
		t.Fatalf("timeout wait answered %d, want 504 (body %s)", code, body)
	}
	if !strings.Contains(string(body), daggerctl.CodeDeliveryWaitTimeout) {
		t.Fatalf("timeout body = %s, want the named %s", body, daggerctl.CodeDeliveryWaitTimeout)
	}

	// An unbounded budget is refused with a named 400, before anything runs.
	code, body = postJSON(t, client, baseURL+"/dagger/wait",
		fmt.Sprintf(`{"agent_id":%q,"payload":{},"idempotency_key":"e2e-unbounded","timeout_seconds":0}`, agent))
	if code != http.StatusBadRequest {
		t.Fatalf("unbounded wait answered %d, want 400 (body %s)", code, body)
	}
}
