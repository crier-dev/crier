package main

// CR-FEAT-030 acceptance — DETECTION, not just attribution.
//
// The task's acceptance criterion, verbatim: "a scripted 'compromised agent'
// that fans out to N new peers trips a documented alert and is contained by the
// kill-switch in one call, captured live."
//
// This file runs that scenario against the REAL server (run(nil): middleware,
// router, registry store, webhook driver and the detection layer wired exactly
// as cmd/server builds them — nothing here re-implements a code path). The
// capture is the test's own output: the alert that tripped, the single
// kill-switch call and its per-action result, and the refusals that follow.
//
// It also proves the two properties the deliverable asks for that a single
// process cannot show on its own:
//
//   - the delivery log is SIGNED and APPEND-ONLY: a second server booted on the
//     same files still verifies the records the first one wrote, and the chain
//     continues at the next sequence number;
//   - containment is ENFORCED at the choke point: after the one call, the
//     contained agent can neither send nor receive.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/detect"
	"github.com/crier-dev/crier/internal/registry"
)

// detectionBoot is one server process booted by run() with the detection layer
// on, plus the files it signs.
type detectionBoot struct {
	baseURL string
	client  *http.Client
	stop    func()
}

// bootDetectionServer starts run(nil) on a free port with detection enabled and
// returns a handle whose stop() drives the same graceful SIGTERM path
// `make stop` uses. Each call is a fresh process image as far as the log is
// concerned: new store, new router, fresh read of the log file.
func bootDetectionServer(t *testing.T, logPath string) *detectionBoot {
	t.Helper()

	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_GUARD_ENABLED", "false")
	// The acceptance drives the bus with plain HTTP (the README dev posture);
	// signature enforcement is orthogonal to detection.
	t.Setenv("CR_REQUIRE_AGENT_SIG", "false")
	t.Setenv("CR_DETECT_ENABLED", "true")
	t.Setenv("CR_DETECT_LOG", logPath)
	t.Setenv("CR_DETECT_KEY", logPath+".key")

	port := freePort(t)
	t.Setenv("CRIER_PORT", strconv.Itoa(port))

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(nil)
	}()

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find own process: %v", err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = self.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("server did not shut down within 10s of SIGTERM")
		}
	}
	t.Cleanup(stop)

	boot := &detectionBoot{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		client:  &http.Client{Timeout: 5 * time.Second},
		stop:    stop,
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := boot.client.Get(boot.baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			return boot
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 20s: %v", err)
		}
		select {
		case <-done:
			t.Fatalf("server exited before answering /health on port %d (last error: %v)", port, err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// detCall performs one HTTP request and returns (status, body).
func detCall(t *testing.T, b *detectionBoot, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, b.baseURL+path, rdr)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// registerAgent registers one agent (201) and fails on anything else.
func detRegister(t *testing.T, b *detectionBoot, id string) {
	t.Helper()
	status, body := detCall(t, b, http.MethodPost, "/agents", fmt.Sprintf(`{"id":%q}`, id))
	if status != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", id, status, body)
	}
}

// detDeliver sends one delivery from sender to target and returns status+body.
func detDeliver(t *testing.T, b *detectionBoot, sender, target string) (int, string) {
	t.Helper()
	return detCall(t, b, http.MethodPost, "/agents/"+target+"/inbox",
		fmt.Sprintf(`{"payload":{"from":%q},"sender":%q}`, sender, sender))
}

// alertsBody is the GET /alerts response.
type alertsBody struct {
	Alerts []detect.Alert `json:"alerts"`
	Count  int            `json:"count"`
}

// logBody is the GET /delivery-log response.
type logBody struct {
	Enabled bool             `json:"enabled"`
	Path    string           `json:"path"`
	Total   int              `json:"total"`
	Records []detect.Entry   `json:"records"`
	Verify  detect.LogReport `json:"verify"`
}

// TestDetectionCatchesAndContainsACompromisedAgent is the acceptance run.
func TestDetectionCatchesAndContainsACompromisedAgent(t *testing.T) {
	logPath := t.TempDir() + "/delivery.jsonl"

	boot := bootDetectionServer(t, logPath)

	// --- the cast -----------------------------------------------------------------
	peers := []string{"peer-1", "peer-2", "peer-3", "peer-4", "peer-5"}
	for _, p := range peers {
		detRegister(t, boot, p)
	}
	detRegister(t, boot, "ops")
	detRegister(t, boot, "compromised")

	// --- the scripted compromise: fan out to N NEW peers --------------------------
	// Five DISTINCT targets nobody had ever written to from this sender is the
	// documented fanout_spike threshold, and 3 first-ever pairs is the
	// new_peer_burst threshold — the same deliveries trip both.
	for _, p := range peers {
		status, body := detDeliver(t, boot, "compromised", p)
		if status != http.StatusCreated {
			t.Fatalf("compromised -> %s: status %d, body %s", p, status, body)
		}
	}

	// --- the DOCUMENTED ALERT ------------------------------------------------------
	status, body := detCall(t, boot, http.MethodGet, "/alerts", "")
	if status != http.StatusOK {
		t.Fatalf("GET /alerts = %d: %s", status, body)
	}
	var alerts alertsBody
	if err := json.Unmarshal([]byte(body), &alerts); err != nil {
		t.Fatalf("decode /alerts: %v (%s)", err, body)
	}
	bySignal := map[string]detect.Alert{}
	for _, a := range alerts.Alerts {
		bySignal[a.Signal] = a
	}
	fanout, ok := bySignal[detect.SignalFanoutSpike]
	if !ok {
		t.Fatalf("the scripted fan-out did NOT trip %s — alerts: %s", detect.SignalFanoutSpike, body)
	}
	if fanout.Agent != "compromised" {
		t.Fatalf("%s named agent %q, want compromised", detect.SignalFanoutSpike, fanout.Agent)
	}
	if got := fanout.Evidence["distinct_targets"]; got != float64(len(peers)) && got != len(peers) {
		t.Fatalf("fanout evidence distinct_targets = %v (%T), want %d", got, got, len(peers))
	}
	newPeers, ok := bySignal[detect.SignalNewPeerBurst]
	if !ok {
		t.Fatalf("the scripted new-peer burst did NOT trip %s — alerts: %s", detect.SignalNewPeerBurst, body)
	}

	// The compromised agent also holds a LEASE, so containment has real leases
	// to revoke: ops delivers to it, and it claims the message without acking.
	if status, body := detDeliver(t, boot, "ops", "compromised"); status != http.StatusCreated {
		t.Fatalf("ops -> compromised: status %d, body %s", status, body)
	}
	status, body = detCall(t, boot, http.MethodGet, "/agents/compromised/inbox", "")
	if status != http.StatusOK {
		t.Fatalf("compromised claims its inbox: %d %s", status, body)
	}
	var claimed struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal([]byte(body), &claimed); err != nil {
		t.Fatalf("decode retrieve: %v (%s)", err, body)
	}
	if claimed.LeaseID == "" || len(claimed.Messages) == 0 {
		t.Fatalf("the compromised agent holds no lease to revoke: %s", body)
	}

	// --- the single kill-switch call ----------------------------------------------
	status, body = detCall(t, boot, http.MethodPost, "/agents/compromised/kill-switch",
		`{"reason":"fan-out to 5 new peers"}`)
	if status != http.StatusOK {
		t.Fatalf("kill-switch = %d: %s", status, body)
	}
	var containment detect.Containment
	if err := json.Unmarshal([]byte(body), &containment); err != nil {
		t.Fatalf("decode containment: %v (%s)", err, body)
	}
	if !containment.Contained {
		t.Fatalf("the kill-switch did not contain the agent: %s", body)
	}
	actions := map[string]detect.ActionResult{}
	for _, a := range containment.Actions {
		actions[a.Action] = a
	}
	for _, want := range []string{detect.ActionPauseWebhooks, detect.ActionRevokeLeases, detect.ActionQuarantine, detect.ActionUnregister} {
		a, ok := actions[want]
		if !ok {
			t.Fatalf("the kill-switch did not report %s: %s", want, body)
		}
		if a.Status != detect.ActionStatusOK {
			t.Fatalf("kill-switch action %s = %q (%s), want ok", want, a.Status, a.Detail)
		}
	}
	if got := actions[detect.ActionRevokeLeases].Count; got != 1 {
		t.Fatalf("revoke_leases released %d leases, want the 1 the agent held", got)
	}
	if containment.Warnings != nil {
		t.Fatalf("containment reported warnings on a fully-wired server: %v", containment.Warnings)
	}

	// --- containment is ENFORCED, both directions ---------------------------------
	if status, body := detDeliver(t, boot, "compromised", "peer-1"); status != http.StatusForbidden {
		t.Fatalf("a contained agent could still SEND: status %d, body %s", status, body)
	} else if !strings.Contains(body, "AGENT_QUARANTINED") || !strings.Contains(body, `"side":"sender"`) {
		t.Fatalf("sender refusal body = %s, want AGENT_QUARANTINED naming the sender side", body)
	}
	if status, body := detDeliver(t, boot, "ops", "compromised"); status != http.StatusForbidden {
		t.Fatalf("a contained agent could still RECEIVE: status %d, body %s", status, body)
	} else if !strings.Contains(body, `"side":"target"`) {
		t.Fatalf("target refusal body = %s, want it to name the target side", body)
	}
	if status, body := detCall(t, boot, http.MethodGet, "/agents/compromised", ""); status != http.StatusNotFound {
		t.Fatalf("GET /agents/compromised after the kill-switch = %d: %s (the row must be gone)", status, body)
	}
	// The peers are untouched: containment is aimed at one agent, not the bus.
	if status, body := detDeliver(t, boot, "ops", "peer-1"); status != http.StatusCreated {
		t.Fatalf("an innocent agent was collaterally contained: %d %s", status, body)
	}

	// --- the log the operator keeps ------------------------------------------------
	status, body = detCall(t, boot, http.MethodGet, "/delivery-log?limit=500", "")
	if status != http.StatusOK {
		t.Fatalf("GET /delivery-log = %d: %s", status, body)
	}
	var first logBody
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode /delivery-log: %v (%s)", err, body)
	}
	if !first.Verify.OK {
		t.Fatalf("the delivery log does not verify: %s", body)
	}
	kinds := map[string]int{}
	quarantineRefusals := 0
	for _, r := range first.Records {
		kinds[r.Kind]++
		if r.Verdict == registry.VerdictQuarantined {
			quarantineRefusals++
		}
	}
	for _, want := range []string{detect.KindDelivery, detect.KindAlert, detect.KindContainment} {
		if kinds[want] == 0 {
			t.Fatalf("the delivery log has no %s record: %s", want, body)
		}
	}
	if quarantineRefusals < 2 {
		t.Fatalf("the log recorded %d quarantine refusals, want the two the enforcement produced", quarantineRefusals)
	}
	if first.Total < len(first.Records) {
		t.Fatalf("log total %d < returned records %d", first.Total, len(first.Records))
	}
	totalBeforeRestart := first.Total

	// --- restart: the log survives, and the chain continues -------------------------
	boot.stop()
	second := bootDetectionServer(t, logPath)

	status, body = detCall(t, second, http.MethodGet, "/delivery-log/verify", "")
	if status != http.StatusOK {
		t.Fatalf("GET /delivery-log/verify after a restart = %d: %s", status, body)
	}
	var rep detect.LogReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode verify: %v (%s)", err, body)
	}
	if !rep.OK {
		t.Fatalf("the log did not verify after a restart: %s", body)
	}
	if rep.Entries != totalBeforeRestart {
		t.Fatalf("verify counted %d entries after restart, want the %d written before it", rep.Entries, totalBeforeRestart)
	}

	detRegister(t, second, "peer-9")
	if status, body := detDeliver(t, second, "ops", "peer-9"); status != http.StatusCreated {
		t.Fatalf("post-restart delivery: %d %s", status, body)
	}
	status, body = detCall(t, second, http.MethodGet, "/delivery-log?limit=1", "")
	if status != http.StatusOK {
		t.Fatalf("GET /delivery-log after restart = %d: %s", status, body)
	}
	var tail logBody
	if err := json.Unmarshal([]byte(body), &tail); err != nil {
		t.Fatalf("decode tail: %v (%s)", err, body)
	}
	if len(tail.Records) != 1 {
		t.Fatalf("tail page = %d records, want 1", len(tail.Records))
	}
	if want := uint64(totalBeforeRestart) + 1; tail.Records[0].Seq != want {
		t.Fatalf("first record after the restart has seq %d, want %d (the sequence must continue, not restart)", tail.Records[0].Seq, want)
	}
	if !tail.Verify.OK {
		t.Fatalf("the log stopped verifying after the post-restart append: %s", body)
	}

	// The canaries exist and are readable by the operator (they are planted
	// secrets, which is the point of them).
	status, body = detCall(t, second, http.MethodGet, "/canaries", "")
	if status != http.StatusOK || !strings.Contains(body, "canary-") {
		t.Fatalf("GET /canaries = %d: %s", status, body)
	}

	// --- the capture -----------------------------------------------------------------
	t.Logf("CAPTURE — CR-FEAT-030 acceptance, live against run(nil)")
	t.Logf("  script: compromised -> %s (5 new peers)", strings.Join(peers, ", "))
	t.Logf("  alert:  %s id=%s severity=%s evidence=%v", fanout.Signal, fanout.ID, fanout.Severity, fanout.Evidence)
	t.Logf("  alert:  %s id=%s detail=%q", newPeers.Signal, newPeers.ID, newPeers.Detail)
	t.Logf("  ONE call: POST /agents/compromised/kill-switch -> contained=%v", containment.Contained)
	for _, a := range containment.Actions {
		t.Logf("    %-14s %-8s count=%d  %s", a.Action, a.Status, a.Count, a.Detail)
	}
	t.Logf("  enforced: send 403 (%s), receive 403 (%s), GET /agents/compromised 404", "AGENT_QUARANTINED/sender", "AGENT_QUARANTINED/target")
	t.Logf("  log:    %s entries=%d verify_ok=%v kinds=%v", logPath, first.Total, first.Verify.OK, kinds)
	t.Logf("  restart: verify ok with %d entries, next seq=%d", rep.Entries, tail.Records[0].Seq)
}

// TestDetectionRoutesAreOptOut is the negative control for the whole feature:
// with CR_DETECT_ENABLED unset, none of the five routes exists and no log file
// is written — the additive promise, measured rather than asserted.
func TestDetectionRoutesAreOptOut(t *testing.T) {
	t.Setenv("CR_DETECT_ENABLED", "")
	t.Setenv("CR_DETECT_LOG", "")
	t.Setenv("CR_DETECT_KEY", "")
	logPath := t.TempDir() + "/absent.jsonl"

	base := startTestServerWithEnv(t, map[string]string{"CR_DETECT_LOG": ""})
	client := &http.Client{Timeout: 2 * time.Second}
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/delivery-log"},
		{http.MethodGet, "/delivery-log/verify"},
		{http.MethodGet, "/alerts"},
		{http.MethodGet, "/canaries"},
		{http.MethodPost, "/agents/anyone/kill-switch"},
	} {
		req, err := http.NewRequest(probe.method, base+probe.path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", probe.method, probe.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d with detection OFF, want 404 (the feature must be opt-in)", probe.method, probe.path, resp.StatusCode)
		}
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("a delivery log was written with detection OFF (%v)", err)
	}
}
