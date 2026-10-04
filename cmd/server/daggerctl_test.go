package main

// End-to-end wiring proof for the dagger control surface (CR-CHAT-033):
// the server registers the six /dagger routes ONLY when CR_DAGGER_URL is set,
// an agent creates a run and reads it back through them, and a terminal run
// lands in that agent's DURABLE INBOX through the shipped delivery path.
//
// The executor is a stub in this test — crier embeds none by design (decision
// D18) — so everything below the HTTP boundary is crier's own machinery.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// daggerStub is a stand-in for the dagger HTTP/JSON surface: it answers the
// adapter's contract and reports the states the test asks for.
func daggerStub(t *testing.T, runID string, status func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"run_id":%q,"status":%q,"evidence":["ckpt/1"]}`, runID, status())
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			_, _ = fmt.Fprintf(w, `{"run_id":%q,"status":"cancelled"}`, runID)
		default:
			_, _ = fmt.Fprintf(w, `{"run_id":%q,"status":"running"}`, runID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// postJSON issues a POST with a JSON body and returns the response with its
// body already read and closed, so no caller has to remember either.
func postJSON(t *testing.T, client *http.Client, url, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// getJSON issues a GET and returns the status with its body read.
func getJSON(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestDaggerControlSurfaceEndToEnd drives create → observe → inbox delivery and
// cancel against a real server boot.
func TestDaggerControlSurfaceEndToEnd(t *testing.T) {
	const (
		agent = "hermes-dagger-e2e"
		runID = "run-e2e-1"
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

	// Register the requesting agent so its inbox exists.
	if code, body := postJSON(t, client, baseURL+"/agents", fmt.Sprintf(`{"id":%q}`, agent)); code != http.StatusCreated {
		t.Fatalf("register agent: status %d, want 201 (body %s)", code, body)
	}

	// CREATE.
	code, body := postJSON(t, client, baseURL+"/dagger/runs",
		fmt.Sprintf(`{"agent_id":%q,"prompt":"build the release"}`, agent))
	if code != http.StatusCreated {
		t.Fatalf("POST /dagger/runs: status %d, want 201 (body %s)", code, body)
	}
	var created daggerctl.RunRecord
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create body: %v (%s)", err, body)
	}
	if created.RunID != runID {
		t.Fatalf("run id = %q, want %q", created.RunID, runID)
	}
	if created.RequestingAgent != agent {
		t.Errorf("requesting agent = %q, want %q", created.RequestingAgent, agent)
	}
	if created.State != daggerctl.StateRunning {
		t.Errorf("state = %q, want running", created.State)
	}

	// OBSERVE a live run: no delivery yet.
	got := getRun(t, client, baseURL, runID)
	if got.State != daggerctl.StateRunning || got.Notified {
		t.Fatalf("live run record = %+v, want running and not notified", got)
	}
	if n := len(inboxMessages(t, client, baseURL, agent)); n != 0 {
		t.Fatalf("a live run put %d message(s) in the inbox, want 0", n)
	}

	// The DAG finishes; the next observation observes it and delivers.
	state = "succeeded"
	got = getRun(t, client, baseURL, runID)
	if got.State != daggerctl.StateSucceeded || !got.Notified {
		t.Fatalf("terminal run record = %+v, want succeeded and notified", got)
	}

	// The outcome is an ordinary inbox message.
	msgs := inboxMessages(t, client, baseURL, agent)
	if len(msgs) != 1 {
		t.Fatalf("inbox holds %d message(s), want exactly 1", len(msgs))
	}
	var note daggerctl.RunNotification
	// The inbox entry's payload is bytes on the wire (Go's []byte marshalling
	// is base64), so decode it the way any inbox client does.
	encoded, _ := msgs[0]["payload"].(string)
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("inbox payload is not base64: %v (%q)", err, encoded)
	}
	if err := json.Unmarshal(payload, &note); err != nil {
		t.Fatalf("decode notification: %v (%s)", err, payload)
	}
	if note.Code != daggerctl.CodeRunSucceeded || note.RunID != runID || note.RequestingAgent != agent {
		t.Errorf("notification = %+v", note)
	}
	if len(note.Evidence) != 1 || note.Evidence[0] != "ckpt/1" {
		t.Errorf("notification evidence = %v, want the executor's reference", note.Evidence)
	}

	// A repeated observation must not deliver a second time. The read above
	// ACKED the message, so anything the repeat delivered would show up here as
	// a NEW message.
	_ = getRun(t, client, baseURL, runID)
	if msgs := inboxMessages(t, client, baseURL, agent); len(msgs) != 0 {
		t.Fatalf("a repeated observation delivered again: %d new message(s)", len(msgs))
	}

	// CANCEL a second run.
	if code, body := postJSON(t, client, baseURL+"/dagger/runs",
		fmt.Sprintf(`{"agent_id":%q,"prompt":"second"}`, agent)); code != http.StatusCreated {
		t.Fatalf("create second run: status %d (body %s)", code, body)
	}
	code, body = postJSON(t, client, baseURL+"/dagger/runs/"+runID+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("POST cancel: status %d, want 200 (body %s)", code, body)
	}
	var cancelled daggerctl.RunRecord
	if err := json.Unmarshal(body, &cancelled); err != nil {
		t.Fatalf("decode cancel body: %v", err)
	}
	if cancelled.State != daggerctl.StateCancelled {
		t.Errorf("state after cancel = %q, want cancelled", cancelled.State)
	}

	// An unknown run is the documented 404.
	if code, _ := getJSON(t, client, baseURL+"/dagger/runs/does-not-exist"); code != http.StatusNotFound {
		t.Errorf("unknown run: status %d, want 404", code)
	}
}

// TestDaggerRoutesAreOptIn pins the additive half of the contract: with
// CR_DAGGER_URL unset the six routes are not registered at all, so a
// deployment that does not control DAGs serves exactly the surface it did
// before the feature existed.
func TestDaggerRoutesAreOptIn(t *testing.T) {
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{}

	for _, path := range []string{"/dagger/runs", "/dagger/runs/anything", "/dagger/skills/s/run"} {
		if code, _ := getJSON(t, client, baseURL+path); code != http.StatusNotFound {
			t.Errorf("GET %s with CR_DAGGER_URL unset: status %d, want 404 (the route must not exist)", path, code)
		}
	}
	// The create verb is unresolvable too, and a POST to an unregistered path
	// must not be mistaken for a successful create.
	if code, _ := postJSON(t, client, baseURL+"/dagger/runs", `{"agent_id":"a","prompt":"p"}`); code == http.StatusCreated {
		t.Fatal("POST /dagger/runs was accepted with CR_DAGGER_URL unset")
	}
}

// ---------- helpers ----------

func getRun(t *testing.T, client *http.Client, baseURL, runID string) daggerctl.RunRecord {
	t.Helper()
	code, body := getJSON(t, client, baseURL+"/dagger/runs/"+runID)
	if code != http.StatusOK {
		t.Fatalf("GET run %s: status %d (body %s)", runID, code, body)
	}
	var rec daggerctl.RunRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decode run %s: %v (%s)", runID, err, body)
	}
	return rec
}

// inboxMessages retrieves and ACKS the agent's inbox, so a later call reads
// what arrived AFTER this one.
func inboxMessages(t *testing.T, client *http.Client, baseURL, agent string) []map[string]any {
	t.Helper()
	code, body := getJSON(t, client, baseURL+"/agents/"+agent+"/inbox")
	if code != http.StatusOK {
		t.Fatalf("retrieve inbox: status %d (body %s)", code, body)
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
		LeaseID  string           `json:"lease_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode inbox: %v (%s)", err, body)
	}
	if len(out.Messages) == 0 || out.LeaseID == "" {
		return out.Messages
	}
	ids := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		if id, ok := m["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	ackBody, _ := json.Marshal(map[string]any{"lease_id": out.LeaseID, "message_ids": ids})
	if code, body := postJSON(t, client, baseURL+"/agents/"+agent+"/inbox/ack", string(ackBody)); code != http.StatusNoContent {
		t.Fatalf("ack inbox: status %d (body %s)", code, body)
	}
	return out.Messages
}
